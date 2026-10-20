package stats_test

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
)

func cpuRule(enabled bool, threshold float64, minutes int) stats.Rule {
	return stats.Rule{Key: "cpu_high", Enabled: enabled, Threshold: threshold, Minutes: minutes}
}

func memRule(enabled bool, threshold float64, minutes int) stats.Rule {
	return stats.Rule{Key: "mem_high", Enabled: enabled, Threshold: threshold, Minutes: minutes}
}

func (r *rig) setRules(rules ...stats.Rule) {
	r.t.Helper()
	if err := r.ev.SetRules(item, rules); err != nil {
		r.t.Fatal(err)
	}
}

// records writes minutes from..from+len(vals)-1 without evaluating them and
// sets the clock just after the last one, as at a restart.
func (r *rig) records(from int, vals ...float64) {
	r.t.Helper()
	for j, v := range vals {
		r.record(from+j, v)
	}
	r.clock.Set(at(from + len(vals)))
}

func TestSetRulesValidation(t *testing.T) {
	good := memRule(true, 50, 3)
	for name, tc := range map[string]struct {
		rules []stats.Rule
		field string
	}{
		"threshold 0":    {[]stats.Rule{good, cpuRule(true, 0, 10)}, "threshold"},
		"threshold 101":  {[]stats.Rule{good, cpuRule(true, 101, 10)}, "threshold"},
		"threshold NaN":  {[]stats.Rule{good, cpuRule(true, math.NaN(), 10)}, "threshold"},
		"threshold +Inf": {[]stats.Rule{good, cpuRule(true, math.Inf(1), 10)}, "threshold"},
		"minutes 0":      {[]stats.Rule{good, cpuRule(true, 90, 0)}, "minutes"},
		"minutes 121":    {[]stats.Rule{good, cpuRule(true, 90, 121)}, "minutes"},
		"key":            {[]stats.Rule{good, {Key: "disk_high", Enabled: true, Threshold: 90, Minutes: 10}}, "key"},
		"duplicate":      {[]stats.Rule{cpuRule(true, 50, 3), cpuRule(true, 60, 3)}, "key"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			before := r.ev.Rules(item)
			err := r.ev.SetRules(item, tc.rules)
			var re *stats.RuleError
			if !errors.As(err, &re) || re.Field != tc.field || re.Index != 1 {
				t.Fatalf("err = %v, want *RuleError at 1 on %s", err, tc.field)
			}
			if got := r.ev.Rules(item); !slices.Equal(got, before) {
				t.Fatalf("Rules changed on error: %+v", got)
			}
		})
	}

	r := newRig(t)
	want := []stats.Rule{cpuRule(true, 1, 1), memRule(true, 100, 120)}
	r.setRules(want[1], want[0])
	if got := r.ev.Rules(item); !slices.Equal(got, want) {
		t.Fatalf("Rules = %+v, want %+v", got, want)
	}
	r.setRules(cpuRule(false, 50, 7))
	want[0] = cpuRule(false, 50, 7)
	if got := r.ev.Rules(item); !slices.Equal(got, want) {
		t.Fatalf("one-element list: Rules = %+v, want %+v", got, want)
	}
}

func TestRebuildContinuesStreak(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.records(0, repeat(95, 7)...)
	r.ev.Rebuild(item)
	r.expect()
	r.steps(7, 95, 95, 95)
	r.expect(firing("cpu_high", 95, 0))
}

func TestRebuildFiresWhenStateLost(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.records(0, repeat(95, 12)...)
	r.ev.Rebuild(item)
	r.expect(firing("cpu_high", 95, 2))
	r.step(12, 95)
	r.expect()
}

func TestRebuildKeepsActiveSilently(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(95, 10)...)
	r.expect(firing("cpu_high", 95, 0))
	r.open()
	r.clock.Set(at(10))
	r.ev.Rebuild(item)
	r.expect()
	r.step(10, 20)
	r.expect(resolved("cpu_high", 20, 0))
}

func TestRebuildResolvesAfterDowntime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clock int // newest committed minute is 9
		want  []stats.Event
	}{
		{"down 5 minutes", 15, []stats.Event{resolved("cpu_high", 0, 0)}},
		{"down 3 minutes", 13, []stats.Event{resolved("cpu_high", 0, 0)}},
		{"restart within 2 minutes", 12, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.writeState(cpuOnly)
			r.open()
			r.steps(0, repeat(95, 10)...)
			r.expect(firing("cpu_high", 95, 0))
			r.open()
			r.clock.Set(at(tc.clock))
			r.ev.Rebuild(item)
			r.expect(tc.want...)
			if got := len(r.ev.Active(item)); got != 1-len(tc.want) {
				t.Fatalf("Active = %d alerts", got)
			}
		})
	}
}

func TestRebuildNoHistoryNoSeries(t *testing.T) {
	r := newRig(t)
	r.records(0, repeat(noAlert, 12)...)
	r.ev.Rebuild(item)
	r.ev.Rebuild("never-recorded")
	r.expect()
	if _, err := os.Stat(filepath.Join(r.dir, "alerts.json")); err == nil {
		if items := r.state()["items"].(map[string]any); len(items) != 0 {
			t.Fatalf("state for nothing: %v", items)
		}
	}
}

func TestDisableWhileFiring(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(95, 10)...)
	r.expect(firing("cpu_high", 95, 0))
	r.setRules(cpuRule(false, 90, 10))
	r.expect(resolved("cpu_high", 95, 0))
	r.steps(10, repeat(95, 15)...)
	r.expect()
	if got := r.ev.Active(item); len(got) != 0 {
		t.Fatalf("Active = %+v", got)
	}
	st := r.state()["items"].(map[string]any)[item].(map[string]any)
	if _, ok := st["active"]; ok {
		t.Fatalf("state still active: %v", st)
	}
	if rules := st["rules"].([]any); rules[0].(map[string]any)["enabled"] != false {
		t.Fatalf("state rules = %v", rules)
	}
}

func TestRaiseThresholdWhileFiring(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(95, 10)...)
	r.expect(firing("cpu_high", 95, 0))
	r.setRules(cpuRule(true, 99, 10))
	r.expect(resolved("cpu_high", 95, 0))
	r.step(10, 95)
	r.expect()

	r = newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(60, 12)...)
	r.expect()
	r.clock.Set(at(12))
	r.setRules(cpuRule(true, 50, 10))
	r.expect(firing("cpu_high", 60, 2))
}

func TestMinutesChangedWhileFiring(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(95, 15)...)
	r.expect(firing("cpu_high", 95, 0))
	r.setRules(cpuRule(true, 90, 120))
	r.expect(resolved("cpu_high", 95, 0))
	r.setRules(cpuRule(true, 90, 10))
	r.expect(firing("cpu_high", 95, 5))
	r.step(15, 95)
	r.expect()
}

func TestResolveStoppedItem(t *testing.T) {
	r := newRig(t)
	r.steps(0, repeat(95, 10)...)
	r.expect(firing("mem_high", 95, 0), firing("cpu_high", 95, 0))
	r.ev.Resolve(item)
	r.expect(resolved("cpu_high", 0, 0), resolved("mem_high", 0, 0))
	r.ev.Resolve(item)
	r.expect()
	if got := r.ev.Active(item); len(got) != 0 {
		t.Fatalf("Active = %+v", got)
	}
}

func TestResolveThenOnMinuteEmitsNothing(t *testing.T) {
	r := newRig(t)
	r.writeState(cpuOnly)
	r.open()
	r.steps(0, repeat(95, 10)...)
	r.expect(firing("cpu_high", 95, 0))
	r.ev.Resolve(item)
	r.expect(resolved("cpu_high", 0, 0))
	r.steps(10, repeat(95, 9)...)
	r.expect()
	r.step(19, 95)
	r.expect(firing("cpu_high", 95, 10))
}

func TestPrune(t *testing.T) {
	r := newRig(t)
	r.records(0, repeat(95, 3)...)
	r.setRules(cpuRule(true, 50, 30))
	late := at(20).Add(8 * 24 * time.Hour)
	for _, ts := range []time.Time{late, late.Add(time.Minute)} {
		if err := r.store.Record("other", ts, stats.Sample{"cpu/total": 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.ev.SetRules("other", []stats.Rule{cpuRule(true, 50, 30)}); err != nil {
		t.Fatal(err)
	}
	keys := func() []string {
		var out []string
		for k := range r.state()["items"].(map[string]any) {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	if err := r.store.Forget(item, at(20)); err != nil {
		t.Fatal(err)
	}
	r.ev.Prune()
	if got := keys(); !slices.Equal(got, []string{item, "other"}) {
		t.Fatalf("forgotten but not swept: items = %v", got)
	}
	if err := r.store.Sweep(late); err != nil {
		t.Fatal(err)
	}
	r.ev.Prune()
	if got := keys(); !slices.Equal(got, []string{"other"}) {
		t.Fatalf("after sweep: items = %v", got)
	}
	if r.ev.Rules(item)[0].Threshold != 90 {
		t.Fatal("pruned item kept its rules")
	}
}
