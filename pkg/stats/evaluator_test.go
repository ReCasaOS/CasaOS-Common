package stats_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
)

const item = "app:demo:web:1"

// noAlert marks a minute recorded without any alert/ series.
var noAlert = math.NaN()

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }

type rig struct {
	t      *testing.T
	dir    string
	store  *stats.Store
	ev     *stats.Evaluator
	mu     sync.Mutex
	events []stats.Event
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, dir: t.TempDir()}
	s, err := stats.Open(r.dir, stats.NewFakeClock(base))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r.store = s
	r.open()
	return r
}

// open (re)creates the evaluator on the rig's directory, as after a restart.
func (r *rig) open() {
	r.t.Helper()
	ev, err := stats.NewEvaluator(r.store, r.dir, func(e stats.Event) {
		r.mu.Lock()
		r.events = append(r.events, e)
		r.mu.Unlock()
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.ev = ev
}

func (r *rig) take() []stats.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// record writes 12 samples into minute i: both alert series at v (noAlert: another series only).
func (r *rig) record(i int, v float64) {
	r.t.Helper()
	smp := stats.Sample{"cpu/total": 1}
	if !math.IsNaN(v) {
		smp["alert/cpu_high"], smp["alert/mem_high"] = v, v
	}
	for k := 0; k < 12; k++ {
		if err := r.store.Record(item, at(i).Add(time.Duration(k)*5*time.Second), smp); err != nil {
			r.t.Fatal(err)
		}
	}
	// An empty sample at the next minute commits minute i.
	if err := r.store.Record(item, at(i+1), stats.Sample{}); err != nil {
		r.t.Fatal(err)
	}
}

// step records minute i then evaluates it.
func (r *rig) step(i int, v float64) {
	r.t.Helper()
	r.record(i, v)
	r.ev.OnMinute(item, at(i))
}

// steps runs consecutive minutes from..from+len(vals)-1.
func (r *rig) steps(from int, vals ...float64) {
	r.t.Helper()
	for j, v := range vals {
		r.step(from+j, v)
	}
}

func repeat(v float64, n int) []float64 { return slices.Repeat([]float64{v}, n) }

func (r *rig) expect(want ...stats.Event) {
	r.t.Helper()
	got := r.take()
	if !slices.Equal(got, want) {
		r.t.Fatalf("events = %+v, want %+v", got, want)
	}
}

func (r *rig) writeState(s string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, "alerts.json"), []byte(s), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) state() map[string]any {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, "alerts.json"))
	if err != nil {
		r.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		r.t.Fatalf("alerts.json: %v: %s", err, b)
	}
	return m
}

func firing(key string, v float64, since int) stats.Event {
	return stats.Event{Item: item, Key: key, State: "firing", Value: v, Since: at(since)}
}

func resolved(key string, v float64, since int) stats.Event {
	return stats.Event{Item: item, Key: key, State: "resolved", Value: v, Since: at(since)}
}

// cpuOnly disables mem_high so that tests see cpu_high events alone.
const cpuOnly = `{"version":1,"items":{"` + item + `":{"rules":[{"key":"cpu_high","enabled":true,"threshold":90,"minutes":10},{"key":"mem_high","enabled":false,"threshold":90,"minutes":5}]}}}`

func TestCpuHighFiresOnTenthMinute(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(95, 9)...)
	r.expect()
	r.step(9, 95)
	r.expect(firing("cpu_high", 95, 0))
	r.steps(10, repeat(95, 5)...)
	r.expect()
	r.step(15, 50)
	r.expect(resolved("cpu_high", 50, 0))
	r.steps(16, 50, 40, 30)
	r.expect()
}

func TestMemHighFiresOnFifthMinute(t *testing.T) {
	r := newRig(t)
	r.writeState(`{"version":1,"items":{"` + item + `":{"rules":[{"key":"cpu_high","enabled":false,"threshold":90,"minutes":10}]}}}`)
	r.open()
	r.steps(0, repeat(95, 4)...)
	r.expect()
	r.step(4, 95)
	r.expect(firing("mem_high", 95, 0))
	r.steps(5, 95, 95)
	r.expect()
	r.step(7, 10)
	r.expect(resolved("mem_high", 10, 0))
}

func TestHookWiring(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.store.SetOnMinute(r.ev.OnMinute)
	for i := 0; i < 10; i++ {
		r.record(i, 95)
	}
	r.expect(firing("cpu_high", 95, 0))
}

func TestStreakBroken(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(95, 9)...)
	r.step(9, 50)
	r.steps(10, repeat(95, 9)...)
	r.expect()
}

func TestThresholdBoundary(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(90, 10)...)
	r.expect(firing("cpu_high", 90, 0))
	r.step(10, 89.99)
	got := r.take()
	if len(got) != 1 || got[0].State != "resolved" {
		t.Fatalf("89.99 after a firing: %+v, want one resolved", got)
	}
}

func TestGapResetsRun(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(95, 9)...)
	r.step(9, noAlert)
	r.step(10, 95)
	r.expect()

	r.steps(11, repeat(95, 9)...)
	r.expect(firing("cpu_high", 95, 10))
	r.step(20, noAlert)
	r.expect(resolved("cpu_high", 0, 10))
}

func TestSkippedMinuteIsGap(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	for i := 0; i <= 10; i++ {
		r.record(i, 95)
	}
	for i := 0; i <= 8; i++ {
		r.ev.OnMinute(item, at(i))
	}
	r.ev.OnMinute(item, at(10))
	r.expect()
}

func TestSkippedMinuteDoesNotResolveFiring(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(95, 10)...)
	r.expect(firing("cpu_high", 95, 0))
	// Minutes 10 and 11: no Record at all (the measurement failed).
	r.step(12, 95)
	r.expect()
	if got := r.ev.Active(item); !slices.Equal(got, []stats.Event{firing("cpu_high", 95, 0)}) {
		t.Fatalf("Active = %+v", got)
	}
	runs := r.state()["items"].(map[string]any)[item].(map[string]any)["runs"].(map[string]any)
	if runs["cpu_high"] != 1.0 {
		t.Fatalf("runs = %v, want cpu_high 1", runs)
	}
	r.step(13, 50)
	r.expect(resolved("cpu_high", 50, 0))
	r.step(14, 50)
	r.expect()
}

func TestUnreadableMinuteIsSkipped(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(95, 10)...)
	r.expect(firing("cpu_high", 95, 0))
	if err := r.store.Close(); err != nil {
		t.Fatal(err)
	}
	r.ev.OnMinute(item, at(10)) // the ring cannot be read: not evidence of a low minute
	r.expect()
	if got := r.ev.Active(item); len(got) != 1 {
		t.Fatalf("Active = %+v", got)
	}
}

func TestDisabledRuleSilent(t *testing.T) {
	r := newRig(t)
	r.writeState(`{"version":1,"items":{"` + item + `":{"rules":[{"key":"cpu_high","enabled":false,"threshold":90,"minutes":10},{"key":"mem_high","enabled":false,"threshold":90,"minutes":5}]}}}`)
	r.open()
	r.steps(0, repeat(99, 30)...)
	r.expect()
	if got := r.ev.Active(item); len(got) != 0 {
		t.Fatalf("Active = %+v", got)
	}

	// A rule found disabled while its alert is active resolves it, once.
	r2 := newRig(t)
	r2.writeState(`{"version":1,"items":{"` + item + `":{"rules":[{"key":"cpu_high","enabled":false,"threshold":90,"minutes":10},{"key":"mem_high","enabled":false,"threshold":90,"minutes":5}],"active":[{"key":"cpu_high","since":` + itoa(at(0).Unix()) + `,"value":95}]}}}`)
	r2.open()
	r2.steps(30, 99, 99)
	r2.expect(resolved("cpu_high", 0, 0))
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestStateSurvivesRestart(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(95, 10)...)
	r.expect(firing("cpu_high", 95, 0))
	r.open()
	if got := r.ev.Active(item); !slices.Equal(got, []stats.Event{firing("cpu_high", 95, 0)}) {
		t.Fatalf("Active after restart = %+v", got)
	}
	r.step(10, 96)
	r.expect()
	r.step(11, 20)
	r.expect(resolved("cpu_high", 20, 0))

	// Runs persist too.
	r.steps(20, repeat(95, 7)...)
	r.open()
	r.steps(27, 95, 95)
	r.expect()
	r.step(29, 95)
	r.expect(firing("cpu_high", 95, 20))
}

func TestCorruptStateFile(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":       "not json {",
		"version 2":     `{"version":2}`,
		"empty":         "",
		"no version":    `{"items":{}}`,
		"bad key":       `{"version":1,"items":{"x":{"rules":[{"key":"disk_high","enabled":true,"threshold":90,"minutes":10}]}}}`,
		"dup key":       `{"version":1,"items":{"x":{"rules":[{"key":"cpu_high","enabled":true,"threshold":90,"minutes":10},{"key":"cpu_high","enabled":true,"threshold":90,"minutes":10}]}}}`,
		"threshold":     `{"version":1,"items":{"x":{"rules":[{"key":"cpu_high","enabled":true,"threshold":500,"minutes":10}]}}}`,
		"threshold low": `{"version":1,"items":{"x":{"rules":[{"key":"cpu_high","enabled":true,"threshold":0.5,"minutes":10}]}}}`,
		"minutes":       `{"version":1,"items":{"x":{"rules":[{"key":"cpu_high","enabled":true,"threshold":90,"minutes":121}]}}}`,
		"minutes low":   `{"version":1,"items":{"x":{"rules":[{"key":"cpu_high","enabled":true,"threshold":90,"minutes":0}]}}}`,
		"active key":    `{"version":1,"items":{"x":{"active":[{"key":"disk_high","since":0,"value":1}]}}}`,
		"runs key":      `{"version":1,"items":{"x":{"runs":{"disk_high":3}}}}`,
		"null item":     `{"version":1,"items":{"x":null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.writeState(content)
			r.open()
			if got := r.ev.Rules("x"); !slices.Equal(got, stats.DefaultRules()) {
				t.Fatalf("Rules = %+v, want defaults", got)
			}
			if got := r.ev.Active("x"); len(got) != 0 {
				t.Fatalf("Active = %+v", got)
			}
			b, err := os.ReadFile(filepath.Join(r.dir, "alerts.json.bad"))
			if err != nil || string(b) != content {
				t.Fatalf("alerts.json.bad = %q, %v; want the original bytes", b, err)
			}
			if _, err := os.Stat(filepath.Join(r.dir, "alerts.json")); !os.IsNotExist(err) {
				t.Fatalf("alerts.json still there: %v", err)
			}
		})
	}
}

func TestValidStateIsKept(t *testing.T) {
	r := newRig(t)
	r.writeState(`{"version":1,"items":{"x":{"rules":[{"key":"mem_high","enabled":true,"threshold":100,"minutes":120},{"key":"cpu_high","enabled":true,"threshold":1,"minutes":1}]}}}`)
	r.open()
	want := []stats.Rule{{Key: "cpu_high", Enabled: true, Threshold: 1, Minutes: 1}, {Key: "mem_high", Enabled: true, Threshold: 100, Minutes: 120}}
	if got := r.ev.Rules("x"); !slices.Equal(got, want) {
		t.Fatalf("Rules = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "alerts.json.bad")); !os.IsNotExist(err) {
		t.Fatalf("valid file quarantined: %v", err)
	}
}

func TestEmitOutsideLock(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	var ev *stats.Evaluator
	var calls int
	ev, err := stats.NewEvaluator(r.store, r.dir, func(e stats.Event) {
		calls++
		_ = ev.Active(e.Item)
		_ = ev.Rules(e.Item)
	})
	if err != nil {
		t.Fatal(err)
	}
	r.ev = ev
	done := make(chan struct{})
	go func() {
		r.steps(0, repeat(95, 10)...)
		r.step(10, 10)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: emit cannot call Active or Rules")
	}
	if calls != 2 {
		t.Fatalf("emit called %d times, want 2", calls)
	}
}

func TestRulesReturnsCopy(t *testing.T) {
	r := newRig(t)
	got := r.ev.Rules(item)
	got[0].Threshold = 1
	if r.ev.Rules(item)[0].Threshold != 90 {
		t.Fatal("default rules changed through the returned slice")
	}
	r.writeState(cpuOnly)
	r.open()
	got = r.ev.Rules(item)
	got[1].Enabled = true
	if r.ev.Rules(item)[1].Enabled {
		t.Fatal("stored rules changed through the returned slice")
	}
	if got := r.ev.Rules("unknown"); !slices.Equal(got, stats.DefaultRules()) {
		t.Fatalf("Rules(unknown) = %+v", got)
	}
}

func TestStateFileAtomic(t *testing.T) {
	r := newRig(t)
	check := func(i int) {
		t.Helper()
		ents, err := os.ReadDir(r.dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Fatalf("minute %d: %s left behind", i, e.Name())
			}
		}
		if _, err := os.Stat(filepath.Join(r.dir, "alerts.json")); err == nil {
			if v := r.state()["version"]; v != 1.0 {
				t.Fatalf("minute %d: version %v", i, v)
			}
		}
	}
	vals := append(repeat(95, 12), 10, 10)
	for i, v := range vals {
		r.step(i, v)
		check(i)
	}
	if len(r.take()) != 4 {
		t.Fatal("want cpu and mem firing then resolved")
	}
	// Back to idle: the item is no longer stored.
	if items := r.state()["items"].(map[string]any); len(items) != 0 {
		t.Fatalf("idle item still stored: %v", items)
	}
}
