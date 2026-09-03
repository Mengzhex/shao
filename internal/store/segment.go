// Package store implements tmon's per-session ring buffers.
//
// A session's stream is a plain append-only byte stream, chopped into
// fixed-size segment files. When the total exceeds the configured cap the
// oldest whole segment is deleted. That gives the two properties the whole
// design rests on:
//
//   - Continuity. Whatever is still on disk is one unbroken run of the
//     stream, so "the last N bytes" is never stitched together from pieces
//     with holes in the middle. This is what capturing a byte stream buys
//     over sampling the screen: no matter when a question is asked, the
//     recent past is complete.
//   - Self-checking. Every segment records the absolute stream offset of its
//     first byte, so a reader can prove contiguity instead of assuming it.
//     A real hole is reported as GapDetected rather than silently returned
//     as if it were continuous, and ring eviction is reported separately as
//     Truncated because that is expected loss, not corruption.
//
// Writers and readers are different processes (a `tmon shell` recorder
// writes; `tmon mcp` reads), so all coordination happens through the
// filesystem. No shared memory, no IPC, and a recorder keeps working when
// nothing else is running.
package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	segMagic      = "TMONSEG1"
	segVersion    = uint16(1)
	segHeaderSize = 32
	segExt        = ".seg"

	// StreamRaw keeps every byte exactly as it came off the pty, for
	// fidelity. StreamCooked keeps the readable line stream that the AI
	// reads by default (see internal/cook).
	StreamRaw    = "raw"
	StreamCooked = "cooked"
)

// ErrNoSegments means the stream directory exists but holds no segments yet.
var ErrNoSegments = errors.New("store: no segments")

// segHeader prefixes every segment file.
//
//	 0..7   magic "TMONSEG1"
//	 8..9   format version (little endian)
//	10..15  reserved, zero
//	16..23  segment sequence number
//	24..31  absolute stream offset of the first payload byte
type segHeader struct {
	Seq         uint64
	FirstOffset uint64
}

func (h segHeader) marshal() []byte {
	b := make([]byte, segHeaderSize)
	copy(b[0:8], segMagic)
	binary.LittleEndian.PutUint16(b[8:10], segVersion)
	binary.LittleEndian.PutUint64(b[16:24], h.Seq)
	binary.LittleEndian.PutUint64(b[24:32], h.FirstOffset)
	return b
}

func parseSegHeader(b []byte) (segHeader, error) {
	if len(b) < segHeaderSize {
		return segHeader{}, fmt.Errorf("store: short segment header (%d bytes)", len(b))
	}
	if string(b[0:8]) != segMagic {
		return segHeader{}, fmt.Errorf("store: bad segment magic %q", b[0:8])
	}
	if v := binary.LittleEndian.Uint16(b[8:10]); v != segVersion {
		return segHeader{}, fmt.Errorf("store: unsupported segment version %d", v)
	}
	return segHeader{
		Seq:         binary.LittleEndian.Uint64(b[16:24]),
		FirstOffset: binary.LittleEndian.Uint64(b[24:32]),
	}, nil
}

// segInfo describes one segment file on disk.
type segInfo struct {
	Seq         uint64
	FirstOffset uint64
	Length      int64 // payload bytes, excluding the header
}

// EndOffset is the stream offset just past this segment's last byte.
func (s segInfo) EndOffset() uint64 { return s.FirstOffset + uint64(s.Length) }

func segName(seq uint64) string {
	return fmt.Sprintf("%012d%s", seq, segExt)
}

func segSeqFromName(name string) (uint64, bool) {
	if !strings.HasSuffix(name, segExt) {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSuffix(name, segExt), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// listSegments reads every segment header in dir and returns them ordered by
// sequence number. Files that are unreadable or too short to hold a header
// are skipped: a segment can legitimately be observed mid-creation by a
// reader running in another process.
func listSegments(dir string) ([]segInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var segs []segInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := segSeqFromName(e.Name()); !ok {
			continue
		}
		info, err := readSegInfo(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		segs = append(segs, info)
	}
	if len(segs) == 0 {
		return nil, ErrNoSegments
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].Seq < segs[j].Seq })
	return segs, nil
}

func readSegInfo(path string) (segInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return segInfo{}, err
	}
	defer f.Close()

	buf := make([]byte, segHeaderSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return segInfo{}, err
	}
	h, err := parseSegHeader(buf)
	if err != nil {
		return segInfo{}, err
	}
	st, err := f.Stat()
	if err != nil {
		return segInfo{}, err
	}
	return segInfo{Seq: h.Seq, FirstOffset: h.FirstOffset, Length: st.Size() - segHeaderSize}, nil
}

// checkContiguity verifies that segs form one unbroken run of the stream.
// segs must be sorted by Seq.
func checkContiguity(segs []segInfo) bool {
	for i := 1; i < len(segs); i++ {
		if segs[i].FirstOffset != segs[i-1].EndOffset() {
			return false
		}
	}
	return true
}
