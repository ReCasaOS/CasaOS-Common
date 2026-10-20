package stats

import (
	"errors"
	"os"
	"path/filepath"
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

// blockingFS parks the first file write made while armed, or the stamp-column
// read that brings stampReads to 0: it closes entered,
// then waits for release to be closed before doing the I/O.
type blockingFS struct {
	fsys
	armed      *atomic.Bool
	stampReads *atomic.Int32 // if set, the ReadAt of the stamp column that brings it to 0 parks
	entered    chan struct{}
	release    chan struct{}
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

func (f blockingFile) ReadAt(p []byte, off int64) (int, error) {
	if f.b.stampReads != nil && off >= dataStart && off < dataStart+colBytes && f.b.stampReads.Add(-1) == 0 {
		close(f.b.entered)
		<-f.b.release
	}
	return f.file.ReadAt(p, off)
}

// errKilled is the panic value of a faultFS kill: the process "dies" there.
var errKilled = errors.New("killed")

// fop is one mutating operation seen by a faultFS.
type fop struct {
	kind string // open (O_CREATE|O_TRUNC), write, sync, rename, remove
	name string // base name for open, rename and remove
	off  int64
	n    int
}

// faultFS counts the mutating operations in order (OpenFile with
// O_CREATE|O_TRUNC, WriteAt, Sync, Rename, Remove) and can kill, tear or
// fail them.
//
// Assumption: an aligned 4-byte write is atomic (sector granularity). That is
// why a minute stamp is 4 bytes, and why tornAt only tears larger writes.
//
// Remove is counted and can be killed but never fails with failErr: freeing
// space does not need any.
type faultFS struct {
	fsys
	killAt   int   // the killAt-th op panics with errKilled before doing anything
	tornAt   int   // the tornAt-th WriteAt of more than 4 bytes writes half, then panics
	failFrom int   // every op from the failFrom-th on returns failErr, writing nothing
	failErr  error //
	readErr  error // if set, every ReadAt returns it
	ops      int   // mutating ops so far
	big      int   // WriteAt calls of more than 4 bytes so far
	trace    []fop
	files    []file
}

func (f *faultFS) op(o fop) error {
	f.ops++
	if f.ops == f.killAt {
		panic(errKilled)
	}
	f.trace = append(f.trace, o)
	if f.failFrom > 0 && f.ops >= f.failFrom && o.kind != "remove" {
		return f.failErr
	}
	return nil
}

// crash closes every file the dead process held, as the kernel would.
func (f *faultFS) crash() {
	for _, fl := range f.files {
		_ = fl.Close()
	}
	f.files = nil
}

func (f *faultFS) OpenFile(name string, flag int, perm os.FileMode) (file, error) {
	if flag&(os.O_CREATE|os.O_TRUNC) == os.O_CREATE|os.O_TRUNC {
		if err := f.op(fop{kind: "open", name: filepath.Base(name)}); err != nil {
			return nil, err
		}
	}
	fl, err := f.fsys.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	f.files = append(f.files, fl)
	return faultFile{fl, f}, nil
}

func (f *faultFS) Rename(a, b string) error {
	if err := f.op(fop{kind: "rename", name: filepath.Base(a)}); err != nil {
		return err
	}
	return f.fsys.Rename(a, b)
}

func (f *faultFS) Remove(name string) error {
	_ = f.op(fop{kind: "remove", name: filepath.Base(name)})
	return f.fsys.Remove(name)
}

type faultFile struct {
	file
	fs *faultFS
}

func (f faultFile) WriteAt(p []byte, off int64) (int, error) {
	if err := f.fs.op(fop{kind: "write", off: off, n: len(p)}); err != nil {
		return 0, err
	}
	if len(p) > 4 {
		f.fs.big++
		if f.fs.big == f.fs.tornAt {
			_, _ = f.file.WriteAt(p[:len(p)/2], off)
			panic(errKilled)
		}
	}
	return f.file.WriteAt(p, off)
}

func (f faultFile) Sync() error {
	if err := f.fs.op(fop{kind: "sync"}); err != nil {
		return err
	}
	return f.file.Sync()
}

func (f faultFile) ReadAt(p []byte, off int64) (int, error) {
	if f.fs.readErr != nil {
		return 0, f.fs.readErr
	}
	return f.file.ReadAt(p, off)
}
