package store

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Span describes what a stream currently holds on disk.
type Span struct {
	// OldestOffset is the stream offset of the earliest byte still present.
	OldestOffset uint64 `json:"oldest_offset"`
	// NewestOffset is the offset just past the latest byte present.
	NewestOffset uint64 `json:"newest_offset"`
	// Truncated means the ring has already discarded older data. This is
	// expected behaviour once a session exceeds its configured size, and is
	// deliberately reported separately from GapDetected.
	Truncated bool `json:"truncated"`
	// GapDetected means segments were found that do not join up: a hole in
	// the middle of the stream. This should never happen, and is surfaced
	// rather than hidden so "the recent past is complete" stays a checkable
	// claim instead of an assumption.
	GapDetected bool `json:"gap_detected"`
}

// Bytes is how much of the stream is still retrievable.
func (s Span) Bytes() int64 { return int64(s.NewestOffset - s.OldestOffset) }

// ReadResult is a slice of a stream plus the context needed to interpret it.
type ReadResult struct {
	Data        []byte `json:"-"`
	StartOffset uint64 `json:"start_offset"`
	EndOffset   uint64 `json:"end_offset"`
	Span        Span   `json:"span"`
}

// StreamDir is the directory holding one stream of one session.
func StreamDir(sessionDir, stream string) string {
	return filepath.Join(sessionDir, stream)
}

// ReadSpan reports what the stream currently holds.
func ReadSpan(dir string) (Span, error) {
	segs, err := listSegments(dir)
	if err != nil {
		if os.IsNotExist(err) || err == ErrNoSegments {
			return Span{}, nil
		}
		return Span{}, err
	}
	return spanOf(segs), nil
}

func spanOf(segs []segInfo) Span {
	return Span{
		OldestOffset: segs[0].FirstOffset,
		NewestOffset: segs[len(segs)-1].EndOffset(),
		Truncated:    segs[0].FirstOffset > 0,
		GapDetected:  !checkContiguity(segs),
	}
}

// ReadRange returns up to n bytes of the stream starting at offset off. The
// request is clamped to what is actually retained, so asking for more than
// the ring holds yields the oldest available bytes rather than an error.
func ReadRange(dir string, off uint64, n int64) (ReadResult, error) {
	segs, err := listSegments(dir)
	if err != nil {
		if os.IsNotExist(err) || err == ErrNoSegments {
			return ReadResult{}, nil
		}
		return ReadResult{}, err
	}
	span := spanOf(segs)

	if off < span.OldestOffset {
		// Asking for evicted data is not an error; report the shortfall.
		n -= int64(span.OldestOffset - off)
		off = span.OldestOffset
		span.Truncated = true
	}
	if off > span.NewestOffset {
		off = span.NewestOffset
	}
	if n < 0 {
		n = 0
	}
	if end := off + uint64(n); end > span.NewestOffset {
		n = int64(span.NewestOffset - off)
	}
	if n == 0 {
		return ReadResult{StartOffset: off, EndOffset: off, Span: span}, nil
	}

	want := off + uint64(n)
	buf := bytes.NewBuffer(make([]byte, 0, n))
	cursor := off

	for _, s := range segs {
		if s.EndOffset() <= cursor || s.FirstOffset >= want {
			continue
		}
		from := cursor
		if from < s.FirstOffset {
			// A segment vanished under us mid-read. If it was at the head
			// of our range that is eviction; anywhere else it is a hole.
			if buf.Len() == 0 {
				span.Truncated = true
			} else {
				span.GapDetected = true
			}
			from = s.FirstOffset
		}
		to := want
		if to > s.EndOffset() {
			to = s.EndOffset()
		}
		chunk, err := readSegRange(filepath.Join(dir, segName(s.Seq)), s, from, to)
		if err != nil {
			if os.IsNotExist(err) {
				// Evicted between listing and reading.
				if buf.Len() == 0 {
					span.Truncated = true
					cursor = to
					continue
				}
				span.GapDetected = true
				break
			}
			return ReadResult{}, err
		}
		buf.Write(chunk)
		cursor = to
		if cursor >= want {
			break
		}
	}

	data := buf.Bytes()
	start := cursor - uint64(len(data))
	return ReadResult{Data: data, StartOffset: start, EndOffset: cursor, Span: span}, nil
}

// readSegRange reads the payload of one segment between two absolute stream
// offsets.
func readSegRange(path string, s segInfo, from, to uint64) ([]byte, error) {
	if to <= from {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Re-read the header: a segment could in principle have been recycled
	// under a reused sequence number, and returning bytes from the wrong
	// place in the stream would be worse than returning nothing.
	hdr := make([]byte, segHeaderSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, err
	}
	h, err := parseSegHeader(hdr)
	if err != nil {
		return nil, err
	}
	if h.Seq != s.Seq || h.FirstOffset != s.FirstOffset {
		return nil, fmt.Errorf("store: segment %s changed identity under read", path)
	}

	inner := int64(from - s.FirstOffset)
	length := int64(to - from)
	buf := make([]byte, length)
	got, err := f.ReadAt(buf, segHeaderSize+inner)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:got], nil
}

// ReadTail returns the last max bytes of the stream.
func ReadTail(dir string, max int64) (ReadResult, error) {
	span, err := ReadSpan(dir)
	if err != nil {
		return ReadResult{}, err
	}
	if span.NewestOffset == 0 && span.OldestOffset == 0 {
		return ReadResult{}, nil
	}
	available := span.Bytes()
	if max > available {
		max = available
	}
	return ReadRange(dir, span.NewestOffset-uint64(max), max)
}

// LinesResult is a line-oriented view of the tail of a stream.
type LinesResult struct {
	Lines []string `json:"lines"`
	// FirstLinePartial means the first returned line was cut off at the
	// start by the byte budget, not by a newline.
	FirstLinePartial bool   `json:"first_line_partial"`
	StartOffset      uint64 `json:"start_offset"`
	EndOffset        uint64 `json:"end_offset"`
	Span             Span   `json:"span"`
}

// ReadTailLines returns the last maxLines lines of the stream, reading at
// most maxBytes to find them.
func ReadTailLines(dir string, maxLines int, maxBytes int64) (LinesResult, error) {
	res, err := ReadTail(dir, maxBytes)
	if err != nil {
		return LinesResult{}, err
	}
	out := LinesResult{
		StartOffset: res.StartOffset,
		EndOffset:   res.EndOffset,
		Span:        res.Span,
	}
	if len(res.Data) == 0 {
		return out, nil
	}

	data := res.Data
	// A trailing newline would otherwise produce a bogus empty last line.
	trimmedTrailing := false
	if data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
		trimmedTrailing = true
	}
	lines := splitLines(data)

	if len(lines) > maxLines && maxLines > 0 {
		lines = lines[len(lines)-maxLines:]
		out.FirstLinePartial = false
		// Recompute where the returned text starts so offsets stay honest.
		consumed := 0
		for _, l := range lines {
			consumed += len(l) + 1
		}
		if trimmedTrailing {
			consumed--
		}
		if int64(consumed) <= int64(len(res.Data)) {
			out.StartOffset = res.EndOffset - uint64(consumed)
		}
	} else if res.StartOffset > res.Span.OldestOffset {
		// The byte budget, not a newline, decided where we started.
		out.FirstLinePartial = true
	}

	out.Lines = lines
	return out, nil
}

// splitLines splits on \n and drops a single trailing \r, so CRLF output
// from Windows shells does not leave carriage returns in every line.
func splitLines(data []byte) []string {
	parts := bytes.Split(data, []byte{'\n'})
	lines := make([]string, 0, len(parts))
	for _, p := range parts {
		lines = append(lines, string(bytes.TrimSuffix(p, []byte{'\r'})))
	}
	return lines
}
