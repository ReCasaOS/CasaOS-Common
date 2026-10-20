package stats

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Item, container and VM names must not be readable by every local user: the
// directory is 0750 and the files are 0640, also when they existed before.
func TestFileModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stats")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o777) // past the umask
	old, err := createRing(osFS, ringPath(dir, "old"), "old")
	if err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	bad := ringPath(dir, "bad")
	if err := os.WriteFile(bad, []byte("garbage"), 0o666); err != nil {
		t.Fatal(err)
	}
	alerts := filepath.Join(dir, "alerts.json")
	if err := os.WriteFile(alerts, []byte("garbage"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alerts+".tmp", nil, 0o666); err != nil { // left by a crash
		t.Fatal(err)
	}
	for _, p := range []string{ringPath(dir, "old"), bad, alerts, alerts + ".tmp"} {
		_ = os.Chmod(p, 0o666)
	}
	linux := runtime.GOOS != "windows" // modes are not enforced on Windows; Open and the writes must still work
	check := func(p string, limit os.FileMode) {
		t.Helper()
		if !linux {
			return
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if m := fi.Mode().Perm(); m&^limit != 0 {
			t.Errorf("%s: mode %o, want at most %o", filepath.Base(p), m, limit)
		}
	}

	s, err := Open(dir, NewFakeClock(t0))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(dir, 0o750) // by Open itself
	ev, err := NewEvaluator(s, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record("new", t0, Sample{"cpu/user": 1}); err != nil {
		t.Fatal(err)
	}
	if err := ev.SetRules("new", []Rule{{Key: "cpu_high", Enabled: true, Threshold: 50, Minutes: 3}}); err != nil {
		t.Fatal(err)
	}
	check(ringPath(dir, "old"), 0o640)
	check(bad+".bad", 0o640)
	check(ringPath(dir, "new"), 0o640)
	check(alerts, 0o640)
	check(alerts+".bad", 0o640)
}
