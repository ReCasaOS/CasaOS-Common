package file

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/mholt/archives"
)

func tarNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	r := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	sort.Strings(names)
	return names
}

// Below a root, a selected link that leads out of it is not followed, and the entries are
// named as WriteArchive names them.
func TestWriteArchiveInStaysBelowItsRoot(t *testing.T) {
	base := t.TempDir()
	top := filepath.Join(base, "Kids")
	for _, d := range []string{filepath.Join(top, "school"), filepath.Join(base, "Other")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(top, "school", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "Other", "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Other", filepath.Join(top, "out")); err != nil {
		t.Skip("no symlink here:", err)
	}
	root, err := os.OpenRoot(top)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var buf bytes.Buffer
	if err := WriteArchiveIn(context.Background(), &buf, archives.Tar{}, root, top, ".", []string{"school"}); err != nil {
		t.Fatal(err)
	}
	got := tarNames(t, buf.Bytes())
	if len(got) != 2 || got[0] != "Kids/school/" || got[1] != "Kids/school/a.txt" {
		t.Errorf("entries: %v", got)
	}

	buf.Reset()
	if err := WriteArchiveIn(context.Background(), &buf, archives.Tar{}, root, top, ".", []string{"out"}); err == nil {
		t.Error("a link out of the root was followed")
	}
	if bytes.Contains(buf.Bytes(), []byte("secret")) {
		t.Error("a file outside the root is in the archive")
	}
	if err := WriteArchiveIn(context.Background(), &buf, archives.Tar{}, root, top, ".", []string{"../Other"}); err == nil {
		t.Error("a path above the root was archived")
	}
}
