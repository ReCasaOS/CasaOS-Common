package stats

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestDiskFullTmpfs fills a real 1 MiB tmpfs: Record fails with ENOSPC, the
// process goes on, history stays readable and the store recovers once space
// is freed. It needs root to mount.
func TestDiskFullTmpfs(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount a tmpfs")
	}
	dir := t.TempDir()
	if out, err := exec.Command("mount", "-t", "tmpfs", "-o", "size=1m", "tmpfs", dir).CombinedOutput(); err != nil {
		t.Skipf("mount tmpfs: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("umount", dir).Run() })
	s, err := Open(dir, NewFakeClock(minuteAt(60)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	feedMinute(t, s, crashItem, minuteAt(0), constSample(Sample{"cpu/user": 1}))

	filler := filepath.Join(dir, "filler")
	fl, err := os.Create(filler)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 4096)
	for {
		if _, err := fl.Write(chunk); err != nil {
			break
		}
	}
	_ = fl.Close()

	// Each new series needs three more columns: the disk is full by the first.
	m := 1
	for ; m < 40; m++ {
		smp := Sample{"cpu/user": 1, fmt.Sprintf("disk/read:d%d", m): 1}
		var rerr error
		for k := 0; k <= 12; k++ {
			if err := s.Record(crashItem, minuteAt(m).Add(time.Duration(k)*5*time.Second), smp); err != nil {
				rerr = err
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, syscall.ENOSPC) {
				t.Fatalf("minute %d: %v", m, rerr)
			}
			break
		}
	}
	if m == 40 {
		t.Fatal("never ran out of space")
	}
	if got := userAvgsAt(t, s, 0); got != 1 {
		t.Fatalf("minute 0 after ENOSPC: %v", got)
	}
	if err := os.Remove(filler); err != nil {
		t.Fatal(err)
	}
	// The failed commit's next minute already holds one sample; it commits whole.
	m++
	feedMinute(t, s, crashItem, minuteAt(m), constSample(Sample{"cpu/user": 1, "disk/read:new": 1}))
	if c := cellsOf(t, s, crashItem, m, m); !c["cpu/user"][0].OK || !c["disk/read:new"][0].OK {
		t.Fatalf("minute %d after freeing space: %+v", m, c)
	}
}

// userAvgsAt returns the cpu/user average of minute m (-1: gap).
func userAvgsAt(t *testing.T, s *Store, m int) float64 {
	t.Helper()
	c := cellsOf(t, s, crashItem, m, m)["cpu/user"]
	if len(c) != 1 || !c[0].OK {
		return -1
	}
	return c[0].Avg
}
