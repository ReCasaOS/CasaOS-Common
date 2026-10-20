package stats

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// retention is how long a forgotten or silent item, and a *.ring.bad, are kept.
const retention = 7 * 24 * time.Hour

var errGone = errors.New("stats: item removed")

// Store keeps, per item, the last 15 minutes in memory (5 s) and 7 days of
// minutes on disk (one ring file per item).
//
// Lock order: Store.mu, then entry.mu, then ring.mu. Query holds Store.mu
// (read) only for the lookup, then ring.cmu (read) while it reads the file;
// ring.Close takes ring.cmu, so a ring is closed, and only then removed, once
// no query reads it.
type Store struct {
	dir   string
	clock Clock
	fs    fsys

	mu          sync.RWMutex // guards the fields below
	items       map[string]*entry
	quarantined []string
	skipped     []string
	refused     map[string]bool // unreadable (not corrupt) ring files: never overwritten
	onMinute    func(item string, minute time.Time)
	closed      bool
}

type entry struct {
	ring *ring // set before the entry is in the map, never changed
	live *liveRing

	mu      sync.Mutex // per-item writer lock, held during I/O
	closed  bool
	gone    bool      // removed by Sweep or Forget; a Record looks the item up again
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
				if fs.Rename(p, p+".bad") != nil {
					s.skipped = append(s.skipped, name) // left in place; a Record of its item recreates it
					continue
				}
				// The age of a *.bad counts from now, not from the old mtime.
				_ = fs.Chtimes(p+".bad", clock.Now())
				s.quarantined = append(s.quarantined, name)
			case err != nil:
				// Unreadable (read-only, dangling or directory symlink, I/O error):
				// one bad file must not take every other item down. The file may
				// hold good history behind a temporary error, so it is never replaced.
				s.skipped = append(s.skipped, name)
				if s.refused == nil {
					s.refused = map[string]bool{}
				}
				s.refused[name] = true
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

// Skipped returns the ring files Open did not load: those it could not read
// for a reason other than corruption (permissions, a dangling symlink, an I/O
// error), which stay untouched (Record of their item returns ErrRingSkipped
// until a later Open reads them), and corrupt ones it could not move to *.bad,
// which a Record of their item replaces. Their items are absent from the store.
func (s *Store) Skipped() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.skipped)
}

// SetOnMinute sets the hook called once per item per committed minute.
func (s *Store) SetOnMinute(fn func(item string, minute time.Time)) {
	s.mu.Lock()
	s.onMinute = fn
	s.mu.Unlock()
}

// minuteValue returns the average of key in the given unix minute of item;
// ok is false when that minute does not hold the series.
func (s *Store) minuteValue(item, key string, minute int64) (avg float64, ok bool, err error) {
	s.mu.RLock()
	e := s.items[item]
	s.mu.RUnlock()
	if e == nil {
		return 0, false, nil
	}
	cells, err := e.ring.readWindow(minute, minute, key)
	if err != nil {
		return 0, false, err
	}
	if c := cells[key]; len(c) == 1 && c[0].OK {
		return c[0].Avg, true, nil
	}
	return 0, false, nil
}

// recentMinutes returns the newest committed minute of item (0: none) and
// the cells of the n minutes ending there.
func (s *Store) recentMinutes(item string, n int64) (newest int64, cells map[string][]minuteCell, err error) {
	s.mu.RLock()
	e := s.items[item]
	s.mu.RUnlock()
	if e == nil {
		return 0, nil, nil
	}
	if newest, err = e.ring.newestMinute(); err != nil || newest < n {
		return 0, nil, err
	}
	cells, err = e.ring.readWindow(newest-n+1, newest)
	return newest, cells, err
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
	if base := filepath.Base(ringPath(s.dir, name)); s.refused[base] {
		return nil, fmt.Errorf("%w: %s", ErrRingSkipped, base)
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
	if len(item) < 1 || len(item) > MaxItemLen || !utf8.ValidString(item) {
		return ErrBadItem
	}
	if at.Unix() < 0 {
		return nil // a pre-1970 clock has nothing worth storing
	}
	var e *entry
	var minute int64
	var committed bool
	var err error
	for {
		if e, err = s.entry(item); err != nil {
			return err
		}
		e.mu.Lock()
		minute, committed, err = e.record(at, smp)
		e.mu.Unlock()
		if !errors.Is(err, errGone) {
			break
		}
	}
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
	if e.gone {
		return 0, false, errGone
	}
	if e.closed {
		return 0, false, ErrClosed
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

// Forget marks item as gone at the given time: its history stays in Items and
// Query until Sweep removes it 7 days later; a Record revives it. Beyond
// MaxForgotten, the items forgotten longest ago are removed at once.
func (s *Store) Forget(item string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	e := s.items[item]
	if e == nil {
		return ErrUnknownItem
	}
	// ring.mu orders this header write with a commit in flight.
	if err := e.ring.setForgotten(max(at.Unix(), 1)); err != nil { // 0 would mean alive
		return err
	}
	var old []string
	for k, x := range s.items {
		if x.ring.ForgottenAt() != 0 {
			old = append(old, k)
		}
	}
	if len(old) <= MaxForgotten {
		return nil
	}
	slices.SortFunc(old, func(a, b string) int {
		return cmp.Or(cmp.Compare(s.items[a].ring.ForgottenAt(), s.items[b].ring.ForgottenAt()), strings.Compare(a, b))
	})
	var errs []error
	for _, k := range old[:len(old)-MaxForgotten] {
		errs = append(errs, s.remove(k))
	}
	return errors.Join(errs...)
}

// Sweep removes every item forgotten, or without data, for 7 days or more,
// every *.ring.tmp and every *.ring.bad older than 7 days. No other file of
// the directory is touched.
// ponytail: it holds the store lock for the whole pass, reading each stamp
// column, waiting on each entry lock (a commit in flight) and, for a removed
// ring, on the queries still reading it; Sweep runs hourly and 256 x 40 KiB is
// quick, a finer lock is not worth it.
func (s *Store) Sweep(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	cut := now.Add(-retention).Unix()
	var errs []error
	for k, e := range s.items {
		e.mu.Lock()
		newest, err := e.ring.newestMinute()
		newest *= 60
		if e.hasLast {
			newest = max(newest, e.last.Unix())
		}
		fa := e.ring.ForgottenAt()
		e.mu.Unlock()
		switch {
		case err != nil:
			errs = append(errs, err)
		case (fa != 0 && fa <= cut) || newest <= cut:
			errs = append(errs, s.remove(k))
		}
	}
	ents, err := s.fs.ReadDir(s.dir)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, d := range ents {
		name := d.Name()
		switch {
		case d.IsDir():
		case strings.HasSuffix(name, ".ring.tmp"):
			errs = append(errs, s.fs.Remove(filepath.Join(s.dir, name)))
		case strings.HasSuffix(name, ".ring.bad"):
			info, err := d.Info()
			switch {
			case err != nil:
				errs = append(errs, err)
			case info.ModTime().Unix() <= cut:
				errs = append(errs, s.fs.Remove(filepath.Join(s.dir, name)))
			}
		}
	}
	return errors.Join(errs...)
}

// remove closes the ring of item, then deletes its file (Windows cannot delete
// an open file). Called with s.mu held. If the delete fails, the file comes
// back at the next Open and is swept then.
func (s *Store) remove(item string) error {
	e := s.items[item]
	delete(s.items, item)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.gone = true
	var err error
	if !e.closed {
		e.closed = true
		err = e.ring.Close()
	}
	return errors.Join(err, s.fs.Remove(ringPath(s.dir, item)))
}
