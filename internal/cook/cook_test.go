package cook

import "testing"

// feed runs input through a Cooker one chunk at a time and returns everything
// emitted, including whatever the final flush produces.
func feed(chunks ...string) string {
	c := New()
	out := ""
	for _, chunk := range chunks {
		out += string(c.Write([]byte(chunk)))
	}
	return out + string(c.Flush())
}

func TestPlainLines(t *testing.T) {
	if got, want := feed("one\ntwo\n"), "one\ntwo\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A progress bar is the case that makes cooking worth doing: without it, one
// line of visual output becomes hundreds of lines in the buffer.
func TestCarriageReturnCollapsesProgressBar(t *testing.T) {
	input := "\r 10%\r 50%\r100% done\n"
	if got, want := feed(input), "100% done\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBackspace(t *testing.T) {
	// Backspace moves the write position without erasing, so the two
	// characters typed after it overwrite the two it moved back over:
	// "abcX", back up twice, type "cd" -> "abcd".
	if got, want := feed("abcX\b\bcd\n"), "abcd\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Erasing what was backed over requires an explicit erase.
	if got, want := feed("abcXY\b\b\x1b[K\n"), "abc\n"; got != want {
		t.Errorf("backspace+erase: got %q, want %q", got, want)
	}
}

func TestColourCodesAreRemoved(t *testing.T) {
	input := "\x1b[31mred\x1b[0m and \x1b[1;32mgreen\x1b[0m\n"
	if got, want := feed(input), "red and green\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEraseToEndOfLine(t *testing.T) {
	// Write a long line, return to column 0, write a short one and erase the
	// remains: the leftovers must not survive.
	input := "aaaaaaaaaa\rbb\x1b[K\n"
	if got, want := feed(input), "bb\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestOSCIsDropped(t *testing.T) {
	// A window-title sequence, and a shell-integration marker: neither is text.
	input := "\x1b]0;my title\x07hello\x1b]133;A\x07\nworld\n"
	if got, want := feed(input), "hello\nworld\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestOSCTerminatedByStringTerminator(t *testing.T) {
	input := "\x1b]8;;http://example.com\x1b\\link\x1b]8;;\x1b\\\n"
	if got, want := feed(input), "link\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Escape sequences arrive split across pty reads all the time, so the parser
// has to hold its state between chunks.
func TestSequenceSplitAcrossChunks(t *testing.T) {
	if got, want := feed("\x1b[3", "1mred\x1b", "[0m\n"), "red\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := feed("\x1b]0;ti", "tle\x07text\n"), "text\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTabExpands(t *testing.T) {
	if got, want := feed("a\tb\n"), "a       b\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A line still being drawn is withheld until it is final, so it is not
// emitted once now and again later. Flush is what releases it at session end.
func TestPartialLineHeldUntilFlush(t *testing.T) {
	c := New()
	if got := string(c.Write([]byte("prompt$ "))); got != "" {
		t.Errorf("partial line was emitted early: %q", got)
	}
	if got := string(c.Write([]byte("ls\n"))); got != "prompt$ ls\n" {
		t.Errorf("got %q, want %q", got, "prompt$ ls\n")
	}
	if got := string(c.Flush()); got != "" {
		t.Errorf("flush emitted a spurious line: %q", got)
	}
}

func TestBlankLinesArePreserved(t *testing.T) {
	if got, want := feed("a\n\n\nb\n"), "a\n\n\nb\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScreenClearEndsTheLine(t *testing.T) {
	// Without treating a clear as a boundary, "before" and "after" would be
	// glued into one line.
	if got, want := feed("before\x1b[2Jafter\n"), "before\nafter\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestConPTYRowRepositioning replays a real ConPTY stream captured on Windows.
//
// ConPTY does not emit a newline between these two lines; it repositions the
// cursor to row 4. Handling only the column merges them and lets the second
// overwrite the first, which silently loses a line that was captured
// correctly. This is the exact stream that caught the bug.
func TestConPTYRowRepositioning(t *testing.T) {
	input := "\x1b[?9001h\x1b[?1004h\x1b[?25l\x1b[2J\x1b[m\x1b[HLINE-ONE\r\n" +
		"\x1b]0;cmd.exe\x07\x1b[?25h\x1b[?25l" +
		"LINE-TWO \x1b[4;1HMicrosoft Windows [Version 10.0.26200.9168]\r\n" +
		"\x1b[?25h\x1b[?9001l\x1b[?1004l"
	want := "LINE-ONE\nLINE-TWO\nMicrosoft Windows [Version 10.0.26200.9168]\n"
	if got := feed(input); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// A reposition to the same row keeps overwrite semantics, which is what makes
// an in-place status line collapse rather than multiply.
func TestSameRowRepositionOverwrites(t *testing.T) {
	if got, want := feed("\x1b[1;1Habcdef\x1b[1;1HXY\n"), "XYcdef\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCursorDownEndsLine(t *testing.T) {
	if got, want := feed("first\x1b[1Bsecond\n"), "first\nsecond\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
