package stats

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRing(t *testing.T, item string) (*ring, string) {
	t.Helper()
	p := ringPath(t.TempDir(), item)
	r, err := createRing(osFS, p, item)
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	return r, p
}

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	require.NoError(t, err)
	return st.Size()
}

func TestRingPathSafe(t *testing.T) {
	dir := t.TempDir()
	re := regexp.MustCompile(`^[0-9a-f]{32}\.ring$`)
	seen := map[string]bool{}
	for _, it := range []string{"../x", "a/b", "app:x:y:1", "Ünï", "A", "a"} {
		p := ringPath(dir, it)
		assert.Equal(t, dir, filepath.Dir(p))
		assert.Regexp(t, re, filepath.Base(p))
		assert.False(t, seen[p], it)
		seen[p] = true
	}
}

func TestCreateOpenRoundTrip(t *testing.T) {
	r, p := newTestRing(t, "app:x")
	require.NoError(t, r.Close())
	r2, err := openRing(osFS, p)
	require.NoError(t, err)
	defer r2.Close()
	assert.Equal(t, "app:x", r2.Item())
	assert.Empty(t, r2.Keys())
	assert.Equal(t, ringFileSize(0), fileSize(t, p))
	_, err = os.Stat(p + ".tmp")
	assert.True(t, os.IsNotExist(err))
}

func TestAddSeriesAndSize(t *testing.T) {
	r, p := newTestRing(t, "i")
	i0, err := r.addSeries("cpu/user")
	require.NoError(t, err)
	i1, err := r.addSeries("memory/rss")
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1}, []int{i0, i1})
	assert.Equal(t, ringFileSize(2), fileSize(t, p))
	again, err := r.addSeries("cpu/user")
	require.NoError(t, err)
	assert.Equal(t, 0, again)
	assert.Equal(t, ringFileSize(2), fileSize(t, p))
	require.NoError(t, r.Close())
	r2, err := openRing(osFS, p)
	require.NoError(t, err)
	defer r2.Close()
	assert.Equal(t, []string{"cpu/user", "memory/rss"}, r2.Keys())
}

func TestAddSeriesLimits(t *testing.T) {
	r, _ := newTestRing(t, "i")
	_, err := r.addSeries("")
	assert.ErrorIs(t, err, ErrBadKey)
	_, err = r.addSeries(strings.Repeat("k", 64))
	assert.ErrorIs(t, err, ErrBadKey)
	_, err = r.addSeries(strings.Repeat("k", 63))
	require.NoError(t, err)
	for i := 1; i < MaxSeries; i++ {
		_, err = r.addSeries("c/" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		require.NoError(t, err)
	}
	_, err = r.addSeries("c/over")
	assert.ErrorIs(t, err, ErrSeriesCap)
}

func TestWriteReadMinute(t *testing.T) {
	r, _ := newTestRing(t, "i")
	_, _ = r.addSeries("cpu/user")
	_, _ = r.addSeries("cpu/system")
	require.NoError(t, r.writeMinute(100, map[int]minuteValue{0: {1.5, 1, 2}}))
	w, err := r.readWindow(99, 101)
	require.NoError(t, err)
	c := w["cpu/user"]
	require.Len(t, c, 3)
	assert.False(t, c[0].OK)
	assert.Equal(t, minuteCell{1.5, 1, 2, true}, c[1])
	assert.False(t, c[2].OK)
	assert.False(t, w["cpu/system"][1].OK)
}

func TestLaterSeriesIsGapForEarlierMinutes(t *testing.T) {
	r, _ := newTestRing(t, "i")
	_, _ = r.addSeries("a/x")
	require.NoError(t, r.writeMinute(100, map[int]minuteValue{0: {1, 1, 1}}))
	_, _ = r.addSeries("a/y")
	require.NoError(t, r.writeMinute(101, map[int]minuteValue{0: {1, 1, 1}, 1: {2, 2, 2}}))
	w, err := r.readWindow(100, 101)
	require.NoError(t, err)
	assert.False(t, w["a/y"][0].OK)
	assert.True(t, w["a/y"][1].OK)
	assert.True(t, w["a/x"][0].OK)
}

func TestFloat32Precision(t *testing.T) {
	r, _ := newTestRing(t, "i")
	_, _ = r.addSeries("a/x")
	_, _ = r.addSeries("a/y")
	require.NoError(t, r.writeMinute(5, map[int]minuteValue{
		0: {17179869184, 17179869184, 17179869184}, 1: {1.5e10, 1.5e10, 1.5e10}}))
	w, err := r.readWindow(5, 5)
	require.NoError(t, err)
	for k, v := range map[string]float64{"a/x": 17179869184, "a/y": 1.5e10} {
		c := w[k][0]
		assert.True(t, c.OK)
		assert.InEpsilon(t, v, c.Avg, 1e-6)
		assert.InEpsilon(t, v, c.Min, 1e-6)
		assert.InEpsilon(t, v, c.Max, 1e-6)
	}
}

func TestWrapAndOverwrite(t *testing.T) {
	p := ringPath(t.TempDir(), "i")
	r, err := createRing(noSync{osFS}, p, "i")
	require.NoError(t, err)
	defer r.Close()
	_, _ = r.addSeries("a/x")
	for m := int64(1); m <= Minutes+5; m++ {
		require.NoError(t, r.writeMinute(m, map[int]minuteValue{0: {float64(m), float64(m) - 1, float64(m) + 1}}))
	}
	w, err := r.readWindow(6, Minutes+5)
	require.NoError(t, err)
	require.Len(t, w["a/x"], Minutes)
	for i, c := range w["a/x"] {
		require.True(t, c.OK, i)
		require.Equal(t, float64(6+i), c.Avg)
		require.Equal(t, float64(5+i), c.Min)
		require.Equal(t, float64(7+i), c.Max)
	}
	w, err = r.readWindow(1, 5)
	require.NoError(t, err)
	for _, c := range w["a/x"] {
		assert.False(t, c.OK)
	}
	n, err := r.newestMinute()
	require.NoError(t, err)
	assert.EqualValues(t, Minutes+5, n)
}

func TestNewestMinuteEmpty(t *testing.T) {
	r, _ := newTestRing(t, "i")
	n, err := r.newestMinute()
	require.NoError(t, err)
	assert.EqualValues(t, 0, n)
}

func readHeaders(t *testing.T, p string) (a, b header, aok, bok bool) {
	t.Helper()
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	a, aerr := decodeHeader(raw[:headerSize])
	b, berr := decodeHeader(raw[headerSize : 2*headerSize])
	return a, b, aerr == nil, berr == nil
}

func patch(t *testing.T, p string, off int64, b []byte) {
	t.Helper()
	fl, err := os.OpenFile(p, os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = fl.WriteAt(b, off)
	require.NoError(t, err)
	require.NoError(t, fl.Close())
}

func TestHeaderAlternates(t *testing.T) {
	r, p := newTestRing(t, "i")
	for _, k := range []string{"a/x", "a/y", "a/z"} {
		_, err := r.addSeries(k)
		require.NoError(t, err)
	}
	require.NoError(t, r.Close())
	a, b, aok, bok := readHeaders(t, p)
	require.True(t, aok && bok)
	d := int64(a.seq) - int64(b.seq)
	assert.True(t, d == 1 || d == -1, d)
	// corrupt the copy with the higher seq: the older one (two keys) must win
	hi := 0
	if b.seq > a.seq {
		hi = 1
	}
	patch(t, p, int64(hi)*headerSize+40, []byte{0xFF})
	r2, err := openRing(osFS, p)
	require.NoError(t, err)
	defer r2.Close()
	assert.Equal(t, []string{"a/x", "a/y"}, r2.Keys())
}

// withHeader rewrites both copies from the current one, mutated, with valid CRCs.
func withHeader(t *testing.T, p string, mut func(h *header)) {
	t.Helper()
	a, b, _, _ := readHeaders(t, p)
	if b.seq > a.seq {
		a = b
	}
	mut(&a)
	patch(t, p, 0, encodeHeader(a))
	a.seq--
	patch(t, p, headerSize, encodeHeader(a))
}

func TestOpenRejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(t *testing.T, p string)
		want error
	}{
		{"empty", func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, nil, 0o644)) }, errCorrupt},
		{"short", func(t *testing.T, p string) { require.NoError(t, os.WriteFile(p, make([]byte, 5000), 0o644)) }, errCorrupt},
		{"magic", func(t *testing.T, p string) {
			patch(t, p, 0, []byte("XXXXXXXX"))
			patch(t, p, headerSize, []byte("XXXXXXXX"))
		}, errCorrupt},
		{"both headers corrupt", func(t *testing.T, p string) {
			patch(t, p, 40, []byte{0xFF})
			patch(t, p, headerSize+40, []byte{0xFF})
		}, errCorrupt},
		{"version 2", func(t *testing.T, p string) { withHeader(t, p, func(h *header) { h.version = 2 }) }, errVersion},
		{"minutes", func(t *testing.T, p string) { withHeader(t, p, func(h *header) { h.minutes = 5 }) }, errCorrupt},
		{"nseries 49", func(t *testing.T, p string) {
			withHeader(t, p, func(h *header) { h.keys = make([]string, 49) })
		}, errCorrupt},
		{"truncated", func(t *testing.T, p string) {
			withHeader(t, p, func(h *header) { h.keys = []string{"a/x"} })
		}, errCorrupt},
		{"name length", func(t *testing.T, p string) {
			withHeader(t, p, func(h *header) { h.nameLen = 257 })
		}, errCorrupt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, p := newTestRing(t, "i")
			require.NoError(t, r.Close())
			c.mut(t, p)
			_, err := openRing(osFS, p)
			require.Error(t, err)
			assert.True(t, errors.Is(err, c.want), err.Error())
		})
	}
	t.Run("trailing bytes", func(t *testing.T) {
		r, p := newTestRing(t, "i")
		require.NoError(t, r.Close())
		patch(t, p, ringFileSize(0), []byte("tail"))
		r2, err := openRing(osFS, p)
		require.NoError(t, err)
		r2.Close()
	})
}

func TestForgotten(t *testing.T) {
	r, p := newTestRing(t, "i")
	require.NoError(t, r.setForgotten(123))
	assert.EqualValues(t, 123, r.ForgottenAt())
	require.NoError(t, r.Close())
	r2, err := openRing(osFS, p)
	require.NoError(t, err)
	assert.EqualValues(t, 123, r2.ForgottenAt())
	require.NoError(t, r2.setForgotten(0))
	require.NoError(t, r2.Close())
	r3, err := openRing(osFS, p)
	require.NoError(t, err)
	defer r3.Close()
	assert.EqualValues(t, 0, r3.ForgottenAt())
}

func TestRejectedNames(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"", strings.Repeat("n", 257)} {
		_, err := createRing(osFS, ringPath(d, "x"), n)
		assert.ErrorIs(t, err, ErrBadItem)
	}
	r, err := createRing(osFS, ringPath(d, "x"), strings.Repeat("n", 256))
	require.NoError(t, err)
	r.Close()
}

// stopBeforeStamp makes the real stamp write fail, as a crash between the
// values and the stamp would.
type stopBeforeStamp struct {
	fsys
	armed *bool
}

func (s stopBeforeStamp) OpenFile(name string, flag int, perm os.FileMode) (file, error) {
	fl, err := s.fsys.OpenFile(name, flag, perm)
	return stampFailFile{fl, s.armed}, err
}

type stampFailFile struct {
	file
	armed *bool
}

func (s stampFailFile) WriteAt(b []byte, off int64) (int, error) {
	if *s.armed && off >= dataStart && off < dataStart+colBytes && binary.LittleEndian.Uint32(b) != 0 {
		return 0, errors.New("crash before stamp")
	}
	return s.file.WriteAt(b, off)
}

func TestOldestSlotNotServedTorn(t *testing.T) {
	armed := false
	p := ringPath(t.TempDir(), "i")
	r, err := createRing(noSync{stopBeforeStamp{osFS, &armed}}, p, "i")
	require.NoError(t, err)
	defer r.Close()
	_, _ = r.addSeries("a/x")
	require.NoError(t, r.writeMinute(5, map[int]minuteValue{0: {1, 1, 1}}))
	armed = true
	require.Error(t, r.writeMinute(5+Minutes, map[int]minuteValue{0: {9, 9, 9}}))
	w, err := r.readWindow(5, 5)
	require.NoError(t, err)
	assert.False(t, w["a/x"][0].OK)
}

type opLog struct {
	fsys
	ops *[]string
}

func (o opLog) OpenFile(name string, flag int, perm os.FileMode) (file, error) {
	fl, err := o.fsys.OpenFile(name, flag, perm)
	return opLogFile{fl, o.ops}, err
}

type opLogFile struct {
	file
	ops *[]string
}

func (l opLogFile) Sync() error { *l.ops = append(*l.ops, "sync"); return l.file.Sync() }
func (l opLogFile) WriteAt(b []byte, off int64) (int, error) {
	switch {
	case off >= dataStart && off < dataStart+colBytes:
		*l.ops = append(*l.ops, fmt.Sprintf("stamp=%d", binary.LittleEndian.Uint32(b)))
	case off >= dataStart+colBytes:
		*l.ops = append(*l.ops, "value")
	}
	return l.file.WriteAt(b, off)
}

func TestWriteMinuteOrder(t *testing.T) {
	var ops []string
	r, err := createRing(opLog{osFS, &ops}, ringPath(t.TempDir(), "i"), "i")
	require.NoError(t, err)
	defer r.Close()
	_, _ = r.addSeries("a/x")
	ops = nil
	require.NoError(t, r.writeMinute(7, map[int]minuteValue{0: {1, 1, 1}}))
	assert.Equal(t, []string{"stamp=0", "sync", "value", "value", "value", "sync", "stamp=7"}, ops)
}

func TestOpenPicksHigherSeq(t *testing.T) {
	r, p := newTestRing(t, "i")
	for _, k := range []string{"a/x", "a/y", "a/z"} {
		_, _ = r.addSeries(k)
	}
	require.NoError(t, r.Close())
	r2, err := openRing(osFS, p)
	require.NoError(t, err)
	defer r2.Close()
	assert.Len(t, r2.Keys(), 3)
}
