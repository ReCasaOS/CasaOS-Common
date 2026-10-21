package stats_test

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
)

// hookRig wires the evaluator as a service does, so that every minute the
// store commits, Close included, is evaluated through the hook.
func hookRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.store.SetOnMinute(r.ev.OnMinute)
	return r
}

// sample records cpu (alert/cpu_high and cpu/total) at t, moving the clock there.
func (r *rig) sample(t time.Time, cpu float64) {
	r.t.Helper()
	r.clock.Set(t)
	if err := r.store.Record(item, t, stats.Sample{"cpu/total": cpu, "alert/cpu_high": cpu}); err != nil {
		r.t.Fatal(err)
	}
}

// highMinutes samples minutes from..to-1 at 95, every 5 s, without the sample
// of minute to that would commit the last one.
func (r *rig) highMinutes(from, to int) {
	r.t.Helper()
	for i := from; i < to; i++ {
		for k := 0; k < 12; k++ {
			r.sample(at(i).Add(time.Duration(k)*5*time.Second), 95)
		}
	}
}

// restart closes the store at the given time, then reopens store and
// evaluator at the other, wired and rebuilt as a service starts.
func (r *rig) restart(closeAt, openAt time.Time) {
	r.t.Helper()
	r.clock.Set(closeAt)
	if err := r.store.Close(); err != nil {
		r.t.Fatal(err)
	}
	r.clock.Set(openAt)
	s, err := stats.Open(r.dir, r.clock)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = s.Close() })
	r.store = s
	r.open()
	s.SetOnMinute(r.ev.OnMinute)
	r.ev.Rebuild(item)
}

// A restart that crosses a minute boundary loses no minute: the 10th high
// minute still fires, right when it is committed.
func TestCloseCommitsPendingMinuteRunSurvives(t *testing.T) {
	r := hookRig(t)
	r.highMinutes(0, 8)
	for k := 0; k <= 10; k++ { // minute 8 up to :50, the 9th high minute, still pending
		r.sample(at(8).Add(time.Duration(k)*5*time.Second), 95)
	}
	r.expect()
	r.restart(at(8).Add(50*time.Second), at(9).Add(10*time.Second))
	r.expect()
	for k := 2; k < 12; k++ { // minute 9 from :10, the 10th
		r.sample(at(9).Add(time.Duration(k)*5*time.Second), 95)
	}
	r.expect()
	r.sample(at(10), 95) // commits minute 9
	r.expect(firing("cpu_high", 95, 0))
	r.sample(at(10).Add(5*time.Second), 95)
	r.expect()
}

// A downtime that covers a whole minute is a gap: the run starts again.
func TestCloseRestartWithWholeMissingMinuteClearsRun(t *testing.T) {
	r := hookRig(t)
	r.highMinutes(0, 8)
	for k := 0; k <= 10; k++ {
		r.sample(at(8).Add(time.Duration(k)*5*time.Second), 95)
	}
	r.restart(at(8).Add(50*time.Second), at(10).Add(10*time.Second)) // minute 9: nothing
	for k := 2; k < 12; k++ {
		r.sample(at(10).Add(time.Duration(k)*5*time.Second), 95)
	}
	r.highMinutes(11, 19)
	r.sample(at(19), 95) // commits minute 18, the 9th of the new run
	r.expect()
	r.highMinutes(19, 20)
	r.sample(at(20), 95)
	r.expect(firing("cpu_high", 95, 10))
}

// Close with nothing pending writes nothing and calls no hook.
func TestCloseWithNothingPendingWritesNothing(t *testing.T) {
	dir := t.TempDir()
	clock := stats.NewFakeClock(at(0))
	s, err := stats.Open(dir, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record(item, at(0), stats.Sample{"cpu/total": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // commits minute 0
		t.Fatal(err)
	}
	var ring string
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".ring" {
			ring = filepath.Join(dir, e.Name())
		}
	}
	before, err := os.ReadFile(ring)
	if err != nil {
		t.Fatal(err)
	}

	clock.Set(at(5))
	s, err = stats.Open(dir, clock)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.SetOnMinute(func(string, time.Time) { calls++ })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("hook called %d times, want 0", calls)
	}
	after, err := os.ReadFile(ring)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("ring file changed by a Close with nothing pending")
	}
}

// The partial minute holds avg, min and max of the samples it had.
func TestClosePartialMinuteValues(t *testing.T) {
	dir := t.TempDir()
	clock := stats.NewFakeClock(at(0))
	s, err := stats.Open(dir, clock)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range []float64{10, 20, 60} {
		if err := s.Record(item, at(0).Add(time.Duration(k)*5*time.Second), stats.Sample{"cpu/total": v, "alert/cpu_high": v}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Set(at(2))
	s, err = stats.Open(dir, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, err := s.Query(item, stats.Range15m)
	if err != nil {
		t.Fatal(err)
	}
	var got *stats.Point
	for _, sd := range res.Series {
		if sd.Chart+"/"+sd.Name != "cpu/total" {
			continue
		}
		for i := range sd.Points {
			if sd.Points[i].T == at(0).Unix() {
				got = &sd.Points[i]
			}
		}
	}
	if got == nil || got.Avg == nil || *got.Avg != 30 || *got.Min != 10 || *got.Max != 60 {
		t.Fatalf("minute 0 = %+v, want avg 30 min 10 max 60", got)
	}

	// The alert series of the partial minute is kept: the evaluator reads 30.
	ev, err := stats.NewEvaluator(s, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.SetRules(item, []stats.Rule{{Key: "cpu_high", Enabled: true, Threshold: 30, Minutes: 1}}); err != nil {
		t.Fatal(err)
	}
	if got := ev.Active(item); len(got) != 1 || got[0].Value != 30 {
		t.Fatalf("Active = %+v, want cpu_high at 30", got)
	}
}

func TestCloseTwice(t *testing.T) {
	r := hookRig(t)
	r.sample(at(0), 95)
	if err := r.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.store.Close(); err != stats.ErrClosed {
		t.Fatalf("second Close = %v, want ErrClosed", err)
	}
}

// The hook is called once for the partial minute, inside Close, and never after.
func TestCloseCallsHookOnceAndNeverAfter(t *testing.T) {
	s, err := stats.Open(t.TempDir(), stats.NewFakeClock(at(0)))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []time.Time
	closed := false
	s.SetOnMinute(func(_ string, m time.Time) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			t.Errorf("hook called after Close returned, minute %v", m)
		}
		got = append(got, m)
	})
	if err := s.Record(item, at(0), stats.Sample{"cpu/total": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(item, at(1), stats.Sample{"cpu/total": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(item, at(1).Add(5*time.Second), stats.Sample{"cpu/total": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	closed = true
	if len(got) != 2 || !got[0].Equal(at(0)) || !got[1].Equal(at(1)) {
		t.Fatalf("hook minutes = %v, want %v then %v", got, at(0), at(1))
	}
	mu.Unlock()
	_ = s.Close()
	if err := s.Record(item, at(2), stats.Sample{"cpu/total": 1}); err != stats.ErrClosed {
		t.Fatalf("Record after Close = %v, want ErrClosed", err)
	}
}

// A Record that committed a minute and is still in its hook when Close starts:
// Close waits for it, then calls the hook for the pending minute, in order.
func TestCloseWaitsForHookInFlight(t *testing.T) {
	s, err := stats.Open(t.TempDir(), stats.NewFakeClock(at(0)))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var got []time.Time
	s.SetOnMinute(func(_ string, m time.Time) {
		if m.Equal(at(0)) {
			close(entered)
			<-release
		}
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	if err := s.Record(item, at(0), stats.Sample{"cpu/total": 1}); err != nil {
		t.Fatal(err)
	}
	recDone := make(chan struct{})
	go func() {
		defer close(recDone)
		_ = s.Record(item, at(1), stats.Sample{"cpu/total": 1}) // commits minute 0
	}()
	<-entered
	closeDone := make(chan error)
	go func() { closeDone <- s.Close() }()
	select {
	case <-closeDone:
		t.Fatal("Close returned while a hook was running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	<-recDone
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || !got[0].Equal(at(0)) || !got[1].Equal(at(1)) {
		t.Fatalf("hook minutes = %v, want %v then %v", got, at(0), at(1))
	}
}
