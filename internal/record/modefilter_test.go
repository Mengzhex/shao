package record

import (
	"strings"
	"testing"
)

// feedFilter runs input through a modeFilter one chunk at a time.
func feedFilter(chunks ...string) string {
	var f modeFilter
	out := ""
	for _, c := range chunks {
		out += string(f.Write([]byte(c)))
	}
	return out + string(f.Flush())
}

// The regression this file exists for: forwarding these two sequences to the
// real terminal makes it switch input encoding, after which its keystrokes
// arrive as literal text and the terminal becomes unusable.
func TestBlockedModesAreRemoved(t *testing.T) {
	// Exactly what ConPTY emits when a recorded pwsh starts.
	input := "\x1b[?9001h\x1b[?1004h\x1b[?25l\x1b[2J\x1b[m\x1b[HPowerShell 7.6.5\r\n"
	got := feedFilter(input)

	for _, forbidden := range []string{"[?9001h", "[?1004h"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("%q reached the terminal: %q", forbidden, got)
		}
	}
	// Everything else must survive untouched.
	for _, wanted := range []string{"\x1b[?25l", "\x1b[2J", "\x1b[m", "\x1b[H", "PowerShell 7.6.5\r\n"} {
		if !strings.Contains(got, wanted) {
			t.Errorf("%q was lost: %q", wanted, got)
		}
	}
}

func TestBlockedModesRemovedOnReset(t *testing.T) {
	// The teardown form, with l instead of h.
	got := feedFilter("done\x1b[?9001l\x1b[?1004l")
	if got != "done" {
		t.Errorf("got %q, want %q", got, "done")
	}
}

// Sequences arrive split across pty reads constantly, so the filter has to
// recognise one that spans chunks.
func TestBlockedModeSplitAcrossChunks(t *testing.T) {
	cases := [][]string{
		{"\x1b", "[?9001h", "text"},
		{"\x1b[", "?9001h", "text"},
		{"\x1b[?", "9001h", "text"},
		{"\x1b[?900", "1h", "text"},
		{"\x1b[?9001", "h", "text"},
		{"\x1b[?9", "00", "1", "h", "text"},
	}
	for _, chunks := range cases {
		if got := feedFilter(chunks...); got != "text" {
			t.Errorf("chunks %q produced %q, want %q", chunks, got, "text")
		}
	}
}

// Anything that is not on the block list must pass through byte for byte,
// including private modes that matter, like the alternate screen buffer.
func TestOtherSequencesPassThrough(t *testing.T) {
	cases := []string{
		"\x1b[?1049h",           // alternate screen: vim and htop depend on it
		"\x1b[?25l",             // hide cursor
		"\x1b[?7h",              // autowrap
		"\x1b[?2004h",           // bracketed paste
		"\x1b[31mred\x1b[0m",    // colour
		"\x1b]0;title\x07",      // OSC
		"\x1b[?1004",            // unterminated: still must not vanish
		"\x1b[H\x1b[2J",         // cursor home, clear
		"plain text",            //
		"\x1b[1;2;3h",           // multi-parameter form is not the one we match
		"\x1b[?9001;1004h",      // parameter list: passed on rather than guessed at
		"\x1bOP",                // SS3
		"\x1b\x1b[?25l",         // a stray ESC before a real sequence
		"\x1b[?99999999999999h", // absurd parameter
	}
	for _, in := range cases {
		if got := feedFilter(in); got != in {
			t.Errorf("input %q was altered to %q", in, got)
		}
	}
}

// A held-back partial sequence must be released, not swallowed, when the
// stream ends.
func TestFlushReleasesPartialSequence(t *testing.T) {
	var f modeFilter
	if got := string(f.Write([]byte("hi\x1b[?900"))); got != "hi" {
		t.Errorf("got %q, want %q", got, "hi")
	}
	if got := string(f.Flush()); got != "\x1b[?900" {
		t.Errorf("flush returned %q, want the held bytes", got)
	}
}

func TestBlockedModeAmongRealOutput(t *testing.T) {
	in := "line one\r\n\x1b[?1004hline two\r\n\x1b[?9001lline three\r\n"
	want := "line one\r\nline two\r\nline three\r\n"
	if got := feedFilter(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The block list must not swallow a mode with a number that merely starts the
// same way.
func TestSimilarModeNumbersUnaffected(t *testing.T) {
	for _, in := range []string{"\x1b[?900h", "\x1b[?90010h", "\x1b[?100h", "\x1b[?10040h"} {
		if got := feedFilter(in); got != in {
			t.Errorf("%q was wrongly filtered to %q", in, got)
		}
	}
}
