package stats

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A NAS without RTC boots at Unix 0: no range may panic while the clock is
// under the first 15 minutes of the epoch.
func TestSmallClockQueriesDoNotPanic(t *testing.T) {
	for _, sec := range []int64{0, 4, 5, 100, 894, 895, 900, 3600, 20000} {
		clk := NewFakeClock(time.Unix(sec, 0))
		s, err := Open(t.TempDir(), clk)
		if err != nil {
			t.Fatal(err)
		}
		for at := int64(0); at <= sec && at < 200; at += 5 {
			if err := s.Record("x", time.Unix(at, 0), Sample{"cpu/user": 1}); err != nil {
				t.Fatal(err)
			}
		}
		for _, r := range []Range{RangeLive, Range15m, Range1h, Range24h, Range7d} {
			if _, err := s.Query("x", r); err != nil {
				t.Fatalf("clock %d range %s: %v", sec, r, err)
			}
		}
		_ = s.Close()
	}
}

// Open lists a file it could not read and carries on with the others.
func TestOpenSkipsUnreadableRing(t *testing.T) {
	good := func(dir string) {
		r, err := createRing(osFS, ringPath(dir, "y"), "y")
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
	}
	check := func(t *testing.T, s *Store, name string) {
		t.Helper()
		defer s.Close()
		if got := s.Items(); !slices.Equal(got, []string{"y"}) {
			t.Fatalf("Items=%q", got)
		}
		if got := s.Skipped(); !slices.Equal(got, []string{name}) {
			t.Fatalf("Skipped=%q, want [%s]", got, name)
		}
		if len(s.Quarantined()) != 0 {
			t.Fatalf("Quarantined=%q", s.Quarantined())
		}
	}
	t.Run("open error", func(t *testing.T) {
		dir := t.TempDir()
		good(dir)
		r, err := createRing(osFS, ringPath(dir, "x"), "x")
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Close()
		s, err := open(dir, NewFakeClock(t0), denyOpenFS{osFS, filepath.Base(ringPath(dir, "x"))})
		if err != nil {
			t.Fatal(err)
		}
		check(t, s, filepath.Base(ringPath(dir, "x")))
	})
	t.Run("dangling symlink", func(t *testing.T) {
		dir := t.TempDir()
		good(dir)
		link := ringPath(dir, "x")
		if err := os.Symlink(filepath.Join(dir, "nowhere"), link); err != nil {
			t.Skip("no symlinks here:", err)
		}
		s, err := Open(dir, NewFakeClock(t0))
		if err != nil {
			t.Fatal(err)
		}
		check(t, s, filepath.Base(link))
	})
	t.Run("symlink to directory", func(t *testing.T) {
		dir := t.TempDir()
		good(dir)
		target := filepath.Join(dir, "adir")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		link := ringPath(dir, "x")
		if err := os.Symlink(target, link); err != nil {
			t.Skip("no symlinks here:", err)
		}
		s, err := Open(dir, NewFakeClock(t0))
		if err != nil {
			t.Fatal(err)
		}
		check(t, s, filepath.Base(link))
	})
}

type denyOpenFS struct {
	fsys
	name string
}

func (d denyOpenFS) OpenFile(name string, flag int, perm os.FileMode) (file, error) {
	if filepath.Base(name) == d.name {
		return nil, os.ErrPermission
	}
	return d.fsys.OpenFile(name, flag, perm)
}

// The age of a quarantined file counts from the quarantine.
func TestQuarantineRestartsRetention(t *testing.T) {
	dir := t.TempDir()
	p := ringPath(dir, "x")
	if err := os.WriteFile(p, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := t0.Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, NewFakeClock(t0))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fi, err := os.Stat(p + ".bad")
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(t0) {
		t.Fatalf("mtime %v, want %v", fi.ModTime(), t0)
	}
	if err := s.Sweep(t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".bad"); err != nil {
		t.Fatalf("young .bad swept: %v", err)
	}
}

func TestIsPartial(t *testing.T) {
	if IsPartial(nil) || IsPartial(errors.New("x")) || IsPartial(ErrClosed) {
		t.Fatal("false positive")
	}
	capErr := fmt.Errorf("%w: %q", ErrSeriesCap, "k")
	if !IsPartial(capErr) || !IsPartial(errors.Join(capErr, fmt.Errorf("%w", ErrBadKey))) {
		t.Fatal("rejected keys must be partial")
	}
	if IsPartial(errors.Join(capErr, syscallErr)) {
		t.Fatal("a joined real failure is not partial")
	}
}

var syscallErr = errors.New("disk failure")

func TestSeriesCapIsPartial(t *testing.T) {
	s, _ := openTest(t)
	smp := Sample{}
	for i := 0; i < MaxSeries; i++ {
		smp[fmt.Sprintf("c/k%02d", i)] = 1
	}
	if err := s.Record("x", minuteAt(0), smp); err != nil {
		t.Fatal(err)
	}
	smp["c/new"] = 2
	err := s.Record("x", minuteAt(0).Add(5*time.Second), smp)
	if !errors.Is(err, ErrSeriesCap) || !IsPartial(err) {
		t.Fatalf("err %v", err)
	}
	res, qerr := s.Query("x", RangeLive)
	if qerr != nil || len(res.Series) != MaxSeries {
		t.Fatalf("stored %d series, %v", len(res.Series), qerr)
	}
}

func TestInvalidUTF8ItemRejected(t *testing.T) {
	s, dir := openTest(t)
	bad := "caf\xe9"
	if err := s.Record(bad, minuteAt(0), Sample{"a/b": 1}); !errors.Is(err, ErrBadItem) {
		t.Fatalf("Record: %v", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("files created: %v", ents)
	}
	ev, err := NewEvaluator(s, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.SetRules(bad, []Rule{{Key: "cpu_high", Enabled: true, Threshold: 50, Minutes: 3}}); !errors.Is(err, ErrBadItem) {
		t.Fatalf("SetRules: %v", err)
	}
}

func TestQueryAfterClose(t *testing.T) {
	s, _ := openTest(t)
	if err := s.Record("x", minuteAt(0), Sample{"a/b": 1}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := s.Query("x", RangeLive); !errors.Is(err, ErrClosed) {
		t.Fatalf("err %v", err)
	}
}
