package stats

// Ring file layout (one file per item, all integers little-endian):
//
//	bytes 0..4095     header copy A
//	bytes 4096..8191  header copy B
//	then              the stamp column: 10 080 x uint32, the minute (Unix
//	                  seconds / 60) stored in each slot, 0 = never written
//	then, for series i = 0.., three columns avg, min, max of 10 080 x float32
//
// Slot of minute m is m % 10 080. A gap is a float32 NaN. The size of the file
// is a pure function of the series count (ringFileSize).
//
// Header: magic "RCSTATS\x00", version u32, minutes u32 (10 080), forgottenAt
// i64 (Unix seconds, 0 = alive), name length u16 + 256 bytes of name, nseries
// u32, 48 key slots of 64 bytes (1 length byte + 63 bytes), seq u64, then the
// CRC32-IEEE of everything before it. The writer always rewrites the copy with
// the lower seq, so one valid copy survives a torn header write; the reader
// takes the valid copy with the higher seq.
//
// A minute is committed by writing every value, fsync, and the 4-byte stamp
// last. A reader accepts a slot only when its stamp equals the expected minute
// both before and after it read the values, so it needs no lock.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

var (
	errCorrupt = errors.New("stats: corrupt ring file")
	errVersion = errors.New("stats: unsupported ring file version")
)

const (
	headerSize = 4096
	dataStart  = 2 * headerSize
	colBytes   = Minutes * 4
	keySlot    = 64

	offVersion   = 8
	offMinutes   = 12
	offForgotten = 16
	offNameLen   = 24
	offName      = 26
	offNSeries   = offName + MaxItemLen
	offKeys      = offNSeries + 4
	offSeq       = offKeys + MaxSeries*keySlot
	offCRC       = offSeq + 8
	headerLen    = offCRC + 4
)

var magic = [8]byte{'R', 'C', 'S', 'T', 'A', 'T', 'S', 0}

// ringFileSize is the size in bytes of a ring file with nseries series.
func ringFileSize(nseries int) int64 { return dataStart + colBytes*int64(1+3*nseries) }

// ringPath is the file of item inside dir; the name never depends on the item
// text apart from its hash, so hostile names cannot leave dir or collide.
func ringPath(dir, item string) string {
	sum := sha256.Sum256([]byte(item))
	return filepath.Join(dir, hex.EncodeToString(sum[:])[:32]+".ring")
}

type minuteCell struct {
	Avg, Min, Max float64
	OK            bool
}

type minuteValue struct{ Avg, Min, Max float64 }

type header struct {
	version, minutes uint32
	forgotten        int64
	name             string
	nameLen          int // raw length field; 0 means len(name) when encoding
	keys             []string
	seq              uint64
}

func encodeHeader(h header) []byte {
	b := make([]byte, headerSize)
	copy(b, magic[:])
	binary.LittleEndian.PutUint32(b[offVersion:], h.version)
	binary.LittleEndian.PutUint32(b[offMinutes:], h.minutes)
	binary.LittleEndian.PutUint64(b[offForgotten:], uint64(h.forgotten))
	nl := h.nameLen
	if nl == 0 {
		nl = len(h.name)
	}
	binary.LittleEndian.PutUint16(b[offNameLen:], uint16(nl))
	copy(b[offName:offName+MaxItemLen], h.name)
	binary.LittleEndian.PutUint32(b[offNSeries:], uint32(len(h.keys)))
	for i, k := range h.keys {
		if i >= MaxSeries {
			break
		}
		s := b[offKeys+i*keySlot:]
		s[0] = byte(len(k))
		copy(s[1:keySlot], k)
	}
	binary.LittleEndian.PutUint64(b[offSeq:], h.seq)
	binary.LittleEndian.PutUint32(b[offCRC:], crc32.ChecksumIEEE(b[:offCRC]))
	return b
}

func decodeHeader(b []byte) (header, error) {
	var h header
	if len(b) < headerLen {
		return h, fmt.Errorf("%w: short header", errCorrupt)
	}
	if [8]byte(b[:8]) != magic {
		return h, fmt.Errorf("%w: bad magic", errCorrupt)
	}
	if crc32.ChecksumIEEE(b[:offCRC]) != binary.LittleEndian.Uint32(b[offCRC:]) {
		return h, fmt.Errorf("%w: header checksum", errCorrupt)
	}
	h.version = binary.LittleEndian.Uint32(b[offVersion:])
	if h.version != FormatVersion {
		return h, fmt.Errorf("%w: %d", errVersion, h.version)
	}
	h.minutes = binary.LittleEndian.Uint32(b[offMinutes:])
	if h.minutes != Minutes {
		return h, fmt.Errorf("%w: %d minutes", errCorrupt, h.minutes)
	}
	h.forgotten = int64(binary.LittleEndian.Uint64(b[offForgotten:]))
	h.nameLen = int(binary.LittleEndian.Uint16(b[offNameLen:]))
	if h.nameLen < 1 || h.nameLen > MaxItemLen {
		return h, fmt.Errorf("%w: name length %d", errCorrupt, h.nameLen)
	}
	h.name = string(b[offName : offName+h.nameLen])
	n := binary.LittleEndian.Uint32(b[offNSeries:])
	if n > MaxSeries {
		return h, fmt.Errorf("%w: %d series", errCorrupt, n)
	}
	h.keys = make([]string, n)
	for i := range h.keys {
		s := b[offKeys+i*keySlot:]
		l := int(s[0])
		if l < 1 || l > MaxKeyLen {
			return h, fmt.Errorf("%w: key length %d", errCorrupt, l)
		}
		h.keys[i] = string(s[1 : 1+l])
	}
	h.seq = binary.LittleEndian.Uint64(b[offSeq:])
	return h, nil
}

type ring struct {
	fs   fsys
	f    file
	item string

	mu  sync.Mutex // serializes writers (header, series, minutes)
	hdr header     // latest durable header
	cur int        // copy (0 or 1) holding hdr

	keys      atomic.Pointer[[]string]
	forgotten atomic.Int64
}

func (r *ring) publish() {
	k := append([]string(nil), r.hdr.keys...)
	r.keys.Store(&k)
	r.forgotten.Store(r.hdr.forgotten)
}

func (r *ring) Item() string       { return r.item }
func (r *ring) ForgottenAt() int64 { return r.forgotten.Load() }
func (r *ring) Keys() []string     { return append([]string(nil), *r.keys.Load()...) }
func (r *ring) Close() error       { return r.f.Close() }
func (r *ring) series() []string   { return *r.keys.Load() }
func colOff(i, c int) int64        { return dataStart + colBytes*int64(1+3*i+c) }
func slotOf(minute int64) int64    { return ((minute % Minutes) + Minutes) % Minutes }
func putF32(b []byte, v float64)   { binary.LittleEndian.PutUint32(b, math.Float32bits(float32(v))) }
func getF32(b []byte) float64      { return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))) }
func nanBits() uint32              { return math.Float32bits(float32(math.NaN())) }

// writeHeader makes h (with seq bumped) durable in the copy not currently valid-latest.
func (r *ring) writeHeader(h header) error {
	h.seq = r.hdr.seq + 1
	idx := 1 - r.cur
	if _, err := r.f.WriteAt(encodeHeader(h), int64(idx)*headerSize); err != nil {
		return err
	}
	if err := r.f.Sync(); err != nil {
		return err
	}
	r.hdr, r.cur = h, idx
	r.publish()
	return nil
}

func createRing(fs fsys, path, item string) (*ring, error) {
	if len(item) < 1 || len(item) > MaxItemLen {
		return nil, ErrBadItem
	}
	tmp := path + ".tmp"
	f, err := fs.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	h := header{version: FormatVersion, minutes: Minutes, name: item, seq: 1}
	hb := encodeHeader(h)
	h.seq = 0
	buf := make([]byte, 0, ringFileSize(0))
	buf = append(buf, hb...)
	buf = append(buf, encodeHeader(h)...)
	buf = append(buf, make([]byte, colBytes)...)
	if _, err = f.WriteAt(buf, 0); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = fs.Rename(tmp, path)
	}
	if err != nil {
		_ = fs.Remove(tmp)
		return nil, err
	}
	_ = fs.SyncDir(filepath.Dir(path))
	return openRing(fs, path)
}

func openRing(fs fsys, path string) (*ring, error) {
	f, err := fs.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	r, err := parseRing(fs, f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return r, nil
}

func parseRing(fs fsys, f file) (*ring, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var hs [2]header
	var ok [2]bool
	for i := range hs {
		b := make([]byte, headerSize)
		if n, rerr := f.ReadAt(b, int64(i)*headerSize); n < headerSize && rerr != nil {
			continue
		}
		h, derr := decodeHeader(b)
		if errors.Is(derr, errVersion) {
			return nil, derr
		}
		hs[i], ok[i] = h, derr == nil
	}
	cur := -1
	for i := range hs {
		if ok[i] && (cur < 0 || hs[i].seq > hs[cur].seq) {
			cur = i
		}
	}
	if cur < 0 {
		return nil, fmt.Errorf("%w: no valid header", errCorrupt)
	}
	if st.Size() < ringFileSize(len(hs[cur].keys)) {
		return nil, fmt.Errorf("%w: file is %d bytes, need %d", errCorrupt, st.Size(), ringFileSize(len(hs[cur].keys)))
	}
	r := &ring{fs: fs, f: f, item: hs[cur].name, hdr: hs[cur], cur: cur}
	r.publish()
	return r, nil
}

func (r *ring) addSeries(key string) (int, error) {
	if len(key) < 1 || len(key) > MaxKeyLen {
		return 0, ErrBadKey
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, k := range r.hdr.keys {
		if k == key {
			return i, nil
		}
	}
	n := len(r.hdr.keys)
	if n >= MaxSeries {
		return 0, ErrSeriesCap
	}
	nan := make([]byte, 3*colBytes)
	bits := nanBits()
	for i := 0; i < len(nan); i += 4 {
		binary.LittleEndian.PutUint32(nan[i:], bits)
	}
	if _, err := r.f.WriteAt(nan, ringFileSize(n)); err != nil {
		return 0, err
	}
	if err := r.f.Sync(); err != nil {
		return 0, err
	}
	h := r.hdr
	h.keys = append(append([]string(nil), h.keys...), key)
	if err := r.writeHeader(h); err != nil {
		return 0, err
	}
	return n, nil
}

func (r *ring) setForgotten(at int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.hdr
	h.forgotten = at
	return r.writeHeader(h)
}

func (r *ring) writeMinute(minute int64, vals map[int]minuteValue) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := slotOf(minute)
	var b [4]byte
	for i := range r.hdr.keys {
		v, ok := vals[i]
		if !ok {
			v = minuteValue{math.NaN(), math.NaN(), math.NaN()}
		}
		for c, x := range [3]float64{v.Avg, v.Min, v.Max} {
			putF32(b[:], x)
			if _, err := r.f.WriteAt(b[:], colOff(i, c)+slot*4); err != nil {
				return err
			}
		}
	}
	if err := r.f.Sync(); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(b[:], uint32(minute))
	_, err := r.f.WriteAt(b[:], dataStart+slot*4)
	return err
}

func readFull(f file, b []byte, off int64) error {
	n, err := f.ReadAt(b, off)
	if n == len(b) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// readWindow returns toMin-fromMin+1 cells per series. A cell is OK only when
// the slot stamp matched the expected minute before and after its values were
// read and the average is not NaN.
func (r *ring) readWindow(fromMin, toMin int64) (map[string][]minuteCell, error) {
	n := toMin - fromMin + 1
	if n < 1 || n > Minutes {
		return nil, fmt.Errorf("stats: window of %d minutes", n)
	}
	keys := r.series()
	first := slotOf(fromMin)
	segs := [][2]int64{{first, min(n, Minutes-first)}} // slot, count
	if rest := n - segs[0][1]; rest > 0 {
		segs = append(segs, [2]int64{0, rest})
	}
	readCol := func(base int64) ([]byte, error) {
		out := make([]byte, 0, n*4)
		for _, s := range segs {
			buf := make([]byte, s[1]*4)
			if err := readFull(r.f, buf, base+s[0]*4); err != nil {
				return nil, err
			}
			out = append(out, buf...)
		}
		return out, nil
	}
	before, err := readCol(dataStart)
	if err != nil {
		return nil, err
	}
	cols := make([][3][]byte, len(keys))
	for i := range keys {
		for c := 0; c < 3; c++ {
			if cols[i][c], err = readCol(colOff(i, c)); err != nil {
				return nil, err
			}
		}
	}
	after, err := readCol(dataStart)
	if err != nil {
		return nil, err
	}
	res := make(map[string][]minuteCell, len(keys))
	for i, k := range keys {
		cells := make([]minuteCell, n)
		for j := int64(0); j < n; j++ {
			want := uint32(fromMin + j)
			if binary.LittleEndian.Uint32(before[j*4:]) != want || binary.LittleEndian.Uint32(after[j*4:]) != want {
				continue
			}
			a := getF32(cols[i][0][j*4:])
			if math.IsNaN(a) {
				continue
			}
			cells[j] = minuteCell{a, getF32(cols[i][1][j*4:]), getF32(cols[i][2][j*4:]), true}
		}
		res[k] = cells
	}
	return res, nil
}

func (r *ring) newestMinute() (int64, error) {
	b := make([]byte, colBytes)
	if err := readFull(r.f, b, dataStart); err != nil {
		return 0, err
	}
	var m uint32
	for i := 0; i < len(b); i += 4 {
		m = max(m, binary.LittleEndian.Uint32(b[i:]))
	}
	return int64(m), nil
}
