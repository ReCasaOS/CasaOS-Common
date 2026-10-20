package stats

import (
	"io"
	"os"
	"runtime"
	"time"
)

// file is the subset of *os.File the ring code uses.
type file interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Stat() (os.FileInfo, error)
	Close() error
}

// fsys is the filesystem seam; tests substitute it to tear or fail writes.
type fsys interface {
	OpenFile(name string, flag int, perm os.FileMode) (file, error)
	Rename(a, b string) error
	Remove(name string) error
	ReadDir(dir string) ([]os.DirEntry, error)
	MkdirAll(dir string, perm os.FileMode) error
	SyncDir(dir string) error
	Chtimes(name string, t time.Time) error // best effort
}

type osFileSystem struct{}

// osFS is the real filesystem.
var osFS fsys = osFileSystem{}

func (osFileSystem) OpenFile(name string, flag int, perm os.FileMode) (file, error) {
	return os.OpenFile(name, flag, perm)
}
func (osFileSystem) Rename(a, b string) error                  { return os.Rename(a, b) }
func (osFileSystem) Remove(name string) error                  { return os.Remove(name) }
func (osFileSystem) ReadDir(dir string) ([]os.DirEntry, error) { return os.ReadDir(dir) }
func (osFileSystem) MkdirAll(dir string, perm os.FileMode) error {
	return os.MkdirAll(dir, perm)
}

// SyncDir is best effort: errors are ignored and Windows has no directory fsync.
func (osFileSystem) SyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (osFileSystem) Chtimes(name string, t time.Time) error { return os.Chtimes(name, t, t) }
