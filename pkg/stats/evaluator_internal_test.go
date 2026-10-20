package stats

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const evItem = "app:demo:web:1"

func high(int) Sample { return Sample{"alert/cpu_high": 95, "alert/mem_high": 95} }

var errTrap = errors.New("trap")

// trapFS fails, or panics on, the creation of alerts.json.tmp.
type trapFS struct {
	fsys
	fail, panics *atomic.Bool
}

func (f trapFS) OpenFile(name string, flag int, perm os.FileMode) (file, error) {
	if strings.HasSuffix(name, "alerts.json.tmp") {
		if f.panics.Load() {
			panic("trap")
		}
		if f.fail.Load() {
			return nil, errTrap
		}
	}
	return f.fsys.OpenFile(name, flag, perm)
}

type evRig struct {
	s      *Store
	ev     *Evaluator
	mu     sync.Mutex
	events []Event
}

func newEvRig(t *testing.T, fs fsys) *evRig {
	t.Helper()
	dir := t.TempDir()
	s, err := open(dir, NewFakeClock(t0), fs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &evRig{s: s}
	r.ev, err = newEvaluator(s, dir, func(e Event) {
		r.mu.Lock()
		r.events = append(r.events, e)
		r.mu.Unlock()
	}, fs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *evRig) take() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// parkedOnMinute evaluates minute i in a goroutine parked on its first read of
// the ring, runs during while it is parked, then lets it finish.
func parkedOnMinute(t *testing.T, r *evRig, bfs blockingFS, i int, during func()) {
	t.Helper()
	feedMinute(t, r.s, evItem, minuteAt(i), high)
	bfs.stampReads.Store(1)
	done := make(chan struct{})
	go func() { r.ev.OnMinute(evItem, minuteAt(i)); close(done) }()
	<-bfs.entered
	during()
	close(bfs.release)
	<-done
}

func newBlockingFS() blockingFS {
	return blockingFS{fsys: osFS, armed: new(atomic.Bool), stampReads: new(atomic.Int32), entered: make(chan struct{}), release: make(chan struct{})}
}

func setRules(t *testing.T, ev *Evaluator, rules ...Rule) {
	t.Helper()
	if err := ev.SetRules(evItem, rules); err != nil {
		t.Fatal(err)
	}
}

// A rule disabled while a minute is being read is not judged on the old rules.
func TestOnMinuteUsesRulesUnderLock(t *testing.T) {
	bfs := newBlockingFS()
	r := newEvRig(t, bfs)
	setRules(t, r.ev, Rule{"cpu_high", true, 90, 1}, Rule{"mem_high", false, 90, 5})
	parkedOnMinute(t, r, bfs, 0, func() { setRules(t, r.ev, Rule{"cpu_high", false, 90, 1}) })
	if got := r.take(); len(got) != 0 {
		t.Fatalf("disabled rule fired: %+v", got)
	}
}

// A minute already counted by a rule change in flight is not counted twice.
func TestOnMinuteAfterReconcileCountsOnce(t *testing.T) {
	bfs := newBlockingFS()
	r := newEvRig(t, bfs)
	setRules(t, r.ev, Rule{"mem_high", false, 90, 5})
	for i := 0; i < 9; i++ {
		feedMinute(t, r.s, evItem, minuteAt(i), high)
		r.ev.OnMinute(evItem, minuteAt(i))
	}
	parkedOnMinute(t, r, bfs, 9, func() { setRules(t, r.ev, Rule{"cpu_high", true, 90, 11}) })
	if got := r.take(); len(got) != 0 {
		t.Fatalf("minute 9 counted twice: %+v", got)
	}
	feedMinute(t, r.s, evItem, minuteAt(10), high)
	r.ev.OnMinute(evItem, minuteAt(10))
	want := []Event{{evItem, "cpu_high", "firing", 95, minuteAt(0)}}
	if got := r.take(); !slices.Equal(got, want) {
		t.Fatalf("events = %+v, want %+v", got, want)
	}
}

func TestOnMinutePanicReleasesLock(t *testing.T) {
	fs := trapFS{osFS, new(atomic.Bool), new(atomic.Bool)}
	r := newEvRig(t, fs)
	feedMinute(t, r.s, evItem, minuteAt(0), high)
	fs.panics.Store(true)
	func() {
		defer func() { _ = recover() }()
		r.ev.OnMinute(evItem, minuteAt(0))
		t.Fatal("no panic")
	}()
	done := make(chan struct{})
	go func() { r.ev.Active(evItem); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lock still held after a panic")
	}
}

func TestLastError(t *testing.T) {
	fs := trapFS{osFS, new(atomic.Bool), new(atomic.Bool)}
	r := newEvRig(t, fs)
	if err := r.ev.LastError(); err != nil {
		t.Fatalf("fresh: %v", err)
	}
	fs.fail.Store(true)
	feedMinute(t, r.s, evItem, minuteAt(0), high)
	r.ev.OnMinute(evItem, minuteAt(0))
	if err := r.ev.LastError(); !errors.Is(err, errTrap) {
		t.Fatalf("after failed save: %v", err)
	}
	fs.fail.Store(false)
	feedMinute(t, r.s, evItem, minuteAt(1), high)
	r.ev.OnMinute(evItem, minuteAt(1))
	if err := r.ev.LastError(); err != nil {
		t.Fatalf("after good save: %v", err)
	}

	// A state back to what the file holds clears the error without a write.
	setRules(t, r.ev, Rule{"cpu_high", true, 50, 10})
	fs.fail.Store(true)
	setRules(t, r.ev, Rule{"cpu_high", true, 60, 10})
	if err := r.ev.LastError(); !errors.Is(err, errTrap) {
		t.Fatalf("after failed save: %v", err)
	}
	fs.fail.Store(false)
	setRules(t, r.ev, Rule{"cpu_high", true, 50, 10})
	if err := r.ev.LastError(); err != nil {
		t.Fatalf("file matches again: %v", err)
	}
}

// A rule change whose history read fails keeps the new rules, the active
// alerts, and saves both.
func TestSetRulesRingReadError(t *testing.T) {
	ff := &faultFS{fsys: noSync{osFS}}
	r := newEvRig(t, ff)
	setRules(t, r.ev, Rule{"cpu_high", true, 90, 1})
	feedMinute(t, r.s, evItem, minuteAt(0), high)
	r.ev.OnMinute(evItem, minuteAt(0))
	active := r.ev.Active(evItem)
	if len(active) != 1 {
		t.Fatalf("Active = %+v", active)
	}
	ff.readErr = errTrap
	setRules(t, r.ev, Rule{"cpu_high", true, 50, 2})
	ff.readErr = nil
	want := []Rule{{"cpu_high", true, 50, 2}, {"mem_high", true, 90, 5}}
	if got := r.ev.Rules(evItem); !slices.Equal(got, want) {
		t.Fatalf("Rules = %+v", got)
	}
	if got := r.ev.Active(evItem); !slices.Equal(got, active) {
		t.Fatalf("Active = %+v, want %+v", got, active)
	}
	b, err := os.ReadFile(filepath.Join(r.s.dir, "alerts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil || sf.Items[evItem] == nil || !slices.Equal(sf.Items[evItem].Rules, want) {
		t.Fatalf("alerts.json: %s (%v)", b, err)
	}
}

// waitMutexBlocked returns once a goroutine running fn is parked on a sync.Mutex.
func waitMutexBlocked(t *testing.T, fn string) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for i := 0; i < 100000; i++ {
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, fn) && strings.Contains(g, "[sync.Mutex.Lock") {
				return
			}
		}
		runtime.Gosched()
	}
	t.Fatalf("%s never blocked", fn)
}

// State an item gets while Prune runs is not pruned.
func TestPruneKeepsStateCreatedDuringIt(t *testing.T) {
	r := newEvRig(t, noSync{osFS})
	r.ev.mu.Lock()
	done := make(chan struct{})
	go func() { r.ev.Prune(); close(done) }()
	waitMutexBlocked(t, "(*Evaluator).Prune")
	// The item's first minute: in the Store, then evaluated.
	feedMinute(t, r.s, evItem, minuteAt(0), high)
	r.ev.onMinute(evItem, minuteNo(0), map[string]float64{"cpu_high": 95, "mem_high": 95})
	r.ev.mu.Unlock()
	<-done
	r.ev.mu.Lock()
	st := r.ev.items[evItem]
	r.ev.mu.Unlock()
	if st == nil || st.Runs["cpu_high"] != 1 {
		t.Fatalf("state pruned: %+v", st)
	}
}

// Emits come in the order of the state changes, even when the first emit is
// slow; meanwhile reads and Records that emit nothing go on.
func TestEmitOrderFollowsStateChanges(t *testing.T) {
	dir := t.TempDir()
	s, err := open(dir, NewFakeClock(minuteAt(1)), osFS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	feedMinute(t, s, evItem, minuteAt(0), high)
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var log []string
	ev, err := newEvaluator(s, dir, func(e Event) {
		if e.State == "firing" {
			close(entered)
			<-release
		}
		mu.Lock()
		log = append(log, e.State)
		mu.Unlock()
	}, osFS)
	if err != nil {
		t.Fatal(err)
	}
	s.SetOnMinute(ev.OnMinute)
	// The item gets a state first: a rule change then recounts with the new rules.
	if err := ev.SetRules(evItem, []Rule{{Key: "cpu_high", Enabled: true, Threshold: 99, Minutes: 1}}); err != nil {
		t.Fatal(err)
	}

	aDone := make(chan struct{})
	go func() { // A: a rule change fires and its emit blocks
		defer close(aDone)
		if err := ev.SetRules(evItem, []Rule{{Key: "cpu_high", Enabled: true, Threshold: 50, Minutes: 1}}); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the rule change did not fire")
	}
	bDone := make(chan struct{})
	go func() { // B: the item stops being measured
		defer close(bDone)
		ev.Resolve(evItem)
	}()
	for len(ev.Active(evItem)) != 0 { // B's state change is made without waiting for A
		runtime.Gosched()
	}
	// While A's emit is blocked and B waits for its turn, nothing else is held up.
	_ = ev.Rules(evItem)
	_ = ev.Active("other")
	feedMinute(t, s, "other", minuteAt(0), constSample(Sample{"cpu/user": 1}))
	select { // a bound for an absence, not for the order
	case <-bDone:
		t.Fatal("B emitted before A's emit returned")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-aDone
	<-bDone
	if !slices.Equal(log, []string{"firing", "resolved"}) {
		t.Fatalf("emit order %q", log)
	}
}
