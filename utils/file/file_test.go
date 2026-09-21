package file_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-Common/utils/file"
	"github.com/stretchr/testify/require"
)

func TestArchiveTree(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(filepath.Join(data, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(data, "a.txt"), []byte("alpha"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(data, "sub", "b.txt"), []byte("bravo"), 0o644))
	outside := filepath.Join(root, "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("must not be archived"), 0o644))

	// data itself is commonPath, so it gets no entry: only what is under it.
	want := map[string]string{
		"data/a.txt":     "alpha",
		"data/sub/":      "",
		"data/sub/b.txt": "bravo",
	}
	// Symlinks need privileges on Windows; where they can be made, one
	// pointing out of the tree must be stored as a link, not followed.
	if os.Symlink(outside, filepath.Join(data, "link")) == nil {
		want["data/link"] = "-> " + outside
	}

	for _, format := range []string{"zip", "targz"} {
		t.Run(format, func(t *testing.T) {
			_, ar, err := file.GetCompressionAlgorithm(format)
			require.NoError(t, err)
			files, err := file.AddFile(nil, data, data)
			require.NoError(t, err)

			var buf bytes.Buffer
			require.NoError(t, ar.Archive(context.Background(), &buf, files))

			got := map[string]string{}
			if format == "zip" {
				zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
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
			} else {
				gz, err := gzip.NewReader(&buf)
				require.NoError(t, err)
				tr := tar.NewReader(gz)
				for {
					h, err := tr.Next()
					if err == io.EOF {
						break
					}
					require.NoError(t, err)
					switch h.Typeflag {
					case tar.TypeDir:
						got[strings.TrimSuffix(h.Name, "/")+"/"] = ""
					case tar.TypeSymlink:
						got[h.Name] = "-> " + h.Linkname
					default:
						body, err := io.ReadAll(tr)
						require.NoError(t, err)
						got[h.Name] = string(body)
					}
				}
			}
			require.Equal(t, want, got)
		})
	}
}
