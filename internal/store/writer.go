package store

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"tmon/internal/config"
)

// ringWriter appends to a segmented ring on disk. It is safe for concurrent
// use; the recorder writes from the pty pump goroutine and flushes from a
// timer goroutine.
type ringWriter struct {
	dir          string
	segmentBytes int64
	maxBytes     int64

	mu     sync.Mutex
	f      *os.File
	bw     *bufio.Writer
	seq    uint64
	first  uint64 // stream offset of the current segment's first byte
	curLen int64  // payload bytes in the current segment, buffered included
	segs   []segInfo
	total  int64
	closed bool
}

// newRingWriter opens (or resumes) a ring in dir.
//
// Resuming matters because a crashed recorder must not silently restart the
// offset numbering: a reader would then see two different bytes claiming the
// same stream offset. So a new segment always continues from the highest
// offset already on disk.
func newRingWriter(dir string, segmentBytes, maxBytes int64) (*ringWriter, error) {
	if err := config.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	if segmentBytes <= 0 {
		segmentBytes = config.DefaultSegmentBytes
	}
	// The ring must hold at least two segments or eviction would target the
	// segment currently being written.
	if maxBytes < 2*segmentBytes {
		maxBytes = 2 * segmentBytes
	}

	w := &ringWriter{dir: dir, segmentBytes: segmentBytes, maxBytes: maxBytes}

	switch segs, err := listSegments(dir); {
	case err == nil:
		w.segs = segs
		for _, s := range segs {
			w.total += s.Length
		}
		last := segs[len(segs)-1]
		w.seq = last.Seq + 1
		w.first = last.EndOffset()
	case os.IsNotExist(err) || err == ErrNoSegments:
		// Fresh ring.
	default:
		return nil, err
	}
	return w, nil
}

func (w *ringWriter) segPath(seq uint64) string {
	return filepath.Join(w.dir, segName(seq))
}

// openSegment starts a new segment file at the current stream offset.
func (w *ringWriter) openSegment() error {
	f, err := config.CreatePrivateFile(w.segPath(w.seq), os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
	if err != nil {
		return err
	}
	// The header is written unbuffered so a reader in another process can
	// identify the segment immediately, even before any payload lands.
	if _, err := f.Write(segHeader{Seq: w.seq, FirstOffset: w.first}.marshal()); err != nil {
		f.Close()
		return err
	}
	w.f = f
	w.bw = bufio.NewWriterSize(f, 64<<10)
	w.curLen = 0
	w.segs = append(w.segs, segInfo{Seq: w.seq, FirstOffset: w.first, Length: 0})
	return nil
}

// rollover closes the current segment and evicts from the tail as needed.
func (w *ringWriter) rollover() error {
	if w.f != nil {
		if err := w.bw.Flush(); err != nil {
			return err
		}
		if err := w.f.Close(); err != nil {
			return err
		}
		w.f, w.bw = nil, nil
		// first must advance by exactly what the closed segment held, and
		// curLen reset with it: Offset() is first+curLen, so leaving curLen
		// stale here would double-count the segment just closed.
		w.first += uint64(w.curLen)
		w.curLen = 0
		w.seq++
	}
	w.evict()
	return nil
}

// evict deletes whole segments from the oldest end until the ring is back
// under its cap.
//
// It stops at the first deletion failure rather than skipping ahead. On
// Windows a reader in another process holding the file open makes os.Remove
// fail, and skipping over it to delete a newer segment would punch a real
// hole in the middle of the stream. Overshooting the cap for a few seconds
// is the far cheaper mistake, and the next rollover retries.
func (w *ringWriter) evict() {
	for w.total > w.maxBytes && len(w.segs) > 1 {
		oldest := w.segs[0]
		if err := os.Remove(w.segPath(oldest.Seq)); err != nil {
			if !os.IsNotExist(err) {
				return
			}
		}
		w.segs = w.segs[1:]
		w.total -= oldest.Length
	}
}

func (w *ringWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, fmt.Errorf("store: write to closed ring %s", w.dir)
	}

	written := 0
	for len(p) > 0 {
		if w.f == nil {
			if err := w.openSegment(); err != nil {
				return written, err
			}
		}
		room := w.segmentBytes - w.curLen
		if room <= 0 {
			if err := w.rollover(); err != nil {
				return written, err
			}
			continue
		}
		n := int64(len(p))
		if n > room {
			n = room
		}
		got, err := w.bw.Write(p[:n])
		w.curLen += int64(got)
		w.total += int64(got)
		w.segs[len(w.segs)-1].Length = w.curLen
		written += got
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// Offset returns the stream offset just past the last byte written, which is
// where the next write will land.
func (w *ringWriter) Offset() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.first + uint64(w.curLen)
}

// Flush pushes buffered bytes to the filesystem. This is what bounds how
// stale a reader in another process can be.
func (w *ringWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.bw == nil {
		return nil
	}
	return w.bw.Flush()
}

// Buffered reports how many bytes are still only in memory.
func (w *ringWriter) Buffered() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.bw == nil {
		return 0
	}
	return w.bw.Buffered()
}

func (w *ringWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.f == nil {
		return nil
	}
	err := w.bw.Flush()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	w.f, w.bw = nil, nil
	return err
}
