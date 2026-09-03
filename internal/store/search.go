package store

import (
	"bytes"
	"regexp"
)

// Search budgets. Searching reads a window of the tail into memory in one
// piece rather than sliding over the ring, which keeps a match from being
// missed because it straddled two windows. The default window is generous
// enough for "what went wrong just now" and small enough to stay cheap.
const (
	DefaultSearchBudget int64 = 8 << 20
	MaxSearchBudget     int64 = 128 << 20
)

// Match is one matching line plus surrounding context.
type Match struct {
	// LineOffset is the stream offset where this line starts.
	LineOffset uint64 `json:"line_offset"`
	// LinesFromEnd is 0 for the last line of the stream, 1 for the one
	// before it, and so on. It gives the model a sense of recency without
	// needing timestamps the stream does not carry.
	LinesFromEnd int      `json:"lines_from_end"`
	Line         string   `json:"line"`
	Before       []string `json:"before,omitempty"`
	After        []string `json:"after,omitempty"`
}

// SearchResult is the outcome of a backward search.
type SearchResult struct {
	Matches []Match `json:"matches"`
	// Scanned is how many bytes were examined.
	Scanned int64 `json:"scanned_bytes"`
	// ReachedOldest is true when the search covered everything retained. When
	// false the budget ran out first and older matches may exist, which is
	// reported rather than left for the reader to assume.
	ReachedOldest bool `json:"reached_oldest"`
	// Capped is true when maxMatches cut the result short.
	Capped bool `json:"capped"`
	Span   Span `json:"span"`
}

// Search scans a stream backward from the newest byte and returns matching
// lines, most recent first.
//
// Searching backward is the point: the question is nearly always "what is the
// most recent occurrence", and stopping after maxMatches then costs a small
// tail scan rather than a walk over the whole buffer.
func Search(dir string, re *regexp.Regexp, maxMatches, contextLines int, budget int64) (SearchResult, error) {
	if budget <= 0 {
		budget = DefaultSearchBudget
	}
	if budget > MaxSearchBudget {
		budget = MaxSearchBudget
	}
	if maxMatches <= 0 {
		maxMatches = 20
	}
	if contextLines < 0 {
		contextLines = 0
	}

	res, err := ReadTail(dir, budget)
	if err != nil {
		return SearchResult{}, err
	}
	out := SearchResult{
		Scanned:       int64(len(res.Data)),
		ReachedOldest: res.StartOffset <= res.Span.OldestOffset,
		Span:          res.Span,
	}
	if len(res.Data) == 0 {
		return out, nil
	}

	lines, offsets := splitLinesWithOffsets(res.Data, res.StartOffset)

	// The first line is only partial when the window started mid-line, which
	// would make a match on it misleading.
	firstUsable := 0
	if !out.ReachedOldest && len(lines) > 1 {
		firstUsable = 1
	}

	last := len(lines) - 1
	for i := last; i >= firstUsable; i-- {
		if !re.MatchString(lines[i]) {
			continue
		}
		if len(out.Matches) >= maxMatches {
			out.Capped = true
			break
		}
		m := Match{
			LineOffset:   offsets[i],
			LinesFromEnd: last - i,
			Line:         lines[i],
		}
		if contextLines > 0 {
			from := i - contextLines
			if from < firstUsable {
				from = firstUsable
			}
			to := i + contextLines
			if to > last {
				to = last
			}
			if from < i {
				m.Before = append([]string(nil), lines[from:i]...)
			}
			if to > i {
				m.After = append([]string(nil), lines[i+1:to+1]...)
			}
		}
		out.Matches = append(out.Matches, m)
	}
	return out, nil
}

// splitLinesWithOffsets splits a buffer into lines and records the stream
// offset at which each begins.
func splitLinesWithOffsets(data []byte, base uint64) ([]string, []uint64) {
	// A trailing newline terminates the last line; it does not start a new
	// empty one.
	trimmed := data
	if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '\n' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	parts := bytes.Split(trimmed, []byte{'\n'})

	lines := make([]string, 0, len(parts))
	offsets := make([]uint64, 0, len(parts))
	off := base
	for _, p := range parts {
		lines = append(lines, string(bytes.TrimSuffix(p, []byte{'\r'})))
		offsets = append(offsets, off)
		off += uint64(len(p)) + 1 // the newline that separated them
	}
	return lines, offsets
}
