package stats

import (
	"fmt"
	"testing"
	"time"
)

const benchItems = 50

// benchKeys are the 24 series of a typical container.
var benchKeys = []string{
	"cpu/user", "cpu/system", "cpu/iowait", "cpu/throttled",
	"memory/rss", "memory/cache", "memory/swap", "memory/limit",
	"network/rx:eth0", "network/tx:eth0", "network/dropped:eth0",
	"network/rx:eth1", "network/tx:eth1", "network/dropped:eth1",
	"disk/read", "disk/write", "disk/iops_read", "disk/iops_write",
	"pids/count", "pids/fds", "restarts/count", "health/ok",
	"alert/cpu_high", "alert/mem_high",
}

// tickRig is 50 items fed every 5 s from a FakeClock.
type tickRig struct {
	s     *Store
	clk   *FakeClock
	ev    *Evaluator
	smp   []Sample
	ticks int

	hookWorst, hookTotal time.Duration // time spent in Evaluator.OnMinute (it holds e.mu through marshal and fsync)
	hookCalls            int
}

// newTickRig creates the items (one Record each, before any timing). With
// withEv, the Evaluator is wired as the store hook and 20 items run an alert
// value above its threshold.
func newTickRig(tb testing.TB, fs fsys, withEv bool) *tickRig {
	tb.Helper()
	clk := NewFakeClock(t0)
	s, err := open(tb.TempDir(), clk, fs)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	r := &tickRig{s: s, clk: clk, smp: make([]Sample, benchItems)}
	if withEv {
		if r.ev, err = newEvaluator(s, tb.TempDir(), func(Event) {}, fs); err != nil {
			tb.Fatal(err)
		}
		s.SetOnMinute(func(item string, minute time.Time) {
			t := time.Now()
			r.ev.OnMinute(item, minute)
			d := time.Since(t)
			r.hookWorst, r.hookTotal, r.hookCalls = max(r.hookWorst, d), r.hookTotal+d, r.hookCalls+1
		})
	}
	for i := range r.smp {
		r.smp[i] = Sample{}
		for j, k := range benchKeys {
			r.smp[i][k] = float64(1 + (i+j)%50)
		}
		if err := s.Record(fmt.Sprintf("item%02d", i), clk.Now(), r.smp[i]); err != nil {
			tb.Fatal(err)
		}
	}
	return r
}

// tick advances 5 s and records one sample for each item. Items 0..19 are
// above the default alert thresholds, except one minute in a hundred so that
// their run and state change at every committed minute.
func (r *tickRig) tick(tb testing.TB) {
	r.clk.Advance(5 * time.Second)
	r.ticks++
	high := 95.0
	if (r.ticks/12)%100 == 99 {
		high = 1
	}
	for i, smp := range r.smp {
		if i < 20 {
			smp["alert/cpu_high"], smp["alert/mem_high"] = high, high
		} else {
			smp["alert/cpu_high"], smp["alert/mem_high"] = 1, 1
		}
		if err := r.s.Record(fmt.Sprintf("item%02d", i), r.clk.Now(), smp); err != nil {
			tb.Fatal(err)
		}
	}
}

// run times n ticks and returns the mean and the worst tick. Wall time is
// only measured here, never used for the data.
func (r *tickRig) run(tb testing.TB, n int) (mean, worst time.Duration) {
	var total time.Duration
	for i := 0; i < n; i++ {
		t := time.Now()
		r.tick(tb)
		d := time.Since(t)
		total += d
		worst = max(worst, d)
	}
	return total / time.Duration(n), worst
}

func BenchmarkRecordTick(b *testing.B) {
	for _, withEv := range []bool{false, true} {
		for _, v := range []struct {
			name string
			fs   fsys
		}{{"nofsync", noSync{osFS}}, {"fsync", osFS}} {
			name := v.name
			if withEv {
				name = "evaluator/" + name
			}
			b.Run(name, func(b *testing.B) {
				r := newTickRig(b, v.fs, withEv)
				b.ReportAllocs()
				b.ResetTimer()
				mean, worst := r.run(b, b.N)
				b.StopTimer()
				b.ReportMetric(float64(worst.Nanoseconds()), "worst-tick-ns")
				b.ReportMetric(float64(mean.Nanoseconds())/5e9*100, "cpu-percent-of-core")
				if r.hookCalls > 0 {
					b.ReportMetric(float64(r.hookTotal.Nanoseconds())/float64(r.hookCalls), "onminute-ns")
					b.ReportMetric(float64(r.hookWorst.Nanoseconds()), "worst-onminute-ns")
				}
			})
		}
	}
}

// BenchmarkMinuteValue is the cost of the Evaluator reading one alert value
// (it reads the minute of every series of the item).
func BenchmarkMinuteValue(b *testing.B) {
	r := newTickRig(b, noSync{osFS}, false)
	for i := 0; i < 24; i++ {
		r.tick(b)
	}
	m := r.clk.Now().Unix()/60 - 1
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, err := r.s.minuteValue("item00", "alert/cpu_high", m); err != nil || !ok {
			b.Fatal(ok, err)
		}
	}
}

func TestRecordTickBudget(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("timing budget: skipped under -short and -race")
	}
	const ticks = 720 // one simulated hour: 60 heavy ticks
	for _, withEv := range []bool{false, true} {
		for _, fsync := range []bool{false, true} {
			name := fmt.Sprintf("evaluator=%v/fsync=%v", withEv, fsync)
			t.Run(name, func(t *testing.T) {
				var fs fsys = noSync{osFS}
				if fsync {
					fs = osFS
				}
				r := newTickRig(t, fs, withEv)
				mean, worst := r.run(t, ticks)
				t.Logf("mean tick %v, worst tick %v", mean, worst)
				if withEv {
					t.Logf("OnMinute: %d calls, mean %v, worst %v", r.hookCalls, r.hookTotal/time.Duration(r.hookCalls), r.hookWorst)
				}
				meanMax := 5 * time.Millisecond // 20 % of the 25 ms that 0.5 % of a core allows per 5 s
				if withEv {
					meanMax = 25 * time.Millisecond // the Evaluator is part of the same service budget
				}
				switch {
				case fsync && worst >= 2*time.Second:
					t.Errorf("worst tick %v >= 2s (mean %v)", worst, mean)
				case !fsync && mean >= meanMax:
					t.Errorf("mean tick %v >= %v (worst %v)", mean, meanMax, worst)
				case !fsync && worst >= 250*time.Millisecond:
					t.Errorf("worst tick %v >= 250ms (mean %v)", worst, mean)
				}
			})
		}
	}
}
