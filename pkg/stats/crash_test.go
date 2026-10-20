package stats

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const crashItem = "app:demo:web:1"

// crashKeys lists the scenario series in index order; minute 3 adds the third.
var crashKeys = []string{"cpu/user", "memory/rss", "disk/read"}

// crashValue is the value written for series index i in minute m.
func crashValue(m, i int) float64 { return float64(m*10 + i + 1) }

// crashSeries is how many series minute m holds.
func crashSeries(m int) int {
	if m == 3 {
		return 3
	}
	return 2
}

// crashScenario opens a store in dir, commits minutes 0..2 with two series,
// minute 3 with a third, then forgets the item. *progress is the number of
// minutes whose commit returned.
func crashScenario(t *testing.T, fs fsys, dir string, progress *int) {
	crashScenarioAt(t, fs, dir, 0, progress)
}

// crashScenarioAt is crashScenario with minute m at minuteAt(start+m).
func crashScenarioAt(t *testing.T, fs fsys, dir string, start int, progress *int) {
	s, err := open(dir, NewFakeClock(t0), fs)
	if err != nil {
		t.Fatal(err)
	}
	s.SetOnMinute(func(string, time.Time) { *progress++ })
	for m := 0; m <= 3; m++ {
		smp := Sample{}
		for i := 0; i < crashSeries(m); i++ {
			smp[crashKeys[i]] = crashValue(m, i)
		}
		for k := 0; k < 12; k++ {
			if err := s.Record(crashItem, minuteAt(start+m).Add(time.Duration(k)*5*time.Second), smp); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Record(crashItem, minuteAt(start+4), Sample{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(crashItem, minuteAt(start+4)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// runFaulted runs the scenario and reports whether a fault killed it.
func runFaulted(t *testing.T, ff *faultFS, dir string, start int, progress *int) (killed bool) {
	defer func() {
		if r := recover(); r != nil {
			if r != errKilled {
				panic(r)
			}
			killed = true
		}
		ff.crash()
	}()
	crashScenarioAt(t, ff, dir, start, progress)
	return false
}

// checkAfterCrash reopens dir on the real filesystem and checks what the
// scenario left: committed minutes whole, the minute in flight all or
// nothing, nothing else, no quarantine, and a store that still works. With
// old, the slots held minutes start-Minutes.. (see prefillOld): each of them
// reads whole or as a gap, never with values of the new minute.
func checkAfterCrash(t *testing.T, dir string, start int, old bool, progress int) (served int) {
	t.Helper()
	s, err := Open(dir, NewFakeClock(minuteAt(start+13)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if q := s.Quarantined(); len(q) != 0 {
		t.Fatalf("quarantined %q", q)
	}
	ents, _ := os.ReadDir(dir)
	for _, d := range ents {
		if strings.HasSuffix(d.Name(), ".ring.tmp") {
			t.Fatalf("leftover %s", d.Name())
		}
	}
	present := map[int]int{} // minute -> series present
	if slices.Contains(s.Items(), crashItem) {
		// 1h has one point per minute; 7d folds minutes 0..3 into one bucket.
		res, err := s.Query(crashItem, Range1h)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		for _, sd := range res.Series {
			i := slices.Index(crashKeys, sd.Chart+"/"+sd.Name)
			for _, p := range sd.Points {
				if p.Avg == nil {
					continue
				}
				m := int((p.T-t0.Unix())/60) - start
				if i < 0 || m < 0 || m > 3 || i >= crashSeries(m) {
					t.Fatalf("unexpected point %s minute %d", sd.Name, m)
				}
				if want := crashValue(m, i); *p.Avg != want || *p.Min != want || *p.Max != want {
					t.Fatalf("%s minute %d = %v/%v/%v, want %v", crashKeys[i], m, *p.Avg, *p.Min, *p.Max, want)
				}
				present[m]++
			}
		}
	} else if progress > 0 {
		t.Fatalf("item lost after %d committed minutes", progress)
	}
	for m := 0; m <= 3; m++ {
		switch {
		case m < progress && present[m] != crashSeries(m):
			t.Fatalf("committed minute %d has %d series", m, present[m])
		case m == progress && present[m] != 0 && present[m] != crashSeries(m):
			t.Fatalf("minute %d in flight has %d of %d series", m, present[m], crashSeries(m))
		case m > progress && present[m] != 0:
			t.Fatalf("minute %d never committed has data", m)
		}
	}
	if old && slices.Contains(s.Items(), crashItem) {
		cells := cellsOf(t, s, crashItem, start-Minutes, start-Minutes+3)
		for i, k := range crashKeys[:2] {
			for j, c := range cells[k] {
				if c.OK && (c.Avg != oldValue(j, i) || c.Min != oldValue(j, i) || c.Max != oldValue(j, i)) {
					t.Fatalf("old minute %d of %s served as %+v", j, k, c)
				}
				if c.OK {
					served++
				}
				if c.OK && j < progress {
					t.Fatalf("old minute %d of %s still served after its slot was reused", j, k)
				}
			}
		}
	}
	feedMinute(t, s, crashItem, minuteAt(start+10), constSample(Sample{"cpu/user": 7}))
	res, err := s.Query(crashItem, Range1h)
	if err != nil {
		t.Fatal(err)
	}
	for _, sd := range res.Series {
		if sd.Name == "user" {
			if p := sd.Points[len(sd.Points)-3]; p.T != minuteAt(start+10).Unix() || p.Avg == nil || *p.Avg != 7 {
				t.Fatalf("new minute not readable: %+v", p)
			}
			return served
		}
	}
	t.Fatal("new minute not readable")
	return served
}

// oldValue is the value prefillOld writes for series i in old minute j.
func oldValue(j, i int) float64 { return float64(900 + j*10 + i) }

// reuseStart is a start minute whose scenario minutes wrap the ring: minute 0
// lands in slot Minutes-2, minute 2 in slot 0.
func reuseStart() int {
	return int(((Minutes-2-minuteNo(0))%Minutes + Minutes) % Minutes)
}

// prefillOld commits minutes start-Minutes..+3 (two series) in dir, so that
// the scenario starting at start overwrites their slots.
func prefillOld(t *testing.T, dir string, start int) {
	t.Helper()
	s, err := open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	for j := 0; j < 4; j++ {
		smp := Sample{crashKeys[0]: oldValue(j, 0), crashKeys[1]: oldValue(j, 1)}
		for k := 0; k < 12; k++ {
			if err := s.Record(crashItem, minuteAt(start-Minutes+j).Add(time.Duration(k)*5*time.Second), smp); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Record(crashItem, minuteAt(start-Minutes+4), Sample{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// faultLoop runs the scenario once per fault position n = 1.. (fault sets
// it) until a run completes, checking after each, and returns the number of
// positions that faulted and the number of old cells served over all runs.
func faultLoop(t *testing.T, start int, old bool, fault func(ff *faultFS, n int)) (n, served int) {
	t.Helper()
	for n = 1; n <= 1000; n++ {
		dir := t.TempDir()
		if old {
			prefillOld(t, dir, start)
		}
		ff := &faultFS{fsys: noSync{osFS}}
		fault(ff, n)
		progress := 0
		killed := runFaulted(t, ff, dir, start, &progress)
		served += checkAfterCrash(t, dir, start, old, progress)
		if !killed {
			return n - 1, served
		}
	}
	t.Fatal("still faulting after 1000 positions")
	return 0, 0
}

func killAt(ff *faultFS, n int) { ff.killAt = n }
func tornAt(ff *faultFS, n int) { ff.tornAt = n }

func TestKillAtEveryWriteStep(t *testing.T) {
	n, _ := faultLoop(t, 0, false, killAt)
	if n < 40 {
		t.Fatalf("only %d write steps", n)
	}
	t.Logf("killed at each of %d write steps", n)
}

func TestTornWriteAtEveryWriteStep(t *testing.T) {
	// creation image, then a column image and a header per series (3), then Forget's header
	if n, _ := faultLoop(t, 0, false, tornAt); n != 8 {
		t.Fatalf("%d large writes, want 8", n)
	}
}

// The same loops over slots that hold minutes of 7 days ago.
func TestKillAtEveryWriteStepReusedSlots(t *testing.T) {
	if slotOf(minuteNo(reuseStart())) != Minutes-2 {
		t.Fatal("reuseStart")
	}
	n, served := faultLoop(t, reuseStart(), true, killAt)
	if n == 0 || served == 0 {
		t.Fatalf("kill loop: %d faults, %d old cells served", n, served)
	}
	t.Logf("killed at each of %d write steps", n)
	n, served = faultLoop(t, reuseStart(), true, tornAt)
	if n == 0 || served == 0 {
		t.Fatalf("torn loop: %d faults, %d old cells served", n, served)
	}
}

// createRing writes its image to a temp file, syncs it, then renames it.
func TestCreateRingOrder(t *testing.T) {
	ff := &faultFS{fsys: noSync{osFS}}
	p := ringPath(t.TempDir(), "i")
	r, err := createRing(ff, p, "i")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tmp := filepath.Base(p) + ".tmp"
	want := []string{"open " + tmp, "write ", "sync ", "rename " + tmp}
	if got := opKinds(ff.trace); !slices.Equal(got, want) {
		t.Fatalf("ops %q, want %q", got, want)
	}
}

// commitTrace returns the operations of the commit of minute 1 of a store
// that already holds both series.
func commitTrace(t *testing.T) []fop {
	t.Helper()
	ff := &faultFS{fsys: noSync{osFS}}
	s, err := open(t.TempDir(), NewFakeClock(t0), ff)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	smp := constSample(Sample{"cpu/user": 1, "memory/rss": 2})
	feedMinute(t, s, crashItem, minuteAt(0), smp)
	for k := 1; k < 12; k++ {
		if err := s.Record(crashItem, minuteAt(1).Add(time.Duration(k)*5*time.Second), smp(k)); err != nil {
			t.Fatal(err)
		}
	}
	ff.trace = nil
	if err := s.Record(crashItem, minuteAt(2), Sample{}); err != nil {
		t.Fatal(err)
	}
	return ff.trace
}

func TestStampIsWrittenLast(t *testing.T) {
	tr := commitTrace(t)
	if len(tr) < 3 {
		t.Fatalf("trace %+v", tr)
	}
	last := tr[len(tr)-1]
	stamp := dataStart + slotOf(minuteNo(1))*4
	if last.kind != "write" || last.off != stamp || last.n != 4 {
		t.Fatalf("last op %+v, want the stamp write at %d", last, stamp)
	}
	if tr[len(tr)-2].kind != "sync" {
		t.Fatalf("no sync before the stamp: %+v", tr)
	}
	for _, o := range tr[:len(tr)-1] {
		if o.kind == "write" && o.off >= dataStart+colBytes && o.n != 4 {
			t.Fatalf("value write %+v", o)
		}
	}
}

func TestHeaderUpdateOrder(t *testing.T) {
	ff := &faultFS{fsys: noSync{osFS}}
	s, err := open(t.TempDir(), NewFakeClock(t0), ff)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	feedMinute(t, s, crashItem, minuteAt(0), constSample(Sample{"cpu/user": 1}))
	for k := 1; k < 12; k++ {
		_ = s.Record(crashItem, minuteAt(1).Add(time.Duration(k)*5*time.Second), Sample{"cpu/user": 1, "memory/rss": 2})
	}
	ff.trace = nil
	if err := s.Record(crashItem, minuteAt(2), Sample{}); err != nil {
		t.Fatal(err)
	}
	// memory/rss is series 1: column image at ringFileSize(1), then the header.
	want := []fop{
		{kind: "write", off: ringFileSize(1), n: 3 * colBytes},
		{kind: "sync"},
	}
	if len(ff.trace) < 4 || !slices.Equal(ff.trace[:2], want) {
		t.Fatalf("trace %+v", ff.trace)
	}
	h := ff.trace[2]
	if h.kind != "write" || (h.off != 0 && h.off != headerSize) || h.n != headerSize || ff.trace[3].kind != "sync" {
		t.Fatalf("header write %+v then %+v", h, ff.trace[3])
	}
}

// threeMinutes commits minutes 0..2 of crashItem (cpu/user = 1, 2, 3) in dir.
func threeMinutes(t *testing.T, dir string, keys ...string) {
	t.Helper()
	s, err := open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	for m := 0; m < 3; m++ {
		smp := Sample{}
		for _, k := range keys {
			smp[k] = float64(m + 1)
		}
		for k := 0; k < 12; k++ {
			if err := s.Record(crashItem, minuteAt(m).Add(time.Duration(k)*5*time.Second), smp); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Record(crashItem, minuteAt(3), Sample{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// userAvgs returns the cpu/user averages of minutes 0..2 (NaN for a gap).
func userAvgs(t *testing.T, s *Store) []float64 {
	t.Helper()
	res, err := s.Query(crashItem, Range1h)
	if err != nil {
		t.Fatal(err)
	}
	out := []float64{noVal, noVal, noVal}
	for _, sd := range res.Series {
		if sd.Name != "user" {
			continue
		}
		for _, p := range sd.Points {
			if m := int((p.T - t0.Unix()) / 60); m >= 0 && m < 3 && p.Avg != nil {
				out[m] = *p.Avg
			}
		}
	}
	return out
}

var noVal = -1.0

func TestCorruptedRegions(t *testing.T) {
	t.Run("stamp byte", func(t *testing.T) {
		dir := t.TempDir()
		threeMinutes(t, dir, "cpu/user")
		patch(t, ringPath(dir, crashItem), dataStart+slotOf(minuteNo(1))*4, []byte{0xFF})
		s, err := Open(dir, NewFakeClock(minuteAt(5)))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if got := userAvgs(t, s); !slices.Equal(got, []float64{1, noVal, 3}) {
			t.Fatalf("minutes %v", got)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		dir := t.TempDir()
		threeMinutes(t, dir, "cpu/user", "memory/rss")
		p := ringPath(dir, crashItem)
		if err := os.Truncate(p, ringFileSize(1)); err != nil {
			t.Fatal(err)
		}
		trunc, _ := os.ReadFile(p)
		s, err := Open(dir, NewFakeClock(minuteAt(5)))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if q := s.Quarantined(); !slices.Equal(q, []string{filepath.Base(p)}) {
			t.Fatalf("quarantined %q", q)
		}
		if b, _ := os.ReadFile(p + ".bad"); !bytes.Equal(b, trunc) {
			t.Fatal(".bad differs from the truncated file")
		}
		feedMinute(t, s, crashItem, minuteAt(3), constSample(Sample{"cpu/user": 4}))
		if got := userAvgs(t, s); !slices.Equal(got, []float64{noVal, noVal, noVal}) {
			t.Fatalf("new ring holds old minutes %v", got)
		}
		if n := fileSize(t, p); n != ringFileSize(1) {
			t.Fatalf("new ring is %d bytes", n)
		}
	})
	t.Run("future version", func(t *testing.T) {
		dir := t.TempDir()
		threeMinutes(t, dir, "cpu/user")
		p := ringPath(dir, crashItem)
		withHeader(t, p, func(h *header) { h.version = 2 })
		orig, _ := os.ReadFile(p)
		s, err := Open(dir, NewFakeClock(minuteAt(5)))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if q := s.Quarantined(); len(q) != 1 {
			t.Fatalf("quarantined %q", q)
		}
		if b, _ := os.ReadFile(p + ".bad"); !bytes.Equal(b, orig) {
			t.Fatal(".bad differs from the original")
		}
	})
	t.Run("value byte", func(t *testing.T) {
		// Documented limit: values carry no CRC, a flipped value bit is served.
		dir := t.TempDir()
		threeMinutes(t, dir, "cpu/user")
		patch(t, ringPath(dir, crashItem), colOff(0, 0)+slotOf(minuteNo(1))*4, []byte{0x01})
		s, err := Open(dir, NewFakeClock(minuteAt(5)))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		got := userAvgs(t, s)
		if got[0] != 1 || got[2] != 3 || got[1] == 2 || got[1] == noVal {
			t.Fatalf("minutes %v", got)
		}
		feedMinute(t, s, crashItem, minuteAt(3), constSample(Sample{"cpu/user": 4}))
	})
}

// diskFullRig records minutes 0..1 of cpu/user (1, 2) and the samples of
// minute 2 (value 3, plus extra), then arms the fault for the commit.
func diskFullRig(t *testing.T, extra Sample) (*Store, *faultFS, *[]time.Time) {
	t.Helper()
	ff := &faultFS{fsys: noSync{osFS}, failErr: syscall.ENOSPC}
	s, err := open(t.TempDir(), NewFakeClock(minuteAt(6)), ff)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	calls := &[]time.Time{}
	s.SetOnMinute(func(_ string, m time.Time) { *calls = append(*calls, m) })
	feedMinute(t, s, crashItem, minuteAt(0), valueThenNext("cpu/user", 1, 2))
	smp := Sample{"cpu/user": 3}
	for k, v := range extra {
		smp[k] = v
	}
	for k := 1; k < 12; k++ {
		at := minuteAt(1).Add(time.Duration(k) * 5 * time.Second)
		if err := s.Record(crashItem, at, Sample{"cpu/user": 2}); err != nil {
			t.Fatal(err)
		}
	}
	for k := 0; k < 12; k++ {
		if err := s.Record(crashItem, minuteAt(2).Add(time.Duration(k)*5*time.Second), smp); err != nil {
			t.Fatal(err)
		}
	}
	ff.failFrom = ff.ops + 1
	return s, ff, calls
}

func TestDiskFullDuringCommit(t *testing.T) {
	commitFails := func(t *testing.T, s *Store, ff *faultFS, calls *[]time.Time) {
		t.Helper()
		err := s.Record(crashItem, minuteAt(3), Sample{"cpu/user": 4})
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("Record: %v", err)
		}
		if len(*calls) != 2 {
			t.Fatalf("OnMinute calls %v", *calls)
		}
		_, _, live := s.items[crashItem].live.snapshot(minuteAt(3))
		if v := live["cpu/user"][LiveSlots-1]; v != 4 {
			t.Fatalf("live slot during the failure: %v", v)
		}
		ff.failFrom = 0
		for k := 1; k < 12; k++ {
			_ = s.Record(crashItem, minuteAt(3).Add(time.Duration(k)*5*time.Second), Sample{"cpu/user": 4, "disk/read": 9})
		}
		if err := s.Record(crashItem, minuteAt(4), Sample{}); err != nil {
			t.Fatal(err)
		}
		if len(*calls) != 3 || !(*calls)[2].Equal(minuteAt(3)) {
			t.Fatalf("OnMinute calls %v", *calls)
		}
		if got := userAvgs(t, s); !slices.Equal(got, []float64{1, 2, noVal}) {
			t.Fatalf("minutes %v", got)
		}
		c := cellsOf(t, s, crashItem, 3, 3)
		if !c["cpu/user"][0].OK || c["cpu/user"][0].Avg != 4 || !c["disk/read"][0].OK {
			t.Fatalf("minute 3 after the fault: %+v", c)
		}
	}
	t.Run("minute write", func(t *testing.T) {
		s, ff, calls := diskFullRig(t, nil)
		commitFails(t, s, ff, calls)
	})
	t.Run("addSeries", func(t *testing.T) {
		s, ff, calls := diskFullRig(t, Sample{"disk/read": 5})
		ring := s.items[crashItem].ring
		err := s.Record(crashItem, minuteAt(3), Sample{"cpu/user": 4})
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("Record: %v", err)
		}
		if len(*calls) != 2 {
			t.Fatalf("OnMinute calls %v", *calls)
		}
		_, _, live := s.items[crashItem].live.snapshot(minuteAt(3))
		if v := live["cpu/user"][LiveSlots-1]; v != 4 {
			t.Fatalf("live slot during the failure: %v", v)
		}
		if slices.Contains(ring.Keys(), "disk/read") {
			t.Fatal("series registered by a failed commit")
		}
		// The rest as for a minute write, the series is added at the next commit.
		ff.failFrom = 0
		for k := 1; k < 12; k++ {
			_ = s.Record(crashItem, minuteAt(3).Add(time.Duration(k)*5*time.Second), Sample{"cpu/user": 4, "disk/read": 9})
		}
		if err := s.Record(crashItem, minuteAt(4), Sample{}); err != nil {
			t.Fatal(err)
		}
		if len(*calls) != 3 || !slices.Contains(ring.Keys(), "disk/read") {
			t.Fatalf("calls %v keys %v", *calls, ring.Keys())
		}
		if got := userAvgs(t, s); !slices.Equal(got, []float64{1, 2, noVal}) {
			t.Fatalf("minutes %v", got)
		}
	})
	t.Run("createRing", func(t *testing.T) {
		ff := &faultFS{fsys: noSync{osFS}, failErr: syscall.ENOSPC}
		dir := t.TempDir()
		s, err := open(dir, NewFakeClock(minuteAt(6)), ff)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		ff.failFrom = ff.ops + 2 // the temp file opens, its image does not fit
		if err := s.Record(crashItem, minuteAt(0), Sample{"cpu/user": 1}); !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("Record: %v", err)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Fatalf("left %v", ents)
		}
		ff.failFrom = 0
		feedMinute(t, s, crashItem, minuteAt(0), constSample(Sample{"cpu/user": 1}))
		if got := userAvgs(t, s); !slices.Equal(got, []float64{1, noVal, noVal}) {
			t.Fatalf("minutes %v", got)
		}
	})
}

// faultEvaluator opens a store and an evaluator writing through ff.
func faultEvaluator(t *testing.T, dir string, ff *faultFS) *Evaluator {
	t.Helper()
	s, err := open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ev, err := newEvaluator(s, dir, nil, ff)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

var (
	oldRules = []Rule{{"cpu_high", true, 50, 10}, {"mem_high", true, 90, 5}}
	newRules = []Rule{{"cpu_high", true, 60, 10}, {"mem_high", true, 90, 5}}
)

func TestEvaluatorStateKillAtEveryStep(t *testing.T) {
	n := 1
	for ; ; n++ {
		dir := t.TempDir()
		ff := &faultFS{fsys: noSync{osFS}}
		ev := faultEvaluator(t, dir, ff)
		if err := ev.SetRules(crashItem, oldRules); err != nil {
			t.Fatal(err)
		}
		ff.killAt = ff.ops + n
		killed := func() (killed bool) {
			defer func() {
				if r := recover(); r != nil {
					if r != errKilled {
						panic(r)
					}
					killed = true
				}
				ff.crash()
			}()
			_ = ev.SetRules(crashItem, newRules)
			return false
		}()
		s, err := Open(t.TempDir(), NewFakeClock(t0))
		if err != nil {
			t.Fatal(err)
		}
		ev2, err := NewEvaluator(s, dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := ev2.Rules(crashItem); !slices.Equal(got, oldRules) && !slices.Equal(got, newRules) {
			t.Fatalf("kill at %d: rules %+v", n, got)
		}
		if _, err := os.Stat(filepath.Join(dir, "alerts.json.bad")); err == nil {
			t.Fatalf("kill at %d: state quarantined", n)
		}
		_ = s.Close()
		if !killed {
			if got := ev2.Rules(crashItem); !slices.Equal(got, newRules) {
				t.Fatalf("finished save: rules %+v", got)
			}
			break
		}
	}
	if n-1 != 4 {
		t.Fatalf("%d steps in a save, want 4", n-1)
	}
}

func opKinds(tr []fop) []string {
	out := make([]string, len(tr))
	for i, o := range tr {
		out[i] = o.kind + " " + o.name
	}
	return out
}

func TestEvaluatorSaveOrder(t *testing.T) {
	ff := &faultFS{fsys: noSync{osFS}}
	ev := faultEvaluator(t, t.TempDir(), ff)
	if err := ev.SetRules(crashItem, oldRules); err != nil {
		t.Fatal(err)
	}
	want := []string{"open alerts.json.tmp", "write ", "sync ", "rename alerts.json.tmp"}
	if got := opKinds(ff.trace); !slices.Equal(got, want) {
		t.Fatalf("ops %q, want %q", got, want)
	}
}

func TestEvaluatorSaveFailure(t *testing.T) {
	dir := t.TempDir()
	ff := &faultFS{fsys: noSync{osFS}, failErr: syscall.ENOSPC}
	ev := faultEvaluator(t, dir, ff)
	if err := ev.SetRules(crashItem, oldRules); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "alerts.json")
	before, _ := os.ReadFile(path)
	ff.failFrom = ff.ops + 4 // the rename
	if err := ev.SetRules(crashItem, newRules); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ev.LastError(), syscall.ENOSPC) {
		t.Fatalf("LastError %v", ev.LastError())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("alerts.json.tmp left: %v", err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, before) {
		t.Fatalf("alerts.json touched: %s", b)
	}
	// The same state again is still written: the failed save is retried.
	ff.failFrom = 0
	if err := ev.SetRules(crashItem, newRules); err != nil {
		t.Fatal(err)
	}
	var sf stateFile
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &sf); err != nil || sf.Items[crashItem] == nil || !slices.Equal(sf.Items[crashItem].Rules, newRules) {
		t.Fatalf("alerts.json after retry: %s (%v)", b, err)
	}
}
