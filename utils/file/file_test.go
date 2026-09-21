package file_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/ReCasaOS/CasaOS-Common/utils/file"
	"github.com/mholt/archives"
	"github.com/stretchr/testify/require"
)

var formats = map[string]string{
	"zip":    ".zip",
	"tar":    ".tar",
	"targz":  ".tar.gz",
	"tarbz2": ".tar.bz2",
	"tarxz":  ".tar.xz",
	"tarlz4": ".tar.lz4",
	"tarsz":  ".tar.sz",
}

// tree builds root/data holding a.txt, sub/b.txt, an empty directory and a
// file no test selects on its own, next to root/outside.txt.
func tree(t *testing.T) (root, data string) {
	root = t.TempDir()
	data = filepath.Join(root, "data")
	write(t, filepath.Join(data, "a.txt"), "alpha")
	write(t, filepath.Join(data, "sub", "b.txt"), "bravo")
	write(t, filepath.Join(data, "other.txt"), "charlie")
	require.NoError(t, os.Mkdir(filepath.Join(data, "empty"), 0o755))
	write(t, filepath.Join(root, "outside.txt"), "TOP SECRET")
	return root, data
}

func write(t *testing.T, name, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o755))
	require.NoError(t, os.WriteFile(name, []byte(body), 0o644))
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks cannot be made here: %v", err)
	}
}

func writeArchive(ctx context.Context, t *testing.T, w io.Writer, format, commonPath string, paths ...string) error {
	t.Helper()
	_, ar, err := file.GetCompressionAlgorithm(format)
	require.NoError(t, err)
	return file.WriteArchive(ctx, w, ar, commonPath, paths)
}

// entries reads an archive back with archive/zip or archive/tar, once the
// archives decompressor has undone the compression layer. Directories map to
// "", symlinks to "-> target" and files to their content.
func entries(t *testing.T, format string, b []byte) map[string]string {
	t.Helper()
	got := map[string]string{}
	if format == "zip" {
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		require.NoError(t, err)
		for _, f := range zr.File {
			rc, err := f.Open()
			require.NoError(t, err)
			body, err := io.ReadAll(rc)
			require.NoError(t, err)
			rc.Close()
			if f.Mode()&fs.ModeSymlink != 0 {
				body = append([]byte("-> "), body...)
			}
			got[f.Name] = string(body)
		}
		return got
	}

	var r io.Reader = bytes.NewReader(b)
	_, ar, err := file.GetCompressionAlgorithm(format)
	require.NoError(t, err)
	if ca, ok := ar.(archives.CompressedArchive); ok {
		rc, err := ca.Compression.OpenReader(r)
		require.NoError(t, err)
		defer rc.Close()
		r = rc
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return got
		}
		require.NoError(t, err)
		switch h.Typeflag {
		case tar.TypeDir:
			got[h.Name] = ""
		case tar.TypeSymlink:
			got[h.Name] = "-> " + h.Linkname
		default:
			body, err := io.ReadAll(tr)
			require.NoError(t, err)
			got[h.Name] = string(body)
		}
	}
}

// The core's download routes select files and folders under one parent.
func TestWriteArchiveSelection(t *testing.T) {
	_, data := tree(t)
	want := map[string]string{
		"data/a.txt":     "alpha",
		"data/sub/":      "",
		"data/sub/b.txt": "bravo",
		"data/empty/":    "",
	}
	for format, ext := range formats {
		t.Run(format, func(t *testing.T) {
			gotExt, _, err := file.GetCompressionAlgorithm(format)
			require.NoError(t, err)
			require.Equal(t, ext, gotExt)

			var buf bytes.Buffer
			require.NoError(t, writeArchive(context.Background(), t, &buf, format, data,
				filepath.Join(data, "a.txt"), filepath.Join(data, "sub"), filepath.Join(data, "empty")))
			require.Equal(t, want, entries(t, format, buf.Bytes()))
		})
	}
}

// commonPath itself gets no entry, and symlinks inside the walked tree, to a
// file or a directory out of it, are stored as links, never followed.
func TestWriteArchiveWholeDirectory(t *testing.T) {
	root, data := tree(t)
	write(t, filepath.Join(root, "elsewhere", "secret.txt"), "TOP SECRET")
	want := map[string]string{
		"data/a.txt":     "alpha",
		"data/other.txt": "charlie",
		"data/sub/":      "",
		"data/sub/b.txt": "bravo",
		"data/empty/":    "",
	}
	outside := filepath.Join(root, "outside.txt")
	elsewhere := filepath.Join(root, "elsewhere")
	if os.Symlink(outside, filepath.Join(data, "link")) == nil {
		want["data/link"] = "-> " + outside
	}
	if os.Symlink(elsewhere, filepath.Join(data, "linkdir")) == nil {
		want["data/linkdir"] = "-> " + elsewhere
	}

	for _, format := range []string{"zip", "tar"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, writeArchive(context.Background(), t, &buf, format, data, data))
			require.Equal(t, want, entries(t, format, buf.Bytes()))
			require.NotContains(t, buf.String(), "TOP SECRET")
		})
	}
}

// A symlink the user selected is followed: its target's content is archived
// under the link's name.
func TestWriteArchiveSelectedSymlink(t *testing.T) {
	root, data := tree(t)
	write(t, filepath.Join(root, "elsewhere", "y.txt"), "yankee")
	symlink(t, filepath.Join(root, "elsewhere"), filepath.Join(data, "linkdir"))
	symlink(t, filepath.Join(root, "outside.txt"), filepath.Join(data, "linkfile"))

	var buf bytes.Buffer
	require.NoError(t, writeArchive(context.Background(), t, &buf, "tar", data,
		filepath.Join(data, "linkdir"), filepath.Join(data, "linkfile")))
	require.Equal(t, map[string]string{
		"data/linkdir/":      "",
		"data/linkdir/y.txt": "yankee",
		"data/linkfile":      "TOP SECRET",
	}, entries(t, "tar", buf.Bytes()))

	// Alone, the selected link is commonPath: only its content is listed.
	linkdir := filepath.Join(data, "linkdir")
	buf.Reset()
	require.NoError(t, writeArchive(context.Background(), t, &buf, "tar", linkdir, linkdir))
	require.Equal(t, map[string]string{"linkdir/y.txt": "yankee"}, entries(t, "tar", buf.Bytes()))
}

// A single selected file is its own commonPath: it is archived, not dropped.
func TestWriteArchiveSingleFile(t *testing.T) {
	_, data := tree(t)
	a := filepath.Join(data, "a.txt")
	var buf bytes.Buffer
	require.NoError(t, writeArchive(context.Background(), t, &buf, "zip", a, a))
	require.Equal(t, map[string]string{"a.txt": "alpha"}, entries(t, "zip", buf.Bytes()))
}

// A bad selection is an error, and what was written before it is not left
// looking like a complete archive.
func TestWriteArchiveBadSelection(t *testing.T) {
	root, data := tree(t)
	var buf bytes.Buffer

	err := writeArchive(context.Background(), t, &buf, "zip", data, filepath.Join(data, "a.txt"), filepath.Join(data, "missing"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, err = zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.Error(t, err, "a zip holding the paths before the missing one")

	require.Error(t, writeArchive(context.Background(), t, &buf, "zip", data, filepath.Join(root, "outside.txt")))
	require.Error(t, writeArchive(context.Background(), t, &buf, "zip", data))
}

var errBoom = errors.New("boom")

// failAfter accepts n bytes, then fails.
type failAfter struct{ n int }

func (w *failAfter) Write(p []byte) (int, error) {
	if len(p) > w.n {
		n := w.n
		w.n = 0
		return n, errBoom
	}
	w.n -= len(p)
	return len(p), nil
}

// A write failure is an error, including one on the last bytes, which
// archives writes from a deferred Close whose error it drops.
func TestWriteArchiveWriteFailure(t *testing.T) {
	_, data := tree(t)
	noise := make([]byte, 256<<10)
	_, _ = rand.Read(noise)
	require.NoError(t, os.WriteFile(filepath.Join(data, "noise.bin"), noise, 0o644))

	for format := range formats {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, writeArchive(context.Background(), t, &buf, format, data, data))

			for _, n := range []int{100, buf.Len() - 1} {
				err := writeArchive(context.Background(), t, &failAfter{n: n}, format, data, data)
				require.ErrorIs(t, err, errBoom, "failing after %d of %d bytes", n, buf.Len())
			}
		})
	}
}

// cancelOnWrite cancels its context on the first write.
type cancelOnWrite struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelOnWrite) Write(p []byte) (int, error) {
	w.cancel()
	return w.Buffer.Write(p)
}

func TestWriteArchiveCancel(t *testing.T) {
	_, data := tree(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	require.ErrorIs(t, writeArchive(ctx, t, &buf, "targz", data, data), context.Canceled)
	require.Zero(t, buf.Len())

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	w := &cancelOnWrite{cancel: cancel}
	require.ErrorIs(t, writeArchive(ctx, t, w, "tar", data, data), context.Canceled)
	require.NotContains(t, w.String(), "alpha", "a.txt was copied after the cancel")
}

// onHeader runs swap when the tar header of name is written: after the walk
// saw the file and before the archiver opens it.
type onHeader struct {
	bytes.Buffer
	name string
	swap func()
}

func (w *onHeader) Write(p []byte) (int, error) {
	if w.swap != nil && bytes.HasPrefix(p, []byte(w.name+"\x00")) {
		w.swap()
		w.swap = nil
	}
	return w.Buffer.Write(p)
}

// A file replaced between the walk and the read is never read through. The
// swap runs on the archiver's goroutine, so it reports errors instead of
// failing the test itself.
func TestWriteArchiveFileSwappedAfterWalk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		symlink bool
		swap    func(root, victim string) error
		want    error
	}{
		{"symlink out of the tree", true, func(root, victim string) error {
			return errors.Join(os.Remove(victim), os.Symlink(filepath.Join(root, "outside.txt"), victim))
		}, nil},
		{"another file moved in", false, func(root, victim string) error {
			intruder := filepath.Join(filepath.Dir(victim), "intruder")
			return errors.Join(os.WriteFile(intruder, []byte("TOP SECRET"), 0o644), os.Rename(intruder, victim))
		}, nil},
		{"file shrank", false, func(root, victim string) error {
			return os.Truncate(victim, 2)
		}, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, data := tree(t)
			if tc.symlink {
				symlink(t, filepath.Join(root, "outside.txt"), filepath.Join(root, "probe"))
			}
			swapped := errors.New("the swap did not run")
			w := &onHeader{name: "data/a.txt", swap: func() { swapped = tc.swap(root, filepath.Join(data, "a.txt")) }}
			err := writeArchive(context.Background(), t, w, "tar", data, data)
			require.NoError(t, swapped)
			require.Error(t, err)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
			require.NotContains(t, w.String(), "TOP SECRET")
		})
	}
}
