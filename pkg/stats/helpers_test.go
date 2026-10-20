package stats

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// minuteAt returns t0 plus i minutes.
func minuteAt(i int) time.Time { return t0.Add(time.Duration(i) * time.Minute) }

// feedMinute records 12 samples 5 s apart inside minute, then one more call at the
// first second of the next minute so that the minute is committed. That last call
// (k == 12) belongs to the following minute and is the one that triggers the commit.
func feedMinute(t *testing.T, s *Store, item string, minute time.Time, perSample func(k int) Sample) {
	t.Helper()
	for k := 0; k <= 12; k++ {
		if err := s.Record(item, minute.Add(time.Duration(k)*5*time.Second), perSample(k)); err != nil {
			t.Fatalf("record %d: %v", k, err)
		}
	}
}

func f(v float64) *float64 { return &v }

// noSync wraps a filesystem so that file Sync is a no-op; it keeps tests that
// write thousands of minutes fast.
type noSync struct{ fsys }

func (n noSync) OpenFile(name string, flag int, perm os.FileMode) (file, error) {
	fl, err := n.fsys.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return noSyncFile{fl}, nil
}

type noSyncFile struct{ file }

func (noSyncFile) Sync() error { return nil }

// blockingFS parks the first file write made while armed: it closes entered,
// then waits for release to be closed before writing.
type blockingFS struct {
	fsys
	armed   *atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (b blockingFS) OpenFile(name string, flag int, perm os.FileMode) (file, error) {
	fl, err := b.fsys.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return blockingFile{fl, b}, nil
}

type blockingFile struct {
	file
	b blockingFS
}

func (f blockingFile) WriteAt(p []byte, off int64) (int, error) {
	if f.b.armed.CompareAndSwap(true, false) {
		close(f.b.entered)
		<-f.b.release
	}
	return f.file.WriteAt(p, off)
}
