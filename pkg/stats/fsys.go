package stats

import (
	"io"
	"os"
	"path/filepath"
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
	Restrict(name string, perm os.FileMode) // best effort: clears the mode bits of name outside perm; never follows a symlink
}

// Item, container, VM and interface names are in the files: no access for other users.
const (
	dirMode  os.FileMode = 0o750
	fileMode os.FileMode = 0o640
)

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

// mkdirPrivate creates dir (missing parents 0755, dir itself dirMode) and
// narrows dir if it already existed: MkdirAll leaves an existing one as it was.
func mkdirPrivate(fs fsys, dir string) error {
	if err := fs.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := fs.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	fs.Restrict(dir, dirMode)
	return nil
}

// Restrict ignores every error, symlinks, and Windows, which has no such modes.
func (osFileSystem) Restrict(name string, perm os.FileMode) {
	if runtime.GOOS == "windows" {
		return
	}
	if fi, err := os.Lstat(name); err == nil && fi.Mode()&os.ModeSymlink == 0 && fi.Mode().Perm()&^perm != 0 {
		_ = os.Chmod(name, fi.Mode().Perm()&perm)
	}
}
