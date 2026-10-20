package stats

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// feedEach records smp(i) once at the start of each minute from..to: minutes
// from..to-1 get committed, minute to stays pending.
func feedEach(t *testing.T, s *Store, item string, from, to int, smp func(i int) Sample) {
	t.Helper()
	for i := from; i <= to; i++ {
		if err := s.Record(item, minuteAt(i), smp(i)); err != nil {
			t.Fatalf("record minute %d: %v", i, err)
		}
	}
}

func setNow(s *Store, at time.Time) { s.clock.(*FakeClock).Set(at) }

func query(t *testing.T, s *Store, item string, r Range) Result {
	t.Helper()
	res, err := s.Query(item, r)
	if err != nil {
		t.Fatalf("query %s: %v", r, err)
	}
	return res
}

func seriesOf(t *testing.T, res Result, key string) []Point {
	t.Helper()
	for _, sd := range res.Series {
		if sd.Chart+"/"+sd.Name == key {
			return sd.Points
		}
	}
	t.Fatalf("%s: no series %q", res.Range, key)
	return nil
}

// pointAt returns the point of pts whose T is minute i.
func pointAt(t *testing.T, pts []Point, i int) Point {
	t.Helper()
	for _, p := range pts {
		if p.T == minuteAt(i).Unix() {
			return p
		}
	}
	t.Fatalf("no point at minute %d", i)
	return Point{}
}

func isGap(p Point) bool { return p.Avg == nil && p.Min == nil && p.Max == nil }

func wantPoint(t *testing.T, p Point, avg, lo, hi float64) {
	t.Helper()
	if p.Avg == nil || p.Min == nil || p.Max == nil || *p.Avg != avg || *p.Min != lo || *p.Max != hi {
		b, _ := json.Marshal(p)
		t.Fatalf("point %s, want [%d,%v,%v,%v]", b, p.T, avg, lo, hi)
	}
}

func idx(i int) Sample { return Sample{"cpu/user": float64(i)} }

func TestRangeShapes(t *testing.T) {
	s, _ := openTest(t)
	end := 20 * 1440
	feedEach(t, s, "x", end-3*1440, end, func(int) Sample { return Sample{"cpu/user": 1} })
	now := minuteAt(end).Add(17 * time.Second)
	setNow(s, now)
	for _, c := range []struct {
		r    Range
		n    int
		step int64
	}{{RangeLive, 180, 5}, {Range15m, 15, 60}, {Range1h, 60, 60}, {Range24h, 288, 300}, {Range7d, 336, 1800}} {
		res := query(t, s, "x", c.r)
		pts := seriesOf(t, res, "cpu/user")
		if res.Range != c.r || len(pts) != c.n || int64(res.StepSeconds) != c.step {
			t.Fatalf("%s: range %s, %d points, step %d", c.r, res.Range, len(pts), res.StepSeconds)
		}
		if res.From != pts[0].T || res.To != pts[c.n-1].T || res.To%c.step != 0 || res.To-res.From != int64(c.n-1)*c.step {
			t.Fatalf("%s: from %d to %d", c.r, res.From, res.To)
		}
		for i := 1; i < c.n; i++ {
			if pts[i].T-pts[i-1].T != c.step {
				t.Fatalf("%s: T[%d]-T[%d] = %d", c.r, i, i-1, pts[i].T-pts[i-1].T)
			}
		}
		lastMin := now.Unix()/60*60 - 60
		want := map[Range]int64{RangeLive: now.Unix() / 5 * 5, Range15m: lastMin, Range1h: lastMin,
			Range24h: lastMin / 300 * 300, Range7d: lastMin / 1800 * 1800}[c.r]
		if res.To != want {
			t.Fatalf("%s: to %d, want %d", c.r, res.To, want)
		}
	}
}

func TestMinuteRangeValues(t *testing.T) {
	s, _ := openTest(t)
	feedMinute(t, s, "x", minuteAt(0), func(k int) Sample { return Sample{"cpu/user": float64(10 + k)} })
	setNow(s, minuteAt(1).Add(30*time.Second))
	pts := seriesOf(t, query(t, s, "x", Range15m), "cpu/user")
	p := pts[len(pts)-1]
	if p.T != minuteAt(0).Unix() {
		t.Fatalf("last T %d, want %d", p.T, minuteAt(0).Unix())
	}
	wantPoint(t, p, 15.5, 10, 21)
}

func TestSpikeSurvivesBucket(t *testing.T) {
	s, _ := openTest(t)
	// Minute 0: 19 samples, 18 x 5 and one 100: avg 10, min 5, max 100.
	for k := 0; k < 19; k++ {
		v := 5.0
		if k == 7 {
			v = 100
		}
		if err := s.Record("x", minuteAt(0).Add(time.Duration(k)*3*time.Second), Sample{"cpu/user": v}); err != nil {
			t.Fatal(err)
		}
	}
	feedEach(t, s, "x", 1, 5, func(int) Sample { return Sample{"cpu/user": 10} })
	setNow(s, minuteAt(5).Add(10*time.Second))
	for _, r := range []Range{Range24h, Range7d} {
		pts := seriesOf(t, query(t, s, "x", r), "cpu/user")
		wantPoint(t, pointAt(t, pts, 0), 10, 5, 100)
	}
}

func TestBucketAverageIsMeanOfMinuteAverages(t *testing.T) {
	s, _ := openTest(t)
	feedEach(t, s, "x", 0, 60, idx)
	setNow(s, minuteAt(60))
	wantPoint(t, pointAt(t, seriesOf(t, query(t, s, "x", Range7d), "cpu/user"), 0), 14.5, 0, 29)
}

func TestGapsAreNilNotZero(t *testing.T) {
	s, _ := openTest(t)
	feedEach(t, s, "x", 0, 19, idx)
	feedEach(t, s, "x", 23, 45, func(i int) Sample {
		smp := idx(i)
		if i >= 25 && i <= 28 {
			smp["memory/used"] = 1
		}
		return smp
	})
	// 0..19 and 23..44 committed (the record at 23 commits 19).
	setNow(s, minuteAt(60))
	res := query(t, s, "x", Range1h)
	if len(res.Series) != 2 {
		t.Fatalf("series %+v", res.Series)
	}
	cpu, mem := seriesOf(t, res, "cpu/user"), seriesOf(t, res, "memory/used")
	for i := 0; i < 60; i++ {
		gap := (i >= 20 && i <= 22) || i >= 45
		if p := pointAt(t, cpu, i); gap != isGap(p) {
			t.Fatalf("cpu minute %d gap=%v: %+v", i, gap, p)
		} else if !gap {
			wantPoint(t, p, float64(i), float64(i), float64(i))
		}
		if p := pointAt(t, mem, i); (i >= 25 && i <= 28) == isGap(p) {
			t.Fatalf("memory minute %d: %+v", i, p)
		}
	}
	week := seriesOf(t, query(t, s, "x", Range7d), "cpu/user")
	wantPoint(t, pointAt(t, week, 30), 37, 30, 44) // only 30..44 of 30..59
	if p := pointAt(t, week, -30); !isGap(p) {
		t.Fatalf("empty bucket %+v", p)
	}
}

func TestAlertSeriesHidden(t *testing.T) {
	s, _ := openTest(t)
	feedEach(t, s, "x", 0, 3, func(int) Sample { return Sample{"cpu/user": 1, "alert/cpu_high": 50} })
	setNow(s, minuteAt(3))
	for _, r := range []Range{RangeLive, Range15m, Range7d} {
		res := query(t, s, "x", r)
		if len(res.Series) != 1 || res.Series[0].Chart != "cpu" {
			t.Fatalf("%s: %+v", r, res.Series)
		}
	}
}

func TestUnitsAndNames(t *testing.T) {
	s, _ := openTest(t)
	feedEach(t, s, "x", 0, 2, func(int) Sample {
		return Sample{"network/rx:eth0": 1, "memory/rss": 2, "cpu/user": 3, "disk/read": 4}
	})
	setNow(s, minuteAt(2))
	for _, r := range []Range{RangeLive, Range1h} {
		res := query(t, s, "x", r)
		var keys []string
		for _, sd := range res.Series {
			keys = append(keys, sd.Chart+"/"+sd.Name)
			if sd.Chart == "network" && (sd.Name != "rx:eth0" || sd.Unit != "bytes_per_second") {
				t.Fatalf("%+v", sd)
			}
		}
		if strings.Join(keys, ",") != "cpu/user,disk/read,memory/rss,network/rx:eth0" {
			t.Fatalf("%s: order %v", r, keys)
		}
	}
}

func TestLiveQuery(t *testing.T) {
	s, _ := openTest(t)
	for k := 0; k < 240; k++ {
		if k == 230 {
			continue
		}
		if err := s.Record("x", t0.Add(time.Duration(5*k)*time.Second), Sample{"cpu/user": float64(k) + 0.1}); err != nil {
			t.Fatal(err)
		}
	}
	setNow(s, t0.Add(239*5*time.Second+3*time.Second))
	res := query(t, s, "x", RangeLive)
	pts := seriesOf(t, res, "cpu/user")
	if len(pts) != 180 || res.To != t0.Unix()+239*5 {
		t.Fatalf("%d points, to %d", len(pts), res.To)
	}
	for i, p := range pts {
		k := 60 + i
		if p.T != t0.Unix()+int64(5*k) {
			t.Fatalf("T %d", p.T)
		}
		if k == 230 {
			if !isGap(p) {
				t.Fatalf("skipped sample %+v", p)
			}
			continue
		}
		wantPoint(t, p, float64(k)+0.1, float64(k)+0.1, float64(k)+0.1)
	}
}

func TestRingWrap7d(t *testing.T) {
	s, _ := openTest(t)
	feedEach(t, s, "x", 0, Minutes+30, idx) // minutes 0..10109 committed
	check := func(nowMin, firstMin int) []Point {
		setNow(s, minuteAt(nowMin))
		pts := seriesOf(t, query(t, s, "x", Range7d), "cpu/user")
		if pts[0].T != minuteAt(firstMin).Unix() {
			t.Fatalf("first T %d, want minute %d", pts[0].T, firstMin)
		}
		return pts
	}
	// Window 0..10079: minutes 0..29 were overwritten by 10080..10109.
	pts := check(Minutes, 0)
	if !isGap(pts[0]) {
		t.Fatalf("minutes older than the ring: %+v", pts[0])
	}
	for _, p := range pts[1:] {
		m := float64((p.T - t0.Unix()) / 60)
		wantPoint(t, p, m+14.5, m, m+29)
	}
	// Window 30..10109 crosses the slot wrap.
	for _, p := range check(Minutes+30, 30) {
		m := float64((p.T - t0.Unix()) / 60)
		wantPoint(t, p, m+14.5, m, m+29)
	}
}

func TestEmptyItemJSON(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Record("x", t0, Sample{"cpu/user": 1}); err != nil {
		t.Fatal(err)
	}
	setNow(s, t0.Add(time.Hour))
	res, err := s.Query("x", Range7d)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"series":[]`) || strings.Contains(string(b), "null") {
		t.Fatalf("%s", b)
	}
}

func TestUnknownAndBadRange(t *testing.T) {
	s, _ := openTest(t)
	feedEach(t, s, "x", 0, 1, idx)
	if _, err := s.Query("nope", Range1h); !errors.Is(err, ErrUnknownItem) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := s.Query("x", Range("bogus")); !errors.Is(err, ErrBadRange) {
		t.Fatalf("bad range: %v", err)
	}
}

func TestTimeZoneIrrelevant(t *testing.T) {
	zone := time.FixedZone("x", 19800)
	run := func(loc *time.Location) map[Range]Result {
		s, _ := openTest(t)
		for k := 0; k < 12*40; k++ {
			at := t0.Add(time.Duration(5*k) * time.Second).In(loc)
			if err := s.Record("x", at, Sample{"cpu/user": float64(k % 17)}); err != nil {
				t.Fatal(err)
			}
		}
		setNow(s, minuteAt(40).In(loc))
		out := map[Range]Result{}
		for _, r := range []Range{RangeLive, Range15m, Range1h, Range24h, Range7d} {
			out[r] = query(t, s, "x", r)
		}
		return out
	}
	if a, b := run(time.UTC), run(zone); !reflect.DeepEqual(a, b) {
		t.Fatal("results differ between UTC and +05:30")
	}
}

func TestQueryDoesNotWaitForWriter(t *testing.T) {
	bfs := blockingFS{fsys: noSync{osFS}, armed: new(atomic.Bool), entered: make(chan struct{}), release: make(chan struct{})}
	s, err := open(t.TempDir(), NewFakeClock(t0), bfs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	feedEach(t, s, "x", 0, 10, idx) // 0..9 committed, 10 pending
	setNow(s, minuteAt(11))

	bfs.armed.Store(true)
	var released sync.Once
	release := func() { released.Do(func() { close(bfs.release) }) }
	defer release()
	writer := make(chan error, 1)
	go func() { writer <- s.Record("x", minuteAt(11), idx(11)) }() // commits minute 10
	select {
	case <-bfs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer never reached WriteAt")
	}

	done := make(chan Result, 1)
	go func() {
		res, _ := s.Query("x", Range15m)
		done <- res
	}()
	var res Result
	select {
	case res = <-done:
	case <-time.After(time.Second):
		t.Fatal("query waited for the writer")
	}
	pts := seriesOf(t, res, "cpu/user")
	for i := 0; i < 10; i++ {
		wantPoint(t, pointAt(t, pts, i), float64(i), float64(i), float64(i))
	}
	if p := pointAt(t, pts, 10); !isGap(p) {
		t.Fatalf("minute being written served: %+v", p)
	}

	release()
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	wantPoint(t, pointAt(t, seriesOf(t, query(t, s, "x", Range15m), "cpu/user"), 10), 10, 10, 10)
}

// TestQueryNeverSeesTornSlot overwrites, while 7d queries run, the slots of the
// oldest minutes of the queried window: a reader that matched the old stamp
// before the writer zeroed it must notice afterwards that the values changed.
func TestQueryNeverSeesTornSlot(t *testing.T) {
	s, _ := openTest(t)
	both := func(i int) Sample { return Sample{"cpu/user": float64(i), "memory/rss": float64(i)} }
	feedEach(t, s, "x", 0, Minutes, both) // minutes 0..10079 committed
	setNow(s, minuteAt(Minutes))          // 7d window = minutes 0..10079

	var stop atomic.Bool
	errc := make(chan error, 1)
	go func() {
		defer stop.Store(true)
		for i := Minutes + 1; i <= Minutes+3000; i++ { // commits 10080..13079 over slots 0..2999
			if err := s.Record("x", minuteAt(i), both(i)); err != nil {
				errc <- err
				return
			}
		}
		errc <- nil
	}()
	queries := 0
	for !stop.Load() {
		res, err := s.Query("x", Range7d)
		if err != nil {
			t.Fatal(err)
		}
		queries++
		for _, sd := range res.Series {
			for _, p := range sd.Points {
				if isGap(p) {
					continue
				}
				m := float64((p.T - t0.Unix()) / 60)
				if *p.Min < m || *p.Max > m+29 || *p.Avg < m || *p.Avg > m+29 {
					t.Fatalf("query %d, %s/%s bucket at minute %v holds [%v %v %v]", queries, sd.Chart, sd.Name, m, *p.Avg, *p.Min, *p.Max)
				}
			}
		}
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	t.Logf("%d queries during the writes", queries)
}
