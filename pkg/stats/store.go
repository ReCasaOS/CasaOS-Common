package stats

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Store keeps, per item, the last 15 minutes in memory (5 s) and 7 days of
// minutes on disk (one ring file per item).
type Store struct {
	dir   string
	clock Clock
	fs    fsys

	mu          sync.Mutex // guards the fields below
	items       map[string]*entry
	quarantined []string
	onMinute    func(item string, minute time.Time)
	closed      bool
}

type entry struct {
	ring *ring // set before the entry is in the map, never changed
	live *liveRing

	mu      sync.Mutex // per-item writer lock, held during I/O
	closed  bool
	last    time.Time // last accepted sample
	hasLast bool
	accMin  int64           // minute of acc
	acc     map[string]*agg // pending minute; nil = none
}

type agg struct {
	sum, min, max float64
	n             int
}

// Open opens (creating it if needed) the store kept in dir.
func Open(dir string, clock Clock) (*Store, error) { return open(dir, clock, osFS) }

func open(dir string, clock Clock, fs fsys) (*Store, error) {
	if err := fs.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ents, err := fs.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, clock: clock, fs: fs, items: map[string]*entry{}}
	for _, e := range ents {
		name := e.Name()
		p := filepath.Join(dir, name)
		switch {
		case e.IsDir():
		case strings.HasSuffix(name, ".ring.tmp"):
			_ = fs.Remove(p) // a leftover of an interrupted create; best effort
		case strings.HasSuffix(name, ".ring"):
			r, err := openRing(fs, p)
			if err == nil && filepath.Base(ringPath(dir, r.item)) != name {
				_ = r.Close()
				err = fmt.Errorf("%w: %s holds item %q", errCorrupt, name, r.item)
			}
			switch {
			case errors.Is(err, errCorrupt) || errors.Is(err, errVersion):
				_ = fs.Remove(p + ".bad")
				if err := fs.Rename(p, p+".bad"); err != nil {
					_ = s.closeAll()
					return nil, err
				}
				s.quarantined = append(s.quarantined, name)
			case err != nil:
				_ = s.closeAll()
				return nil, err
			default:
				s.items[r.item] = &entry{ring: r, live: newLiveRing()}
			}
		}
	}
	return s, nil
}

func (s *Store) closeAll() error {
	var errs []error
	for _, e := range s.items {
		e.mu.Lock()
		if !e.closed {
			e.closed = true
			errs = append(errs, e.ring.Close())
		}
		e.mu.Unlock()
	}
	return errors.Join(errs...)
}

// Close closes every ring file; later calls return ErrClosed.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.closed = true
	return s.closeAll()
}

// Items returns every item with history, forgotten ones included, sorted.
func (s *Store) Items() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.items))
	for k := range s.items {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Quarantined returns the ring files moved aside to *.bad at Open.
func (s *Store) Quarantined() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.quarantined)
}

// SetOnMinute sets the hook called once per item per committed minute.
func (s *Store) SetOnMinute(fn func(item string, minute time.Time)) {
	s.mu.Lock()
	s.onMinute = fn
	s.mu.Unlock()
}

// entry returns the live entry of name, creating or reviving it.
// ponytail: create and revive do their I/O under the store lock; it happens
// once per item lifetime, a finer lock is not worth it.
func (s *Store) entry(name string) (*entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	e := s.items[name]
	if e != nil && e.ring.ForgottenAt() == 0 {
		return e, nil
	}
	live := 0
	for _, x := range s.items {
		if x.ring.ForgottenAt() == 0 {
			live++
		}
	}
	if live >= MaxItems {
		return nil, ErrTooManyItems
	}
	if e != nil {
		return e, e.ring.setForgotten(0)
	}
	r, err := createRing(s.fs, ringPath(s.dir, name), name)
	if err != nil {
		return nil, err
	}
	e = &entry{ring: r, live: newLiveRing()}
	s.items[name] = e
	return e, nil
}

// Record adds one sample of item taken at the given time.
func (s *Store) Record(item string, at time.Time, smp Sample) error {
	if len(item) < 1 || len(item) > MaxItemLen {
		return ErrBadItem
	}
	e, err := s.entry(item)
	if err != nil {
		return err
	}
	e.mu.Lock()
	minute, committed, err := e.record(at, smp)
	e.mu.Unlock()
	if committed {
		s.mu.Lock()
		fn := s.onMinute
		s.mu.Unlock()
		if fn != nil {
			fn(item, time.Unix(minute*60, 0).UTC())
		}
	}
	return err
}

// record returns the minute it committed, if any. Called with e.mu held.
func (e *entry) record(at time.Time, smp Sample) (int64, bool, error) {
	if e.closed {
		return 0, false, ErrClosed
	}
	if at.Unix() < 0 {
		return 0, false, nil // a pre-1970 clock has nothing worth storing
	}
	if e.hasLast && at.Before(e.last) {
		if e.last.Sub(at) <= time.Minute {
			return 0, false, ErrOutOfOrder
		}
		// The clock stepped back: what is pending or live belongs to the old time.
		e.acc = nil
		e.live.reset()
	}
	e.last, e.hasLast = at, true

	clean, rejected := e.clean(smp)
	e.live.put(at, clean)

	var minute int64
	var committed bool
	var cerr error
	m := at.Unix() / 60
	if e.acc != nil && m > e.accMin {
		if cerr = e.commit(); cerr == nil {
			minute, committed = e.accMin, true
		}
		e.acc = nil
	}
	if e.acc == nil {
		e.acc, e.accMin = map[string]*agg{}, m
	}
	for k, v := range clean {
		a := e.acc[k]
		if a == nil {
			e.acc[k] = &agg{v, v, v, 1}
			continue
		}
		a.sum += v
		a.min = min(a.min, v)
		a.max = max(a.max, v)
		a.n++
	}
	return minute, committed, errors.Join(cerr, rejected)
}

// clean drops values that are not finite float32 numbers (absent, never 0)
// and keys that are too long or would exceed MaxSeries.
func (e *entry) clean(smp Sample) (Sample, error) {
	known := e.ring.series()
	n := len(known)
	for k := range e.acc {
		if !slices.Contains(known, k) {
			n++
		}
	}
	keys := make([]string, 0, len(smp))
	for k := range smp {
		keys = append(keys, k)
	}
	slices.Sort(keys) // which keys get cut at the cap must not be random
	out := make(Sample, len(smp))
	var errs []error
	for _, k := range keys {
		v := smp[k]
		if len(k) < 1 || len(k) > MaxKeyLen {
			errs = append(errs, fmt.Errorf("%w: %q", ErrBadKey, k))
			continue
		}
		if math.IsNaN(v) || math.Abs(v) > math.MaxFloat32 {
			continue
		}
		if e.acc[k] == nil && !slices.Contains(known, k) {
			if n >= MaxSeries {
				errs = append(errs, fmt.Errorf("%w: %q", ErrSeriesCap, k))
				continue
			}
			n++
		}
		out[k] = v
	}
	return out, errors.Join(errs...)
}

// commit writes the pending minute: avg over the samples where each series was present.
func (e *entry) commit() error {
	keys := make([]string, 0, len(e.acc))
	for k := range e.acc {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	vals := make(map[int]minuteValue, len(keys))
	for _, k := range keys {
		i, err := e.ring.addSeries(k)
		if err != nil {
			return err
		}
		a := e.acc[k]
		vals[i] = minuteValue{a.sum / float64(a.n), a.min, a.max}
	}
	return e.ring.writeMinute(e.accMin, vals)
}
