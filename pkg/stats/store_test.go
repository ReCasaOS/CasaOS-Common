package stats

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// failFS fails file writes while failWrite is set, and Remove/Rename of
// paths ending in failSuffix.
type failFS struct {
	fsys
	failWrite  *atomic.Bool
	failSuffix string
}

var errInjected = errors.New("injected failure")

func (f failFS) OpenFile(name string, flag int, perm os.FileMode) (file, error) {
	fl, err := f.fsys.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return failFile{fl, f.failWrite}, nil
}

func (f failFS) Remove(name string) error {
	if f.failSuffix != "" && strings.HasSuffix(name, f.failSuffix) {
		return errInjected
	}
	return f.fsys.Remove(name)
}

func (f failFS) Rename(a, b string) error {
	if f.failSuffix != "" && strings.HasSuffix(b, f.failSuffix) {
		return errInjected
	}
	return f.fsys.Rename(a, b)
}

type failFile struct {
	file
	failWrite *atomic.Bool
}

func (f failFile) WriteAt(b []byte, off int64) (int, error) {
	if f.failWrite != nil && f.failWrite.Load() {
		return 0, errInjected
	}
	return f.file.WriteAt(b, off)
}

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func minuteNo(i int) int64 { return minuteAt(i).Unix() / 60 }

// cellsOf reads minutes i..j (relative to t0) of item from its ring file.
func cellsOf(t *testing.T, s *Store, item string, i, j int) map[string][]minuteCell {
	t.Helper()
	c, err := s.items[item].ring.readWindow(minuteNo(i), minuteNo(j))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func constSample(smp Sample) func(int) Sample { return func(int) Sample { return smp } }

// valueThenNext gives key the value v inside the minute and next for the
// rollover sample (k == 12), which belongs to the following minute.
func valueThenNext(key string, v, next float64) func(int) Sample {
	return func(k int) Sample {
		if k == 12 {
			return Sample{key: next}
		}
		return Sample{key: v}
	}
}

func TestMinuteAverageMinMax(t *testing.T) {
	s, _ := openTest(t)
	feedMinute(t, s, "x", minuteAt(0), func(k int) Sample { return Sample{"cpu/user": float64(10 + k)} })
	c := cellsOf(t, s, "x", 0, 0)["cpu/user"][0]
	if !c.OK || c.Avg != 15.5 || c.Min != 10 || c.Max != 21 {
		t.Fatalf("got %+v", c)
	}
}

func TestSeriesAbsentInSomeSamples(t *testing.T) {
	s, _ := openTest(t)
	feedMinute(t, s, "x", minuteAt(0), func(k int) Sample {
		smp := Sample{"cpu/user": 1}
		if k < 12 && k%4 == 0 {
			smp["network/rx:eth0"] = float64(3 * (k/4 + 1)) // 3, 6, 9
		}
		return smp
	})
	feedMinute(t, s, "x", minuteAt(1), constSample(Sample{"cpu/user": 1}))
	rx := cellsOf(t, s, "x", 0, 1)["network/rx:eth0"]
	if c := rx[0]; !c.OK || c.Avg != 6 || c.Min != 3 || c.Max != 9 {
		t.Fatalf("minute 0: %+v", c)
	}
	if rx[1].OK {
		t.Fatalf("minute 1 should be a gap: %+v", rx[1])
	}
}

func TestCommitOnlyOnRollover(t *testing.T) {
	s, _ := openTest(t)
	var calls []time.Time
	s.SetOnMinute(func(item string, minute time.Time) {
		if item != "x" {
			t.Errorf("item %q", item)
		}
		calls = append(calls, minute)
	})
	for k := 0; k < 12; k++ {
		if err := s.Record("x", minuteAt(0).Add(time.Duration(k)*5*time.Second), Sample{"cpu/user": 1}); err != nil {
			t.Fatal(err)
		}
	}
	for k, cells := range cellsOf(t, s, "x", 0, 0) {
		if cells[0].OK {
			t.Fatalf("%s committed before rollover", k)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("calls before rollover: %v", calls)
	}
	if err := s.Record("x", minuteAt(1), Sample{"cpu/user": 2}); err != nil {
		t.Fatal(err)
	}
	if !cellsOf(t, s, "x", 0, 0)["cpu/user"][0].OK {
		t.Fatal("minute 0 not committed")
	}
	if len(calls) != 1 || !calls[0].Equal(minuteAt(0)) {
		t.Fatalf("calls=%v", calls)
	}
	// Nothing recorded during minute 2: no call for it, and the next Record
	// commits only minute 1.
	if err := s.Record("x", minuteAt(3), Sample{"cpu/user": 3}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !calls[1].Equal(minuteAt(1)) {
		t.Fatalf("calls=%v", calls)
	}
}

func TestPartialMinuteLostOnCloseIsOnlyLastMinute(t *testing.T) {
	dir := t.TempDir()
	s, err := open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		feedMinute(t, s, "x", minuteAt(i), valueThenNext("cpu/user", float64(i+1), float64(i+2)))
	}
	for k := 1; k < 7; k++ { // with the rollover sample of feedMinute: 7 pending
		if err := s.Record("x", minuteAt(3).Add(time.Duration(k)*5*time.Second), Sample{"cpu/user": 9}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := cellsOf(t, s, "x", 0, 3)["cpu/user"]
	for i := 0; i < 3; i++ {
		if !c[i].OK || c[i].Avg != float64(i+1) {
			t.Fatalf("minute %d: %+v", i, c[i])
		}
	}
	if c[3].OK {
		t.Fatalf("pending minute stored: %+v", c[3])
	}
}

func TestRecordRejectsBadSamples(t *testing.T) {
	s, _ := openTest(t)
	bad := []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1e300}
	feedMinute(t, s, "x", minuteAt(0), func(k int) Sample {
		return Sample{"cpu/user": bad[k%len(bad)], "cpu/system": 5, "cpu/iowait": -3}
	})
	c := cellsOf(t, s, "x", 0, 0)
	if c["cpu/user"] != nil && c["cpu/user"][0].OK {
		t.Fatalf("cpu/user stored: %+v", c["cpu/user"][0])
	}
	if v := c["cpu/system"][0]; !v.OK || v.Avg != 5 {
		t.Fatalf("cpu/system %+v", v)
	}
	if v := c["cpu/iowait"][0]; !v.OK || v.Avg != -3 || v.Min != -3 || v.Max != -3 {
		t.Fatalf("cpu/iowait %+v", v)
	}
	// The live ring gets the cleaned sample too: no Inf, no 1e300.
	_, _, live := s.items["x"].live.snapshot(minuteAt(1))
	for i, v := range live["cpu/user"] {
		if !math.IsNaN(v) {
			t.Fatalf("live cpu/user slot %d = %v", i, v)
		}
	}
	if v := live["cpu/system"][179]; v != 5 {
		t.Fatalf("live cpu/system %v", v)
	}
}

func TestClockStepBackwards(t *testing.T) {
	s, _ := openTest(t)
	for i := 0; i < 5; i++ {
		feedMinute(t, s, "x", minuteAt(i), valueThenNext("cpu/user", float64(i+1), float64(i+2)))
	}
	// 20 s earlier than the last sample: refused, nothing changes.
	last := minuteAt(5)
	if err := s.Record("x", last.Add(-20*time.Second), Sample{"cpu/user": 100}); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("err=%v", err)
	}
	if err := s.Record("x", last.Add(-time.Minute), Sample{"cpu/user": 100}); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("exactly 60 s back: err=%v", err)
	}
	_, _, live := s.items["x"].live.snapshot(last)
	if live["cpu/user"][179] != 6 || live["cpu/user"][175] != 5 {
		t.Fatalf("live changed: %v", live["cpu/user"][170:])
	}
	// Two hours earlier: a clock step. Accepted, pending and live dropped.
	back := minuteAt(-120)
	feedMinute(t, s, "x", back, valueThenNext("cpu/user", 7, 8))
	_, _, old := s.items["x"].live.snapshot(minuteAt(5))
	for i, v := range old["cpu/user"] {
		if !math.IsNaN(v) {
			t.Fatalf("pre-step live slot %d survived the reset: %v", i, v)
		}
	}
	_, _, live = s.items["x"].live.snapshot(back.Add(time.Minute))
	for i, v := range live["cpu/user"][:166] {
		if !math.IsNaN(v) {
			t.Fatalf("live not reset: slot %d = %v", i, v)
		}
	}
	feedMinute(t, s, "x", minuteAt(-119), constSample(Sample{"cpu/user": 8}))
	c := cellsOf(t, s, "x", -120, -119)["cpu/user"]
	if !c[0].OK || c[0].Avg != 7 || !c[1].OK || c[1].Avg != 8 {
		t.Fatalf("after step: %+v", c)
	}
	c = cellsOf(t, s, "x", 0, 5)["cpu/user"]
	for i := 0; i < 5; i++ {
		if !c[i].OK || c[i].Avg != float64(i+1) {
			t.Fatalf("minute %d corrupted: %+v", i, c[i])
		}
	}
	if c[5].OK {
		t.Fatalf("dropped pending minute stored: %+v", c[5])
	}
}

func TestClockJumpFrom1970(t *testing.T) {
	s, _ := openTest(t)
	early := time.Unix(0, 0).Add(3 * time.Hour)
	feedMinute(t, s, "x", early, constSample(Sample{"cpu/user": 1}))
	feedMinute(t, s, "x", minuteAt(0), constSample(Sample{"cpu/user": 2}))
	r := s.items["x"].ring
	c, err := r.readWindow(early.Unix()/60, early.Unix()/60)
	if err != nil || !c["cpu/user"][0].OK || c["cpu/user"][0].Avg != 1 {
		t.Fatalf("1970 minute: %+v %v", c, err)
	}
	if v := cellsOf(t, s, "x", 0, 0)["cpu/user"][0]; !v.OK || v.Avg != 2 {
		t.Fatalf("2026 minute: %+v", v)
	}
}

func TestRecordBefore1970DoesNotPanic(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Record("x", time.Unix(-3600, 0), Sample{"cpu/user": 1}); err != nil {
		t.Fatal(err)
	}
	feedMinute(t, s, "x", minuteAt(0), constSample(Sample{"cpu/user": 2}))
	if v := cellsOf(t, s, "x", 0, 0)["cpu/user"][0]; !v.OK || v.Avg != 2 {
		t.Fatalf("%+v", v)
	}
	if m, err := s.items["x"].ring.newestMinute(); err != nil || m != minuteNo(0) {
		t.Fatalf("newest minute %d %v", m, err)
	}
}

func TestRecordBefore1970CreatesNothing(t *testing.T) {
	s, dir := openTest(t)
	if err := s.Record("x", time.Unix(-3600, 0), Sample{"cpu/user": 1}); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 || len(s.Items()) != 0 {
		t.Fatalf("files=%v items=%q", ents, s.Items())
	}
}

func TestSeriesAppearsLater(t *testing.T) {
	s, _ := openTest(t)
	for i := 0; i < 5; i++ {
		smp := Sample{"cpu/user": 1}
		if i >= 3 {
			smp["disk/read"] = 4
		}
		feedMinute(t, s, "x", minuteAt(i), constSample(smp))
	}
	c := cellsOf(t, s, "x", 0, 4)["disk/read"]
	for i := 0; i < 3; i++ {
		if c[i].OK {
			t.Fatalf("minute %d not a gap: %+v", i, c[i])
		}
	}
	if !c[3].OK || c[3].Avg != 4 || !c[4].OK {
		t.Fatalf("%+v", c[3:])
	}
}

func TestSeriesCapAndBadKey(t *testing.T) {
	s, _ := openTest(t)
	smp := Sample{strings.Repeat("k", MaxKeyLen+1): 1}
	var want []string
	for i := 0; i < 2*MaxSeries; i++ {
		k := fmt.Sprintf("c/s%02d", i)
		smp[k] = 1
		if i < MaxSeries {
			want = append(want, k)
		}
	}
	err := s.Record("x", minuteAt(0), smp)
	if !errors.Is(err, ErrSeriesCap) || !errors.Is(err, ErrBadKey) {
		t.Fatalf("err=%v", err)
	}
	if err := s.Record("x", minuteAt(1), Sample{"c/s00": 1}); err != nil {
		t.Fatal(err)
	}
	if got := s.items["x"].ring.Keys(); !slices.Equal(got, want) {
		t.Fatalf("kept keys %q", got)
	}
}

// New keys spread over two samples of the same minute count against the cap
// together, so the commit never meets more than MaxSeries keys.
func TestSeriesCapCountsPendingKeys(t *testing.T) {
	s, _ := openTest(t)
	first, second := Sample{}, Sample{}
	for i := 0; i < 40; i++ {
		first[fmt.Sprintf("a/k%02d", i)] = 1
		second[fmt.Sprintf("b/k%02d", i)] = 1
	}
	if err := s.Record("x", minuteAt(0), first); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("x", minuteAt(0).Add(5*time.Second), second); !errors.Is(err, ErrSeriesCap) {
		t.Fatalf("err=%v", err)
	}
	if err := s.Record("x", minuteAt(1), Sample{"a/k00": 1}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n := len(s.items["x"].ring.Keys()); n != MaxSeries {
		t.Fatalf("series=%d", n)
	}
}

func TestCommitFailure(t *testing.T) {
	var fail atomic.Bool
	s, err := open(t.TempDir(), NewFakeClock(t0), failFS{fsys: noSync{osFS}, failWrite: &fail})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var calls []time.Time
	s.SetOnMinute(func(_ string, m time.Time) { calls = append(calls, m) })
	feedMinute(t, s, "x", minuteAt(0), constSample(Sample{"cpu/user": 1}))
	fail.Store(true)
	for k := 1; k < 12; k++ {
		if err := s.Record("x", minuteAt(1).Add(time.Duration(k)*5*time.Second), Sample{"cpu/user": 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Record("x", minuteAt(2), Sample{"cpu/user": 3}); !errors.Is(err, errInjected) {
		t.Fatalf("err=%v", err)
	}
	fail.Store(false)
	if len(calls) != 1 || !calls[0].Equal(minuteAt(0)) {
		t.Fatalf("calls=%v", calls)
	}
	_, _, live := s.items["x"].live.snapshot(minuteAt(2))
	if v := live["cpu/user"][179]; v != 3 {
		t.Fatalf("live slot after failed commit: %v", v)
	}
	// The failed minute is a gap and the dropped pending is not written later.
	feedMinute(t, s, "x", minuteAt(2), constSample(Sample{"cpu/user": 3}))
	c := cellsOf(t, s, "x", 0, 2)["cpu/user"]
	if !c[0].OK || c[1].OK || !c[2].OK || c[2].Avg != 3 {
		t.Fatalf("cells %+v", c)
	}
}

func TestHostileItemNames(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "stats")
	s, err := open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	names := []string{"../x", "a/b", "A", "a", "app:p:s:1", "été", strings.Repeat("z", MaxItemLen)}
	for i, n := range names {
		if err := s.Record(n, minuteAt(0), Sample{"cpu/user": 1}); err != nil {
			t.Fatalf("%q: %v", n, err)
		}
		ents, _ := os.ReadDir(dir)
		if len(ents) != i+1 {
			t.Fatalf("after %q: %d files", n, len(ents))
		}
	}
	for _, n := range []string{"", strings.Repeat("z", MaxItemLen+1)} {
		if err := s.Record(n, minuteAt(0), Sample{"cpu/user": 1}); !errors.Is(err, ErrBadItem) {
			t.Fatalf("len %d: %v", len(n), err)
		}
	}
	want := slices.Clone(names)
	slices.Sort(want)
	if got := s.Items(); !slices.Equal(got, want) {
		t.Fatalf("Items=%q", got)
	}
	if ents, _ := os.ReadDir(parent); len(ents) != 1 {
		t.Fatalf("files outside dir: %v", ents)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != len(names) {
		t.Fatalf("files in dir: %d", len(ents))
	}
}

func TestOpenQuarantine(t *testing.T) {
	dir := t.TempDir()
	ringFile := ringPath(dir, "x")
	garbage := []byte("not a ring file at all")
	write := func(p string, b []byte) {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(ringFile, garbage)
	write(ringFile+".bad", []byte("older quarantine"))
	write(filepath.Join(dir, "x.ring.tmp"), []byte("tmp"))
	foreign := []string{"alerts.json", "alerts.json.tmp", "alerts.json.bad", "notes.tmp"}
	for _, n := range foreign {
		write(filepath.Join(dir, n), []byte(n))
	}
	s, err := Open(dir, NewFakeClock(t0))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if q := s.Quarantined(); len(q) != 1 || q[0] != filepath.Base(ringFile) {
		t.Fatalf("Quarantined=%q", q)
	}
	if b, _ := os.ReadFile(ringFile + ".bad"); !bytes.Equal(b, garbage) {
		t.Fatalf(".bad holds %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.ring.tmp")); !os.IsNotExist(err) {
		t.Fatalf("leftover tmp: %v", err)
	}
	for _, n := range foreign {
		if b, _ := os.ReadFile(filepath.Join(dir, n)); string(b) != n {
			t.Fatalf("%s touched: %q", n, b)
		}
	}
	if len(s.Items()) != 0 {
		t.Fatalf("Items=%q", s.Items())
	}
	if err := s.Record("x", minuteAt(0), Sample{"cpu/user": 1}); err != nil {
		t.Fatal(err)
	}
}

// A quarantine that cannot be done skips that file; Open still succeeds.
func TestOpenQuarantineFailureSkipsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(ringPath(dir, "x"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := createRing(osFS, ringPath(dir, "y"), "y")
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	s, err := open(dir, NewFakeClock(t0), failFS{fsys: osFS, failSuffix: ".bad"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.Items(); !slices.Equal(got, []string{"y"}) {
		t.Fatalf("Items=%q", got)
	}
	if err := s.Record("x", minuteAt(0), Sample{"cpu/user": 1}); err != nil {
		t.Fatal(err)
	}
}

// A valid ring under another item's file name is not trusted either.
func TestOpenQuarantinesMisnamedRing(t *testing.T) {
	dir := t.TempDir()
	r, err := createRing(osFS, ringPath(dir, "x"), "y")
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	s, err := Open(dir, NewFakeClock(t0))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.Quarantined()) != 1 || len(s.Items()) != 0 {
		t.Fatalf("Quarantined=%q Items=%q", s.Quarantined(), s.Items())
	}
}

func TestItemCap(t *testing.T) {
	s, dir := openTest(t)
	for i := 0; i < MaxItems; i++ {
		if err := s.Record(fmt.Sprintf("item%d", i), minuteAt(0), Sample{"a/b": 1}); err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	if err := s.Record("one-too-many", minuteAt(0), Sample{"a/b": 1}); !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("err=%v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != MaxItems {
		t.Fatalf("files=%d", len(ents))
	}
}

func TestCloseThenRecord(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Record("x", minuteAt(0), Sample{"a/b": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("x", minuteAt(0), Sample{"a/b": 1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("err=%v", err)
	}
	if err := s.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second close: %v", err)
	}
}
