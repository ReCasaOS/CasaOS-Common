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

	mu    sync.Mutex // guards the fields below
	items map[string]*itemState
	saved []byte // content last written
}

// NewEvaluator loads <dir>/alerts.json. A missing file means defaults; an
// unreadable, invalid or other-version file is moved to alerts.json.bad and
// defaults are used. emit is called after the state is saved, outside every lock.
func NewEvaluator(store *Store, dir string, emit func(Event)) (*Evaluator, error) {
	if err := store.fs.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if emit == nil {
		emit = func(Event) {}
	}
	e := &Evaluator{store: store, fs: store.fs, path: filepath.Join(dir, "alerts.json"), emit: emit, items: map[string]*itemState{}}
	b, err := readFile(e.fs, e.path)
	if errors.Is(err, os.ErrNotExist) {
		return e, nil
	}
	if err == nil {
		err = e.load(b)
	}
	if err != nil {
		_ = e.fs.Remove(e.path + ".bad")
		_ = e.fs.Rename(e.path, e.path+".bad") // best effort: the service runs on defaults either way
		return e, nil
	}
	e.saved, _ = json.Marshal(stateFile{stateVersion, e.items})
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
	for _, st := range sf.Items {
		if st == nil {
			return errCorrupt
		}
		if err := validateRules(st.Rules); err != nil {
			return err
		}
		for _, a := range st.Active {
			if !slices.Contains(ruleKeys, a.Key) {
				return errCorrupt
			}
		}
		for k := range st.Runs {
			if !slices.Contains(ruleKeys, k) {
				return errCorrupt
			}
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
	rules := e.Rules(item)
	vals := map[string]float64{}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		v, ok, err := e.store.minuteValue(item, "alert/"+r.Key, m)
		if err != nil {
			return // unreadable: like a minute never written
		}
		if ok {
			vals[r.Key] = v
		}
	}

	var evs []Event
	e.mu.Lock()
	st := e.items[item]
	if st == nil {
		st = &itemState{}
	}
	if m != st.Last+1 {
		// Minutes without any Record: the run restarts, an active alert stays.
		// Last is 0 only when no run counts, so a fresh item loses nothing.
		st.Runs = nil
	}
	for _, r := range rules {
		v, ok := vals[r.Key]
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
		st.Runs[r.Key]++
		if n := st.Runs[r.Key]; n >= r.Minutes && i < 0 {
			since := (m - int64(n) + 1) * 60
			st.Active = append(st.Active, activeAlert{r.Key, since, v})
			slices.SortFunc(st.Active, func(a, b activeAlert) int { return strings.Compare(a.Key, b.Key) })
			evs = append(evs, Event{item, r.Key, "firing", v, time.Unix(since, 0).UTC()})
		}
	}
	st.Last = 0
	if len(st.Runs) > 0 {
		st.Last = m
	} else {
		st.Runs = nil
	}
	if st.Rules == nil && len(st.Active) == 0 && st.Runs == nil {
		delete(e.items, item)
	} else {
		e.items[item] = st
	}
	e.save()
	e.mu.Unlock()
	for _, ev := range evs {
		e.emit(ev)
	}
}

// save writes the state if it changed. Called with e.mu held. A failed write
// is retried at the next change; the service never fails on it.
func (e *Evaluator) save() {
	b, err := json.Marshal(stateFile{stateVersion, e.items})
	if err != nil || bytes.Equal(b, e.saved) {
		return
	}
	if writeFileAtomic(e.fs, e.path, b) == nil {
		e.saved = b
	}
}

// writeFileAtomic writes path through path.tmp, fsync and rename.
func writeFileAtomic(fs fsys, path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := fs.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
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
