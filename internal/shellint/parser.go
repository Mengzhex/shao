package shellint

import "strings"

// EventKind is the type of a shell-integration marker found in the stream.
type EventKind int

const (
	// EvPromptStart means a prompt was drawn (OSC 133;A).
	EvPromptStart EventKind = iota
	// EvOutputStart means a command's output begins here (OSC 133;C).
	EvOutputStart
	// EvCommandEnd means the command finished (OSC 133;D).
	EvCommandEnd
	// EvCommandText carries the command line (OSC 7331;cmd).
	EvCommandText
	// EvVersion announces that integration is installed (OSC 7331;ver).
	EvVersion
	// EvTitle carries the terminal title (OSC 0 or OSC 2).
	//
	// This one costs nothing to collect: a pseudoconsole sets the title on its
	// own, so every recorded session already has these in its stream whether
	// or not the shell cooperates.
	EvTitle
	// EvCWD carries the working directory, either as OSC 7 (which terminals
	// on macOS and Linux commonly emit) or as shao's own OSC 7331;cwd, which
	// is the only reliable source on Windows.
	EvCWD
)

// Event is one marker, with the position in the fed chunk just past its
// terminator. The recorder adds the chunk's base stream offset to Pos to get
// the absolute offset at which the marker took effect, which is what makes a
// command block's byte range exact.
type Event struct {
	Kind EventKind
	Pos  int

	Cmd   string
	Title string
	CWD   string

	ExitCode int
	// ExitKnown distinguishes "exited 0" from a D marker with no status,
	// which is what a shell emits if it was interrupted oddly.
	ExitKnown bool
}

type parserState int

const (
	psGround parserState = iota
	psEsc
	psOSC
	psOSCEsc
)

// maxPayload bounds marker accumulation. A stray ESC ] in binary output would
// otherwise make the parser buffer the rest of the session looking for a
// terminator that never comes.
const maxPayload = 64 << 10

// Parser extracts markers from a raw stream incrementally, tolerating
// sequences split across chunk boundaries.
//
// It is fed the raw stream rather than the cooked one on purpose: cooking
// discards all OSC sequences, markers included.
type Parser struct {
	st       parserState
	payload  []byte
	overflow bool // current payload exceeded maxPayload and is being dropped
}

func NewParser() *Parser { return &Parser{} }

// Feed consumes a chunk and returns the markers it contained, in order.
func (p *Parser) Feed(chunk []byte) []Event {
	var events []Event
	for i, b := range chunk {
		switch p.st {
		case psGround:
			if b == 0x1b {
				p.st = psEsc
			}
		case psEsc:
			if b == ']' {
				p.st = psOSC
				p.payload = p.payload[:0]
				p.overflow = false
			} else if b == 0x1b {
				// Another ESC: stay here and reinterpret.
			} else {
				p.st = psGround
			}
		case psOSC:
			switch b {
			case 0x07: // BEL terminator
				if e, ok := p.finish(i + 1); ok {
					events = append(events, e)
				}
			case 0x1b:
				p.st = psOSCEsc
			default:
				if len(p.payload) < maxPayload {
					p.payload = append(p.payload, b)
				} else {
					p.overflow = true
				}
			}
		case psOSCEsc:
			if b == '\\' { // ESC \ terminator
				if e, ok := p.finish(i + 1); ok {
					events = append(events, e)
				}
			} else {
				// A stray ESC inside the payload; treat the sequence as
				// abandoned rather than swallowing the rest of the stream.
				p.st = psGround
				p.payload = p.payload[:0]
			}
		}
	}
	return events
}

func (p *Parser) finish(pos int) (Event, bool) {
	payload := string(p.payload)
	overflowed := p.overflow
	p.st = psGround
	p.payload = p.payload[:0]
	p.overflow = false
	if overflowed {
		return Event{}, false
	}
	return parsePayload(payload, pos)
}

// parsePayload interprets one OSC payload. Anything that is not a shao or
// OSC 133 marker is silently ignored: window-title and hyperlink sequences
// come through here constantly.
func parsePayload(payload string, pos int) (Event, bool) {
	// Split on the first semicolon only. A title is arbitrary text and
	// routinely contains semicolons, so splitting it up front would truncate
	// it; the codes that do carry structured parameters split again below.
	code, rest, ok := strings.Cut(payload, ";")
	if !ok {
		return Event{}, false
	}

	switch code {
	case "0", "2":
		// OSC 0 sets icon name and title, OSC 2 sets the title. Either is a
		// usable human label for a terminal.
		title := strings.TrimSpace(rest)
		if title == "" {
			return Event{}, false
		}
		return Event{Kind: EvTitle, Pos: pos, Title: title}, true

	case "7":
		dir := parseFileURL(rest)
		if dir == "" {
			return Event{}, false
		}
		return Event{Kind: EvCWD, Pos: pos, CWD: dir}, true
	}

	parts := strings.SplitN(payload, ";", 3)
	if len(parts) < 2 {
		return Event{}, false
	}
	switch parts[0] {
	case "133":
		switch parts[1] {
		case "A":
			return Event{Kind: EvPromptStart, Pos: pos}, true
		case "C":
			return Event{Kind: EvOutputStart, Pos: pos}, true
		case "D":
			e := Event{Kind: EvCommandEnd, Pos: pos}
			if len(parts) == 3 {
				if code, ok := formatExit(parts[2]); ok {
					e.ExitCode, e.ExitKnown = code, true
				}
			}
			return e, true
		}
	case "7331":
		if len(parts) < 3 {
			return Event{}, false
		}
		switch parts[1] {
		case "cmd", "cmdraw":
			cmd := DecodeCommand(parts[1], parts[2])
			if cmd == "" {
				return Event{}, false
			}
			return Event{Kind: EvCommandText, Pos: pos, Cmd: cmd}, true
		case "cwd", "cwdraw":
			// Base64 for the same reason the command line is: a Windows path
			// is full of backslashes and could contain anything.
			dir := decodeMarker(parts[1] == "cwdraw", parts[2])
			if dir == "" {
				return Event{}, false
			}
			return Event{Kind: EvCWD, Pos: pos, CWD: dir}, true
		case "ver":
			return Event{Kind: EvVersion, Pos: pos}, true
		}
	}
	return Event{}, false
}
