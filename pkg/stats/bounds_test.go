package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFileSizeIsFunctionOfSeriesCount(t *testing.T) {
	s, dir := openTest(t)
	p := ringPath(dir, "x")
	smp := Sample{"a/1": 1, "a/2": 2, "b/3": 3}
	if err := s.Record("x", minuteAt(0), smp); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("x", minuteAt(1), smp); err != nil {
		t.Fatal(err)
	}
	want := int64(8192 + 40320*10)
	if got := ringFileSize(3); got != want || want != 411392 {
		t.Fatalf("ringFileSize(3)=%d", got)
	}
	if n := fileSize(t, p); n != want {
		t.Fatalf("after 1 minute: %d", n)
	}
	for i := 2; i <= 3*Minutes; i++ {
		if err := s.Record("x", minuteAt(i), smp); err != nil {
			t.Fatal(err)
		}
		if i%1000 == 0 {
			if n := fileSize(t, p); n != want {
				t.Fatalf("minute %d: %d", i, n)
			}
		}
	}
	if n := fileSize(t, p); n != want {
		t.Fatalf("after wrapping twice: %d", n)
	}
}

func TestMaxFileSize(t *testing.T) {
	if got := int64(MaxFileSize); got != 5854592 || got != ringFileSize(MaxSeries) {
		t.Fatalf("MaxFileSize=%d", got)
	}
	if got := ringFileSize(24); got != 2951552 {
		t.Fatalf("24 series: %d", got)
	}
	// Worst directory: 256 live + 256 forgotten items at the cap (2.8 GiB).
	if worst := int64(MaxItems+MaxForgotten) * MaxFileSize; worst != 2997551104 {
		t.Fatalf("worst directory %d", worst)
	}
	// A realistic 41 items of 24 series: about 115 MiB.
	if real := 41 * ringFileSize(24); real != 121013632 {
		t.Fatalf("41 items %d", real)
	}
}

func TestSeriesCap(t *testing.T) {
	s, dir := openTest(t)
	p := ringPath(dir, "x")
	smp := Sample{}
	for i := 0; i < 60; i++ {
		smp[fmt.Sprintf("c/k%02d", i)] = float64(i)
	}
	for i := 0; i <= 100; i++ {
		if err := s.Record("x", minuteAt(i), smp); !errors.Is(err, ErrSeriesCap) {
			t.Fatalf("minute %d: %v", i, err)
		}
		if i == 1 {
			if n := fileSize(t, p); n != MaxFileSize {
				t.Fatalf("size %d", n)
			}
		}
	}
	if n := fileSize(t, p); n != MaxFileSize {
		t.Fatalf("size after 100 minutes: %d", n)
	}
	if got := len(s.items["x"].ring.series()); got != MaxSeries {
		t.Fatalf("%d series stored", got)
	}
	cells := cellsOf(t, s, "x", 0, 99)
	for i := 0; i < 48; i++ {
		c := cells[fmt.Sprintf("c/k%02d", i)]
		if len(c) != 100 || !c[50].OK || c[50].Avg != float64(i) {
			t.Fatalf("series %d unreadable: %+v", i, c[50])
		}
	}
	if _, ok := cells["c/k48"]; ok {
		t.Fatal("series past the cap stored")
	}

	t.Run("bad key", func(t *testing.T) {
		s, _ := openTest(t)
		err := s.Record("y", minuteAt(0), Sample{strings.Repeat("k", 64): 1, "c/ok": 2})
		if !errors.Is(err, ErrBadKey) {
			t.Fatalf("%v", err)
		}
		if err := s.Record("y", minuteAt(1), Sample{"c/ok": 2}); err != nil {
			t.Fatal(err)
		}
		if got := s.items["y"].ring.series(); len(got) != 1 || got[0] != "c/ok" {
			t.Fatalf("series %v", got)
		}
	})
}

func TestItemCapDirectory(t *testing.T) {
	s, dir := openTest(t)
	smp := Sample{"a/b": 1}
	for i := 0; i < MaxItems; i++ {
		if err := s.Record(fmt.Sprintf("a%03d", i), minuteAt(0), smp); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < MaxItems; i++ { // commit the first minute: one series each
		if err := s.Record(fmt.Sprintf("a%03d", i), minuteAt(1), smp); err != nil {
			t.Fatal(err)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != MaxItems || ringFiles(t, dir) != MaxItems {
		t.Fatalf("%d entries, err %v", len(ents), err)
	}
	var total int64
	for _, e := range ents {
		total += fileSize(t, filepath.Join(dir, e.Name()))
	}
	if total != MaxItems*ringFileSize(1) || total > MaxItems*MaxFileSize {
		t.Fatalf("total %d", total)
	}
	if err := s.Record("one-more", minuteAt(1), smp); !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("item 257: %v", err)
	}
	for round := 0; round < 2; round++ {
		for i := 0; i < MaxItems; i++ {
			name := fmt.Sprintf("r%d-%03d", round, i)
			if round == 0 {
				name = fmt.Sprintf("a%03d", i)
			}
			if round == 1 {
				if err := s.Record(name, minuteAt(2), smp); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Forget(name, minuteAt(10+round*MaxItems+i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	ents, _ = os.ReadDir(dir)
	if n := ringFiles(t, dir); n != MaxForgotten || len(ents) > 2*MaxItems {
		t.Fatalf("%d rings, %d entries", n, len(ents))
	}
}

func TestMemoryBounded(t *testing.T) {
	g0 := runtime.NumGoroutine()
	dir := t.TempDir()
	clk := NewFakeClock(t0)
	s, err := open(dir, clk, noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	smp := Sample{}
	for i := 0; i < 24; i++ {
		smp[fmt.Sprintf("c/k%02d", i)] = float64(i)
	}
	var m1, m2 runtime.MemStats
	const calls = 20000
	for n := 0; n < calls; n++ {
		at := t0.Add(time.Duration(n/3) * 5 * time.Second)
		if err := s.Record(fmt.Sprintf("it%d", n%3), at, smp); err != nil {
			t.Fatal(err)
		}
		if n == calls/2 || n == calls-1 {
			m := &m1
			if n == calls-1 {
				m = &m2
			}
			runtime.GC()
			runtime.ReadMemStats(m)
		}
	}
	if grow := int64(m2.HeapAlloc) - int64(m1.HeapAlloc); grow >= 2<<20 {
		t.Fatalf("heap grew %d bytes", grow)
	}
	for name, e := range s.items {
		if len(e.live.stamp) > 180 || len(e.live.series) > MaxSeries {
			t.Fatalf("%s: %d slots, %d series", name, len(e.live.stamp), len(e.live.series))
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if g := runtime.NumGoroutine(); g != g0 {
		t.Fatalf("goroutines %d -> %d", g0, g)
	}
}

func TestBadFilesBounded(t *testing.T) {
	dir := t.TempDir()
	p := ringPath(dir, "x")
	for i := 0; i < 3; i++ {
		s, err := open(dir, NewFakeClock(t0), noSync{osFS})
		if err != nil {
			t.Fatal(err)
		}
		feedMinute(t, s, "x", minuteAt(i), constSample(Sample{"a/b": 1}))
		_ = s.Close()
		if err := os.Truncate(p, ringFileSize(1)-1); err != nil {
			t.Fatal(err)
		}
		s, err = open(dir, NewFakeClock(t0), noSync{osFS})
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Quarantined()) != 1 {
			t.Fatalf("round %d: quarantined %v", i, s.Quarantined())
		}
		_ = s.Close()
	}
	bad, _ := filepath.Glob(filepath.Join(dir, "*.bad"))
	if len(bad) != 1 {
		t.Fatalf("%v", bad)
	}
}

func TestAlertsFileBounded(t *testing.T) {
	s, dir := openTest(t)
	ev, err := newEvaluator(s, dir, nil, noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		at := minuteAt(0)
		if i >= 40 {
			at = minuteAt(8 * 24 * 60)
		}
		if err := s.Record(fmt.Sprintf("it%02d", i), at, Sample{"a/b": 1}); err != nil {
			t.Fatal(err)
		}
	}
	for n := 0; n < 200; n++ {
		r := []Rule{{Key: "cpu_high", Enabled: true, Threshold: float64(1 + n%50), Minutes: 1 + n%100}}
		if err := ev.SetRules(fmt.Sprintf("it%02d", n%50), r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Sweep(minuteAt(8*24*60 + 1)); err != nil {
		t.Fatal(err)
	}
	ev.Prune()
	b, err := os.ReadFile(filepath.Join(dir, "alerts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st stateFile
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Items) != 10 || len(b) >= 64<<10 {
		t.Fatalf("%d items, %d bytes", len(st.Items), len(b))
	}
}
