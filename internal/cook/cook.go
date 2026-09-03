// Package cook turns a raw pty byte stream into a readable line stream.
//
// Raw pty output is full of redraw traffic: colour codes, cursor moves, and
// progress bars that rewrite the same line hundreds of times with carriage
// returns. Handing that to a model wastes its attention on escape sequences
// and, worse, makes a progress bar look like hundreds of distinct lines.
//
// So tmon keeps two streams per session. The raw stream is byte-exact and
// never interpreted. The cooked stream, produced here, applies the handful of
// control codes that change what a line finally says (carriage return,
// backspace, tab, erase-line, erase-screen, and both horizontal and vertical
// cursor moves) and discards the rest. What comes out is what a human would
// have seen settle on the screen.
//
// A line is emitted only once it is final, that is when a newline or a screen
// clear ends it. The partial line still being drawn is held back, because
// emitting it and then emitting it again after more bytes arrive would show
// the same line twice. In practice the withheld line is the shell prompt;
// error messages end with a newline and are emitted immediately. When the
// in-progress line really is wanted, the raw stream still has it.
//
// Columns are tracked as byte offsets, not display cells. That is exact for
// the ASCII that progress bars and prompts are made of, and for multibyte or
// double-width text it degrades to plain appending rather than corrupting
// anything.
package cook

import "bytes"

type state int

const (
	stGround  state = iota
	stEsc           // saw ESC
	stCSI           // inside ESC [ ... final
	stOSC           // inside ESC ] ... BEL|ST
	stOSCEsc        // inside OSC, saw ESC, expecting backslash
	stStr           // inside DCS/SOS/PM/APC ... ST
	stStrEsc        // inside string, saw ESC, expecting backslash
	stCharset       // ESC ( or ESC ) etc., swallow one more byte
)

const tabWidth = 8

// Cooker is an incremental raw-to-readable filter. It is not safe for
// concurrent use; the recorder owns one per session.
type Cooker struct {
	st   state
	csi  []byte // CSI parameter bytes accumulated so far
	line []byte // the line currently being drawn
	col  int    // write position within line, as a byte offset
	row  int    // current row, tracked only to notice vertical movement
	out  bytes.Buffer
}

func New() *Cooker { return &Cooker{} }

// Write feeds raw bytes in and returns cooked bytes to append to the cooked
// stream. The returned slice is only valid until the next call.
func (c *Cooker) Write(p []byte) []byte {
	c.out.Reset()
	for _, b := range p {
		c.step(b)
	}
	return c.out.Bytes()
}

// Flush emits the line currently being drawn, terminated with a newline.
//
// The recorder calls this when a session ends, so a final line that never got
// its newline is not lost. It is deliberately not called on every idle tick:
// doing so would emit the shell prompt as a line, and then emit it again as
// soon as the user typed the next character.
func (c *Cooker) Flush() []byte {
	c.out.Reset()
	c.endLineIfPending()
	return c.out.Bytes()
}

// Pending reports the length of the line being drawn but not yet emitted.
func (c *Cooker) Pending() int { return len(c.line) }

func (c *Cooker) step(b byte) {
	switch c.st {
	case stGround:
		c.ground(b)
	case stEsc:
		c.escape(b)
	case stCSI:
		// Parameter and intermediate bytes are 0x20-0x3F; the final byte is
		// 0x40-0x7E.
		if b >= 0x40 && b <= 0x7e {
			c.csiFinal(b)
			c.st = stGround
			c.csi = c.csi[:0]
		} else {
			c.csi = append(c.csi, b)
		}
	case stOSC:
		switch b {
		case 0x07: // BEL terminates an OSC string
			c.st = stGround
		case 0x1b:
			c.st = stOSCEsc
		}
	case stOSCEsc:
		// ESC \ is the other OSC terminator; anything else was a stray ESC.
		c.st = stGround
		if b != '\\' {
			c.step(b)
		}
	case stStr:
		if b == 0x1b {
			c.st = stStrEsc
		}
	case stStrEsc:
		c.st = stGround
		if b != '\\' {
			c.step(b)
		}
	case stCharset:
		c.st = stGround
	}
}

func (c *Cooker) ground(b byte) {
	switch b {
	case 0x1b: // ESC
		c.st = stEsc
	case '\n':
		c.endLine()
		c.row++
	case '\r':
		// Carriage return alone does not end a line; it moves the write
		// position back so following bytes overwrite it. This is what makes
		// a redrawing progress bar collapse into its final state.
		c.col = 0
	case '\b':
		if c.col > 0 {
			c.col--
		}
	case '\t':
		next := ((c.col / tabWidth) + 1) * tabWidth
		for c.col < next {
			c.put(' ')
		}
	case 0x00, 0x07: // NUL, BEL carry no text
	default:
		if b < 0x20 {
			// Any other C0 control is display state we do not model.
			return
		}
		c.put(b)
	}
}

func (c *Cooker) escape(b byte) {
	switch b {
	case '[':
		c.st = stCSI
		c.csi = c.csi[:0]
	case ']':
		// OSC. Shell integration markers live here, but they are consumed
		// from the raw stream by internal/shellint, not from cooked output.
		c.st = stOSC
	case 'P', 'X', '^', '_': // DCS, SOS, PM, APC
		c.st = stStr
	case '(', ')', '*', '+', '-', '.', '/': // charset designators
		c.st = stCharset
	case 'c': // RIS, full reset
		c.endLineIfPending()
		c.st = stGround
	default:
		c.st = stGround
	}
}

func (c *Cooker) csiFinal(final byte) {
	switch final {
	case 'K': // EL, erase in line
		switch c.csiParam(0, 0) {
		case 0: // erase from the write position to the end
			if c.col < len(c.line) {
				c.line = c.line[:c.col]
			}
		case 1: // erase from the start to the write position
			for i := 0; i < c.col && i < len(c.line); i++ {
				c.line[i] = ' '
			}
		case 2:
			c.line = c.line[:0]
			c.col = 0
		}
	case 'J': // ED, erase in display
		// Any erase-display ends whatever was on the current line. Treating
		// a full clear as a line boundary keeps a `clear` from silently
		// gluing the last line before it to the first line after it.
		c.endLineIfPending()
	case 'G', '`': // CHA, cursor to absolute column (1-based)
		n := c.csiParam(0, 1)
		if n < 1 {
			n = 1
		}
		c.setCol(n - 1)
	case 'C': // CUF, cursor forward
		c.setCol(c.col + max1(c.csiParam(0, 1)))
	case 'D': // CUB, cursor back
		c.setCol(c.col - max1(c.csiParam(0, 1)))
	case 'H', 'f': // CUP, cursor position (row, column), both 1-based
		c.gotoRow(max1(c.csiParam(0, 1)) - 1)
		c.setCol(max1(c.csiParam(1, 1)) - 1)
	case 'A': // CUU, cursor up
		c.gotoRow(c.row - max1(c.csiParam(0, 1)))
	case 'B': // CUD, cursor down
		c.gotoRow(c.row + max1(c.csiParam(0, 1)))
	case 'E': // CNL, cursor to start of a following line
		c.gotoRow(c.row + max1(c.csiParam(0, 1)))
		c.setCol(0)
	case 'F': // CPL, cursor to start of a preceding line
		c.gotoRow(c.row - max1(c.csiParam(0, 1)))
		c.setCol(0)
	case 'd': // VPA, absolute row
		c.gotoRow(max1(c.csiParam(0, 1)) - 1)
	case 'X': // ECH, erase characters at the write position
		n := max1(c.csiParam(0, 1))
		for i := c.col; i < c.col+n && i < len(c.line); i++ {
			c.line[i] = ' '
		}
	}
	// Everything else (colour, scroll regions, mode sets, cursor save and
	// restore) does not change the text of a line and is dropped.
}

// csiParam returns the idx-th numeric parameter of the pending CSI sequence,
// or def when it is absent or empty.
func (c *Cooker) csiParam(idx, def int) int {
	// Strip private-marker and intermediate bytes, keeping digits and ';'.
	params := make([]byte, 0, len(c.csi))
	for _, b := range c.csi {
		if (b >= '0' && b <= '9') || b == ';' {
			params = append(params, b)
		}
	}
	fields := bytes.Split(params, []byte{';'})
	if idx >= len(fields) || len(fields[idx]) == 0 {
		return def
	}
	n := 0
	for _, b := range fields[idx] {
		n = n*10 + int(b-'0')
		if n > 1<<20 { // guard against absurd parameters
			return 1 << 20
		}
	}
	return n
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// gotoRow handles vertical cursor movement, which on Windows is not optional.
//
// ConPTY is a diff-based renderer: rather than emitting a newline when output
// moves to the next line, it repositions the cursor by row. So a stream can
// read "LINE-TWO", ESC[4;1H, "next line" with no newline anywhere. Tracking
// only the column, as an ordinary log filter would, merges those into one line
// and then lets the second overwrite the first at column 0 -- output that was
// captured perfectly still comes out missing.
//
// A row change therefore ends the current line. Gaps are not padded with blank
// lines: ConPTY parks the cursor on arbitrary rows often enough that doing so
// would add more noise than it removed.
func (c *Cooker) gotoRow(row int) {
	if row < 0 {
		row = 0
	}
	if row == c.row {
		return
	}
	c.endLineIfPending()
	c.row = row
	c.col = 0
}

// setCol moves the write position, padding with spaces when it moves past
// the end of what has been written.
func (c *Cooker) setCol(col int) {
	if col < 0 {
		col = 0
	}
	// Bound the column so a bogus escape cannot make tmon allocate a huge
	// line buffer.
	if col > 1<<16 {
		col = 1 << 16
	}
	for len(c.line) < col {
		c.line = append(c.line, ' ')
	}
	c.col = col
}

func (c *Cooker) put(b byte) {
	if c.col < len(c.line) {
		c.line[c.col] = b
	} else {
		for len(c.line) < c.col {
			c.line = append(c.line, ' ')
		}
		c.line = append(c.line, b)
	}
	c.col++
}

// endLineIfPending ends the line only when something has been drawn on it.
// Screen clears and end-of-session flushes use this so they do not inject a
// blank line that was never on screen.
func (c *Cooker) endLineIfPending() {
	if len(c.line) > 0 {
		c.endLine()
	}
}

// endLine emits the current line and starts a new one. A line that is empty
// still counts: blank lines are part of how output reads.
func (c *Cooker) endLine() {
	c.out.Write(bytes.TrimRight(c.line, " "))
	c.out.WriteByte('\n')
	c.line = c.line[:0]
	c.col = 0
}
