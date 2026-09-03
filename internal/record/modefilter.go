package record

import "bytes"

// A pseudoconsole negotiates with its host by writing private-mode sequences
// into its output. Two of them must never reach the real terminal:
//
//	ESC [ ? 9001 h    win32-input-mode: "send me keystrokes Win32-encoded"
//	ESC [ ? 1004 h    focus reporting:  "tell me when focus changes"
//
// Both are addressed to tmon, which is the pseudoconsole's host. Forwarding
// them instead tells the *user's* terminal to switch input encoding, and then
// its keystrokes arrive as `ESC[...;32;1_` and its focus changes as `ESC[I`,
// which tmon passes straight through to a shell that renders them as literal
// text. The result is a terminal that spews punctuation and cannot be typed
// into -- and it only happens with a real console attached, which is why a
// test harness with pipes on stdout never sees it.
//
// So they are dropped on the way to the screen. They are still written to the
// raw stream, which stays byte-exact.
//
// The cost is that focus reporting is unavailable to full-screen programs
// running inside a recorded shell. That is a fair trade against a terminal
// that cannot be used at all, and tmon cannot tell the two uses apart.
var blockedModes = map[int]bool{
	9001: true,
	1004: true,
}

// maxModeSeq bounds how much can be held back while a sequence is being
// recognised. Real ones are eight bytes; anything longer is not one of these.
const maxModeSeq = 16

type modeState int

const (
	msGround  modeState = iota
	msEsc               // saw ESC
	msBracket           // saw ESC [
	msPrivate           // saw ESC [ ?, collecting digits
)

// modeFilter removes the blocked private-mode sequences from a byte stream,
// tolerating sequences split across reads.
type modeFilter struct {
	st      modeState
	pending []byte // the candidate sequence so far, emitted verbatim if it turns out not to match
	digits  []byte
	out     bytes.Buffer
}

// Write filters a chunk. The returned slice is valid until the next call.
func (f *modeFilter) Write(p []byte) []byte {
	f.out.Reset()
	for _, b := range p {
		f.step(b)
	}
	return f.out.Bytes()
}

// Flush releases a partially recognised sequence, for use when the stream
// ends mid-candidate.
func (f *modeFilter) Flush() []byte {
	f.out.Reset()
	f.release()
	return f.out.Bytes()
}

// release emits whatever was being held back and returns to ground state.
func (f *modeFilter) release() {
	f.out.Write(f.pending)
	f.pending = f.pending[:0]
	f.digits = f.digits[:0]
	f.st = msGround
}

func (f *modeFilter) step(b byte) {
	switch f.st {
	case msGround:
		if b == 0x1b {
			f.st = msEsc
			f.pending = append(f.pending[:0], b)
			return
		}
		f.out.WriteByte(b)

	case msEsc:
		f.pending = append(f.pending, b)
		if b == '[' {
			f.st = msBracket
			return
		}
		// Not a CSI after all. A second ESC starts a fresh candidate rather
		// than being swallowed.
		if b == 0x1b {
			f.out.Write(f.pending[:len(f.pending)-1])
			f.pending = append(f.pending[:0], b)
			return
		}
		f.release()

	case msBracket:
		f.pending = append(f.pending, b)
		if b == '?' {
			f.st = msPrivate
			f.digits = f.digits[:0]
			return
		}
		// An ordinary CSI: nothing to inspect, let it through.
		f.release()

	case msPrivate:
		f.pending = append(f.pending, b)
		switch {
		case b >= '0' && b <= '9':
			f.digits = append(f.digits, b)
			if len(f.pending) > maxModeSeq {
				f.release()
			}
		case b == 'h' || b == 'l':
			if f.blocked() {
				// Drop the whole sequence.
				f.pending = f.pending[:0]
				f.digits = f.digits[:0]
				f.st = msGround
				return
			}
			f.release()
		default:
			// A parameter list, an intermediate byte, or something else. Only
			// the single-parameter form is recognised, so pass it on.
			f.release()
		}
	}
}

func (f *modeFilter) blocked() bool {
	if len(f.digits) == 0 {
		return false
	}
	n := 0
	for _, d := range f.digits {
		n = n*10 + int(d-'0')
		if n > 100000 {
			return false
		}
	}
	return blockedModes[n]
}
