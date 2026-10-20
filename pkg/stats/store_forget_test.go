package stats

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const week = 7 * 24 * time.Hour

func mustSweep(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	if err := s.Sweep(now); err != nil {
		t.Fatalf("sweep %v: %v", now, err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func ringFiles(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".ring") {
			n++
		}
	}
	return n
}

func TestForgetKeepsHistory7Days(t *testing.T) {
	s, dir := openTest(t)
	feedMinute(t, s, "x", minuteAt(0), constSample(Sample{"cpu/user": 5}))
	at := minuteAt(0) // before the last sample: only the forgotten rule can remove it at at+7d
	if err := s.Forget("x", at); err != nil {
		t.Fatal(err)
	}
	mustSweep(t, s, at.Add(week-time.Second))
	if !slices.Equal(s.Items(), []string{"x"}) || !exists(ringPath(dir, "x")) {
		t.Fatalf("removed early: Items=%q", s.Items())
	}
	setNow(s, minuteAt(2))
	wantPoint(t, pointAt(t, seriesOf(t, query(t, s, "x", Range15m), "cpu/user"), 0), 5, 5, 5)

	mustSweep(t, s, at.Add(week))
	if len(s.Items()) != 0 || exists(ringPath(dir, "x")) {
		t.Fatalf("kept: Items=%q", s.Items())
	}
	if _, err := s.Query("x", Range15m); !errors.Is(err, ErrUnknownItem) {
		t.Fatalf("query after sweep: %v", err)
	}
}

func TestForgetSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	feedMinute(t, s, "x", minuteAt(0), constSample(Sample{"cpu/user": 1}))
	if err := s.Forget("x", minuteAt(0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.items["x"].ring.ForgottenAt() != minuteAt(0).Unix() {
		t.Fatalf("forgottenAt=%d", s.items["x"].ring.ForgottenAt())
	}
	mustSweep(t, s, minuteAt(0).Add(8*24*time.Hour))
	if len(s.Items()) != 0 || exists(ringPath(dir, "x")) {
		t.Fatalf("kept: Items=%q", s.Items())
	}
}

func TestRecordRevivesForgotten(t *testing.T) {
	dir := t.TempDir()
	s, err := open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	feedMinute(t, s, "x", minuteAt(0), constSample(Sample{"cpu/user": 1}))
	if err := s.Forget("x", minuteAt(1)); err != nil {
		t.Fatal(err)
	}
	later := minuteAt(0).Add(8*24*time.Hour - 2*time.Minute)
	feedMinute(t, s, "x", later, constSample(Sample{"cpu/user": 2}))
	if s.items["x"].ring.ForgottenAt() != 0 {
		t.Fatal("flag not cleared by Record")
	}
	mustSweep(t, s, minuteAt(0).Add(8*24*time.Hour))
	if !slices.Equal(s.Items(), []string{"x"}) {
		t.Fatalf("revived item removed: %q", s.Items())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.items["x"].ring.ForgottenAt() != 0 {
		t.Fatal("flag back after reopen")
	}
}

func TestSweepStaleWithoutForget(t *testing.T) {
	s, dir := openTest(t)
	feedMinute(t, s, "x", minuteAt(0), constSample(Sample{"cpu/user": 1})) // last sample at minute 1
	end := minuteAt(0).Add(week + time.Minute)
	feedMinute(t, s, "y", end.Add(-2*time.Minute), constSample(Sample{"cpu/user": 1}))
	mustSweep(t, s, end.Add(-time.Second))
	if !slices.Equal(s.Items(), []string{"x", "y"}) {
		t.Fatalf("removed early: %q", s.Items())
	}
	mustSweep(t, s, end)
	if !slices.Equal(s.Items(), []string{"y"}) || exists(ringPath(dir, "x")) || !exists(ringPath(dir, "y")) {
		t.Fatalf("Items=%q", s.Items())
	}
}

// After a restart only the stamps tell when data stopped.
func TestSweepStaleAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	feedMinute(t, s, "x", minuteAt(0), constSample(Sample{"cpu/user": 1}))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = open(dir, NewFakeClock(t0), noSync{osFS})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mustSweep(t, s, minuteAt(0).Add(week-time.Second))
	if len(s.Items()) != 1 {
		t.Fatal("removed early")
	}
	mustSweep(t, s, minuteAt(0).Add(week))
	if len(s.Items()) != 0 {
		t.Fatal("kept")
	}
}

// A brand-new item whose first minute is still pending has no stamp yet.
func TestSweepKeepsPendingItem(t *testing.T) {
	s, _ := openTest(t)
	at := minuteAt(0).Add(30 * 24 * time.Hour)
	if err := s.Record("x", at, Sample{"cpu/user": 1}); err != nil {
		t.Fatal(err)
	}
	mustSweep(t, s, at)
	if len(s.Items()) != 1 {
		t.Fatal("pending item removed")
	}
}

func TestSweepCleansTmpAndOldBad(t *testing.T) {
	s, dir := openTest(t)
	now := minuteAt(0).Add(30 * 24 * time.Hour)
	files := map[string]time.Time{"x.ring.tmp": now, "old.ring.bad": now.Add(-8 * 24 * time.Hour), "new.ring.bad": now.Add(-24 * time.Hour)}
	for n, mt := range files {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	mustSweep(t, s, now)
	if exists(filepath.Join(dir, "x.ring.tmp")) || exists(filepath.Join(dir, "old.ring.bad")) || !exists(filepath.Join(dir, "new.ring.bad")) {
		t.Fatal("wrong files removed")
	}
}

func TestSweepReopenable(t *testing.T) {
	s, _ := openTest(t)
	feedMinute(t, s, "x", minuteAt(0), constSample(Sample{"cpu/user": 1, "memory/rss": 2}))
	at := minuteAt(0).Add(8 * 24 * time.Hour)
	mustSweep(t, s, at)
	if err := s.Record("x", at, Sample{"cpu/user": 3}); err != nil {
		t.Fatal(err)
	}
	r := s.items["x"].ring
	if m, err := r.newestMinute(); err != nil || m != 0 || len(r.Keys()) != 0 {
		t.Fatalf("history kept: newest=%d keys=%q err=%v", m, r.Keys(), err)
	}
}

func TestSweepKeepsForeignFiles(t *testing.T) {
	s, dir := openTest(t)
	now := minuteAt(0).Add(30 * 24 * time.Hour)
	foreign := []string{"alerts.json", "alerts.json.tmp", "alerts.json.bad", "notes.tmp"}
	for _, n := range foreign {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-8 * 24 * time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	mustSweep(t, s, now)
	mustSweep(t, s, now.Add(365*24*time.Hour))
	for _, n := range foreign {
		if b, _ := os.ReadFile(filepath.Join(dir, n)); string(b) != n {
			t.Fatalf("%s touched: %q", n, b)
		}
	}
}

func TestLiveCapIgnoresForgotten(t *testing.T) {
	s, _ := openTest(t)
	for i := 0; i < MaxItems; i++ {
		if err := s.Record(fmt.Sprintf("item%d", i), minuteAt(0), Sample{"a/b": 1}); err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	if err := s.Forget("item0", minuteAt(0)); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("new", minuteAt(0), Sample{"a/b": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("item0", minuteAt(1), Sample{"a/b": 1}); !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("revive past the cap: %v", err)
	}
}

func TestForgottenAreBounded(t *testing.T) {
	s, dir := openTest(t)
	const n = 300
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("item%03d", i)
		if err := s.Record(name, minuteAt(i), Sample{"a/b": 1}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := s.Forget(name, minuteAt(i)); err != nil {
			t.Fatalf("forget %s: %v", name, err)
		}
	}
	if got := ringFiles(t, dir); got != MaxForgotten {
		t.Fatalf("ring files=%d", got)
	}
	items := s.Items()
	if len(items) != MaxForgotten || items[0] != fmt.Sprintf("item%03d", n-MaxForgotten) {
		t.Fatalf("%d items, first %q", len(items), items[0])
	}
	if exists(ringPath(dir, "item000")) || !exists(ringPath(dir, "item299")) {
		t.Fatal("wrong ones removed")
	}
}

func TestForgetUnknown(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Forget("x", minuteAt(0)); !errors.Is(err, ErrUnknownItem) {
		t.Fatalf("err=%v", err)
	}
}

// A clock at 1970 still forgets: 0 in the header would mean alive.
func TestForgetAt1970(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Record("x", minuteAt(0), Sample{"a/b": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget("x", time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	if s.items["x"].ring.ForgottenAt() == 0 {
		t.Fatal("still alive")
	}
}

func TestForgetSweepClosed(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Record("x", minuteAt(0), Sample{"a/b": 1}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if err := s.Forget("x", minuteAt(0)); !errors.Is(err, ErrClosed) {
		t.Fatalf("forget: %v", err)
	}
	if err := s.Sweep(minuteAt(0)); !errors.Is(err, ErrClosed) {
		t.Fatalf("sweep: %v", err)
	}
}

// Sweep closes a ring only once the queries reading it are done.
func TestSweepWaitsForQuery(t *testing.T) {
	bfs := blockingFS{fsys: noSync{osFS}, armed: new(atomic.Bool), stampReads: new(atomic.Int32),
		entered: make(chan struct{}), release: make(chan struct{})}
	s, err := open(t.TempDir(), NewFakeClock(t0), bfs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	feedEach(t, s, "x", 0, 2, idx)
	setNow(s, minuteAt(3))
	var released sync.Once
	release := func() { released.Do(func() { close(bfs.release) }) }
	defer release()
	bfs.stampReads.Store(1) // park the query's first stamp read
	type qr struct {
		res Result
		err error
	}
	done := make(chan qr, 1)
	go func() {
		res, err := s.Query("x", Range15m)
		done <- qr{res, err}
	}()
	select {
	case <-bfs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("query never reached the stamp read")
	}
	swept := make(chan error, 1)
	go func() { swept <- s.Sweep(minuteAt(0).Add(30 * 24 * time.Hour)) }()
	time.Sleep(50 * time.Millisecond) // let Sweep reach the close
	release()
	q := <-done
	if q.err != nil {
		t.Fatalf("query: %v", q.err)
	}
	wantPoint(t, pointAt(t, seriesOf(t, q.res, "cpu/user"), 1), 1, 1, 1)
	if err := <-swept; err != nil {
		t.Fatal(err)
	}
	if len(s.Items()) != 0 {
		t.Fatal("not swept")
	}
}

// A query that looked the item up before Sweep removed it reports it unknown.
func TestQueryOfSweptRing(t *testing.T) {
	s, _ := openTest(t)
	feedEach(t, s, "x", 0, 2, idx)
	if err := s.items["x"].ring.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query("x", Range15m); !errors.Is(err, ErrUnknownItem) {
		t.Fatalf("err=%v", err)
	}
}

// A Record that looked the item up just before its removal starts a new ring.
func TestRecordAfterRemovalRetries(t *testing.T) {
	s, dir := openTest(t)
	if err := s.Record("x", minuteAt(0), Sample{"a/b": 1}); err != nil {
		t.Fatal(err)
	}
	old := s.items["x"]
	old.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- s.Record("x", minuteAt(1), Sample{"a/b": 2}) }()
	time.Sleep(50 * time.Millisecond) // let Record pass the lookup and wait on old.mu
	s.mu.Lock()
	delete(s.items, "x") // what remove does, old.mu being held here
	old.gone, old.closed = true, true
	_ = old.ring.Close()
	_ = os.Remove(ringPath(dir, "x"))
	s.mu.Unlock()
	old.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e := s.items["x"]; e == nil || e == old || !exists(ringPath(dir, "x")) {
		t.Fatal("no new ring")
	}
}

// remove marks the entry so that a Record holding it looks the item up again.
func TestRemoveMarksEntryGone(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Record("x", minuteAt(0), Sample{"a/b": 1}); err != nil {
		t.Fatal(err)
	}
	old := s.items["x"]
	s.mu.Lock()
	err := s.remove("x")
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	old.mu.Lock()
	_, _, err = old.record(minuteAt(1), Sample{"a/b": 2})
	old.mu.Unlock()
	if !errors.Is(err, errGone) {
		t.Fatalf("err=%v", err)
	}
}

func TestSweepBadAgeEdge(t *testing.T) {
	s, dir := openTest(t)
	now := minuteAt(0).Add(30 * 24 * time.Hour)
	for n, mt := range map[string]time.Time{"edge.ring.bad": now.Add(-week), "young.ring.bad": now.Add(-week + time.Second)} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	mustSweep(t, s, now)
	if exists(filepath.Join(dir, "edge.ring.bad")) || !exists(filepath.Join(dir, "young.ring.bad")) {
		t.Fatal("wrong .bad removed at the 7-day edge")
	}
}

// Forgotten at the same time, the first name is evicted first. The candidates
// come from a map in random order, so the scenario is repeated: without the
// tie-break "a" would survive a run only by being sorted first by chance.
func TestForgottenCapTieBreakByName(t *testing.T) {
	names := []string{"a"}
	for i := 0; i < MaxForgotten; i++ {
		names = append(names, fmt.Sprintf("b%03d", i))
	}
	for run := 0; run < 5; run++ {
		s, dir := openTest(t)
		for _, n := range names {
			if err := s.Record(n, minuteAt(0), Sample{"a/b": 1}); err != nil {
				t.Fatal(err)
			}
			if err := s.Forget(n, minuteAt(0)); err != nil {
				t.Fatal(err)
			}
		}
		if exists(ringPath(dir, "a")) || !exists(ringPath(dir, "b000")) || len(s.Items()) != MaxForgotten {
			t.Fatalf("run %d: evicted the wrong one: %d items", run, len(s.Items()))
		}
	}
}

// A second Forget overwrites the time: the 7 days count from it.
func TestForgetTwiceOverwrites(t *testing.T) {
	s, _ := openTest(t)
	feedMinute(t, s, "x", minuteAt(0), constSample(Sample{"a/b": 1}))
	second := minuteAt(0).Add(-time.Hour)
	for _, at := range []time.Time{minuteAt(0), second} {
		if err := s.Forget("x", at); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.items["x"].ring.ForgottenAt(); got != second.Unix() {
		t.Fatalf("forgottenAt=%d", got)
	}
	mustSweep(t, s, second.Add(week))
	if len(s.Items()) != 0 {
		t.Fatal("kept past 7 days from the second Forget")
	}
}

// A ring that cannot be read is kept and reported; the others are still swept.
func TestSweepGoesOnAfterReadError(t *testing.T) {
	s, _ := openTest(t)
	for _, n := range []string{"broken", "stale"} {
		feedMinute(t, s, n, minuteAt(0), constSample(Sample{"a/b": 1}))
	}
	if err := s.items["broken"].ring.Close(); err != nil {
		t.Fatal(err)
	}
	err := s.Sweep(minuteAt(0).Add(8 * 24 * time.Hour))
	if !errors.Is(err, errRingClosed) {
		t.Fatalf("err=%v", err)
	}
	if !slices.Equal(s.Items(), []string{"broken"}) {
		t.Fatalf("Items=%q", s.Items())
	}
}

// infoFailFS returns directory entries whose Info fails for *.ring.bad.
type infoFailFS struct{ fsys }

type infoFailEntry struct{ os.DirEntry }

func (e infoFailEntry) Info() (os.FileInfo, error) { return nil, errInjected }

func (f infoFailFS) ReadDir(dir string) ([]os.DirEntry, error) {
	ents, err := f.fsys.ReadDir(dir)
	for i, e := range ents {
		if strings.HasSuffix(e.Name(), ".ring.bad") {
			ents[i] = infoFailEntry{e}
		}
	}
	return ents, err
}

func TestSweepReportsBadInfoError(t *testing.T) {
	dir := t.TempDir()
	s, err := open(dir, NewFakeClock(t0), infoFailFS{noSync{osFS}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := filepath.Join(dir, "x.ring.bad")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Sweep(minuteAt(0)); !errors.Is(err, errInjected) {
		t.Fatalf("err=%v", err)
	}
	if !exists(p) {
		t.Fatal("removed without knowing its age")
	}
}
