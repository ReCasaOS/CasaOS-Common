package stats

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Event is one alert transition; State is "firing" or "resolved".
type Event struct {
	Item, Key string
	State     string
	Value     float64
	Since     time.Time
}

const stateVersion = 1

type stateFile struct {
	Version int                   `json:"version"`
	Items   map[string]*itemState `json:"items"`
}

// itemState is what differs from defaults and idle for one item.
type itemState struct {
	Rules  []Rule         `json:"rules,omitempty"`
	Active []activeAlert  `json:"active,omitempty"`
	Runs   map[string]int `json:"runs,omitempty"` // consecutive high minutes per key
	Last   int64          `json:"last,omitempty"` // unix minute last evaluated, kept while a run counts
}

type activeAlert struct {
	Key   string  `json:"key"`
	Since int64   `json:"since"` // unix seconds
	Value float64 `json:"value"`
}

// Evaluator turns the alert/* series of each committed minute into firing and
// resolved events. Its settings and state live in <dir>/alerts.json.
type Evaluator struct {
	store *Store
	fs    fsys
	path  string
	emit  func(Event)

	mu       sync.Mutex // guards the fields below
	items    map[string]*itemState
	saved    []byte  // content of the file on disk
	saveErr  error   // error of the last write, nil once the file matches
	pending  []Event // made by state changes, not yet emitted, in order
	draining bool    // a call is emitting pending
}

// maxRun caps a run at the longest rule window: a longer run counts the same.
const maxRun = 120

// NewEvaluator loads <dir>/alerts.json. A missing file means defaults; an
// unreadable, invalid or other-version file is moved to alerts.json.bad and
// defaults are used. emit is called after the state is saved, outside every lock,
// in the order of the state changes. Resolve, SetRules, Rebuild and OnMinute
// (so Record) can return before their own events are emitted: when another
// goroutine is emitting the queue, or after a panic in emit until a later
// call (not Prune) drains the leftovers. There is no flush at shutdown; a
// caller that needs its events delivered waits for them itself.
func NewEvaluator(store *Store, dir string, emit func(Event)) (*Evaluator, error) {
	return newEvaluator(store, dir, emit, osFS)
}

func newEvaluator(store *Store, dir string, emit func(Event), fs fsys) (*Evaluator, error) {
	if err := mkdirPrivate(fs, dir); err != nil {
		return nil, err
	}
	if emit == nil {
		emit = func(Event) {}
	}
	e := &Evaluator{store: store, fs: fs, path: filepath.Join(dir, "alerts.json"), emit: emit, items: map[string]*itemState{}}
	e.fs.Restrict(e.path, fileMode) // so that a move to alerts.json.bad keeps it private
	b, err := readFile(e.fs, e.path)
	if errors.Is(err, os.ErrNotExist) {
		return e, nil
	}
	if err == nil {
		err = e.load(b)
	}
	if err != nil {
		_ = e.fs.Rename(e.path, e.path+".bad") // best effort: the service runs on defaults either way
		return e, nil
	}
	e.saved = b
	return e, nil
}

func (e *Evaluator) load(b []byte) error {
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return err
	}
	if sf.Version != stateVersion {
		return errVersion
	}
	for name, st := range sf.Items {
		if st == nil {
			return errCorrupt
		}
		if err := validateRules(st.Rules); err != nil {
			return err
		}
		for i, a := range st.Active {
			if !slices.Contains(ruleKeys, a.Key) || slices.ContainsFunc(st.Active[:i], func(p activeAlert) bool { return p.Key == a.Key }) {
				return errCorrupt
			}
		}
		for k, n := range st.Runs {
			if !slices.Contains(ruleKeys, k) || n < 0 || n > maxRun {
				return errCorrupt
			}
		}
		if idle(st) {
			delete(sf.Items, name) // valid but empty: dropped at the next save
		}
	}
	if sf.Items != nil {
		e.items = sf.Items
	}
	return nil
}

// Rules returns the rules of item: both keys, cpu_high then mem_high, defaults unless overridden.
func (e *Evaluator) Rules(item string) []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rules(item)
}

func (e *Evaluator) rules(item string) []Rule {
	out := DefaultRules()
	if st := e.items[item]; st != nil {
		for _, r := range st.Rules {
			out[slices.IndexFunc(out, func(d Rule) bool { return d.Key == r.Key })] = r
		}
	}
	return out
}

// Active returns the firing alerts of item.
func (e *Evaluator) Active(item string) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []Event{}
	if st := e.items[item]; st != nil {
		for _, a := range st.Active {
			out = append(out, Event{item, a.Key, "firing", a.Value, time.Unix(a.Since, 0).UTC()})
		}
	}
	return out
}

// OnMinute evaluates the committed minute of item. It is the Store's
// SetOnMinute hook; services never call it.
func (e *Evaluator) OnMinute(item string, minute time.Time) {
	m := minute.Unix() / 60
	vals := map[string]float64{}
	for _, k := range ruleKeys {
		v, ok, err := e.store.minuteValue(item, "alert/"+k, m)
		if err != nil {
			return // unreadable: like a minute never written
		}
		if ok {
			vals[k] = v
		}
	}
	e.do(func() []Event { return e.onMinute(item, m, vals) })
}

// onMinute applies the values of minute m. Called with e.mu held, so that the
// rules are those in force when the minute is judged.
func (e *Evaluator) onMinute(item string, m int64, vals map[string]float64) []Event {
	st := e.state(item)
	if m == st.Last {
		return nil // already counted by a rule change made while the minute was read
	}
	if m != st.Last+1 {
		// Minutes without any Record: the run restarts, an active alert stays.
		// Last is 0 only when no run counts, so a fresh item loses nothing.
		st.Runs = nil
	}
	var evs []Event
	for _, r := range e.rules(item) {
		v, ok := vals[r.Key]
		if !r.Enabled {
			v, ok = 0, false
		}
		i := slices.IndexFunc(st.Active, func(a activeAlert) bool { return a.Key == r.Key })
		if !ok || v < r.Threshold {
			delete(st.Runs, r.Key)
			if i >= 0 {
				evs = append(evs, Event{item, r.Key, "resolved", v, time.Unix(st.Active[i].Since, 0).UTC()})
				st.Active = slices.Delete(st.Active, i, i+1)
			}
			continue
		}
		if st.Runs == nil {
			st.Runs = map[string]int{}
		}
		st.Runs[r.Key] = min(st.Runs[r.Key]+1, maxRun)
		if n := st.Runs[r.Key]; n >= r.Minutes && i < 0 {
			evs = append(evs, e.fire(item, st, r.Key, m-int64(n)+1, v))
		}
	}
	st.Last = m
	e.put(item, st)
	return evs
}

// SetRules replaces the rules of item for the keys listed; the others keep
// theirs. On error nothing changes. The alerts of item are then recounted
// from its history: one that no longer holds resolves, one that now holds fires.
func (e *Evaluator) SetRules(item string, rules []Rule) error {
	if !utf8.ValidString(item) {
		return ErrBadItem
	}
	if err := validateRules(rules); err != nil {
		return err
	}
	e.do(func() []Event {
		merged := e.rules(item)
		for _, r := range rules {
			merged[slices.IndexFunc(merged, func(d Rule) bool { return d.Key == r.Key })] = r
		}
		st := e.state(item)
		st.Rules = merged
		if slices.Equal(merged, DefaultRules()) {
			st.Rules = nil
		}
		e.items[item] = st // reconcile counts with the new rules; put drops it again if idle
		return e.reconcile(item, st)
	})
	return nil
}

// Rebuild recounts the runs of item from its history, at start: an alert
// still holding is kept silently, one that ended while the service was down
// resolves, one that built up fires.
func (e *Evaluator) Rebuild(item string) {
	e.do(func() []Event { return e.reconcile(item, e.state(item)) })
}

// Resolve ends every active alert of item, which is no longer measured, and
// clears its runs: later minutes fire only after a full new run.
func (e *Evaluator) Resolve(item string) {
	e.do(func() []Event {
		st := e.items[item]
		if st == nil {
			return nil
		}
		var evs []Event
		for _, a := range st.Active {
			evs = append(evs, Event{item, a.Key, "resolved", 0, time.Unix(a.Since, 0).UTC()})
		}
		st.Active, st.Runs = nil, nil
		e.put(item, st)
		return evs
	})
}

// Prune drops the rules and state of the items no longer in the Store.
// Items is read under e.mu, so the state of an item that commits its first
// minute meanwhile is not dropped.
func (e *Evaluator) Prune() {
	e.mu.Lock()
	defer e.mu.Unlock()
	keep := e.store.Items()
	for k := range e.items {
		if _, ok := slices.BinarySearch(keep, k); !ok {
			delete(e.items, k)
		}
	}
	e.save()
}

// LastError returns the error of the last write of alerts.json; nil once the
// file matches the state again.
func (e *Evaluator) LastError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.saveErr
}

// reconcile recounts each run of item from the newest committed minute
// backwards, up to the rule's window, and fires or resolves to match. The
// walk only starts if that minute is at most 2 minutes older than the clock's
// last complete minute; otherwise the item was down and every run is 0.
// Called with e.mu held.
// ponytail: it reads up to 120 minutes of the ring under e.mu; it runs at
// start and on a rule change only.
func (e *Evaluator) reconcile(item string, st *itemState) []Event {
	newest, hist, err := e.store.recentMinutes(item, maxRun)
	if err != nil {
		e.put(item, st) // unreadable: the alerts stay until a minute shows otherwise
		return nil
	}
	fresh := newest > 0 && newest >= e.store.clock.Now().Unix()/60-1-2
	var evs []Event
	runs := map[string]int{}
	for _, r := range e.rules(item) {
		cells := hist["alert/"+r.Key]
		v, n := 0.0, 0
		if fresh && len(cells) > 0 {
			if c := cells[len(cells)-1]; c.OK {
				v = c.Avg
			}
			for n < r.Minutes && n < len(cells) {
				if c := cells[len(cells)-1-n]; !c.OK || c.Avg < r.Threshold {
					break
				}
				n++
			}
		}
		if !r.Enabled {
			n = 0
		}
		i := slices.IndexFunc(st.Active, func(a activeAlert) bool { return a.Key == r.Key })
		switch {
		case i >= 0 && n < r.Minutes:
			evs = append(evs, Event{item, r.Key, "resolved", v, time.Unix(st.Active[i].Since, 0).UTC()})
			st.Active = slices.Delete(st.Active, i, i+1)
		case i < 0 && n >= r.Minutes:
			evs = append(evs, e.fire(item, st, r.Key, newest-int64(n)+1, v))
		}
		if n > 0 {
			runs[r.Key] = n
		}
	}
	st.Runs, st.Last = runs, newest
	e.put(item, st)
	return evs
}

// fire records an active alert of key since the given unix minute.
func (e *Evaluator) fire(item string, st *itemState, key string, sinceMin int64, v float64) Event {
	st.Active = append(st.Active, activeAlert{key, sinceMin * 60, v})
	slices.SortFunc(st.Active, func(a, b activeAlert) int { return strings.Compare(a.Key, b.Key) })
	return Event{item, key, "firing", v, time.Unix(sinceMin*60, 0).UTC()}
}

// do runs fn under e.mu and queues its events in the same section, so the
// queue follows the order of the state changes. The call that finds no one
// emitting becomes the drainer: it emits the queue, outside every lock, until
// it is empty, including the events queued meanwhile by other calls or by emit
// itself. A slow emit delays later events only; a call that re-enters from
// emit queues and returns.
func (e *Evaluator) do(fn func() []Event) {
	drain := func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.pending = append(e.pending, fn()...)
		if e.draining || len(e.pending) == 0 {
			return false
		}
		e.draining = true
		return true
	}()
	if drain {
		e.drain()
	}
}

// drain emits the queue until it is empty. If emit panics, the event it was
// given counts as emitted, the rest of the batch goes back to the front of the
// queue and the next call becomes the drainer; the panic goes on to the caller.
func (e *Evaluator) drain() {
	var batch []Event
	defer func() {
		if batch == nil {
			return
		}
		e.mu.Lock()
		e.pending = append(slices.Clone(batch[1:]), e.pending...)
		e.draining = false
		e.mu.Unlock()
	}()
	for {
		e.mu.Lock()
		if len(e.pending) == 0 {
			e.draining = false
			e.mu.Unlock()
			return
		}
		batch, e.pending = e.pending, nil
		e.mu.Unlock()
		for len(batch) > 0 {
			e.emit(batch[0])
			batch = batch[1:]
		}
		batch = nil
	}
}

// state returns the state of item, a fresh one if it has none. Called with e.mu held.
func (e *Evaluator) state(item string) *itemState {
	if st := e.items[item]; st != nil {
		return st
	}
	return &itemState{}
}

// put stores st for item, or drops the item when nothing differs from
// defaults and idle, then saves. Called with e.mu held.
func (e *Evaluator) put(item string, st *itemState) {
	if len(st.Runs) == 0 {
		st.Runs, st.Last = nil, 0
	}
	if idle(st) {
		delete(e.items, item)
	} else {
		e.items[item] = st
	}
	e.save()
}

func idle(st *itemState) bool {
	return len(st.Rules) == 0 && len(st.Active) == 0 && len(st.Runs) == 0
}

// save writes the state if it differs from the file. Called with e.mu held.
// A failed write is retried at the next change and kept for LastError; the
// service never fails on it.
func (e *Evaluator) save() {
	b, err := json.Marshal(stateFile{stateVersion, e.items})
	if err == nil && bytes.Equal(b, e.saved) {
		e.saveErr = nil
		return
	}
	if err == nil {
		err = writeFileAtomic(e.fs, e.path, b)
	}
	if err == nil {
		e.saved = b
	}
	e.saveErr = err
}

// writeFileAtomic writes path through path.tmp, fsync and rename.
func writeFileAtomic(fs fsys, path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := fs.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return err
	}
	fs.Restrict(tmp, fileMode) // a leftover keeps its old mode through O_TRUNC
	_, err = f.WriteAt(b, 0)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = fs.Rename(tmp, path)
	}
	if err != nil {
		_ = fs.Remove(tmp)
		return err
	}
	return fs.SyncDir(filepath.Dir(path))
}

func readFile(fs fsys, path string) ([]byte, error) {
	f, err := fs.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	b := make([]byte, info.Size())
	return b, readFull(f, b, 0)
}
