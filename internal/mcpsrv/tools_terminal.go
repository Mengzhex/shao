package mcpsrv

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Mengzhex/shao/internal/store"
)

// Defaults for the terminal tools. They are set so that a question asked
// without arguments returns something immediately useful rather than either a
// handful of lines or a wall of text.
const (
	defaultTailLines   = 400
	maxTailLines       = 5000
	defaultTailBudget  = 4 << 20
	defaultErrorOutput = 300
)

// errorPattern is the fallback for shells with no integration hook: the
// shapes that error output actually takes across common tooling.
var errorPattern = regexp.MustCompile(
	`(?i)(^|\W)(error|fatal|failed|failure|panic|exception|traceback|refused|denied|not found|no such file|cannot |could not |unable to |timed out|segmentation fault|core dumped)`)

func (s *Server) registerTerminalTools() {
	s.register(toolDef{
		Name:  "list_sessions",
		Title: "List recorded terminal sessions",
		Description: `List the terminal sessions shao has recorded on this machine, newest first.

One endpoint serves every recorded terminal, so this is how you find out which terminals exist. Each entry carries its working directory, terminal title, and the last command it ran with how that ended, which is what tells several open terminals apart. Use it first whenever the question could refer to more than one terminal.

The working directory is the reliable discriminator; titles are often identical across terminals. A "running=" line appears only for bash and zsh, which signal the moment a command starts. PowerShell reports a command only once it finishes, so a PowerShell terminal shows its last completed command and nothing about what is in flight.

By default it returns every terminal still recording, plus any that finished in the last couple of hours, and says how many older ones it left out. Ask for those with all=true or ended_within_hours.

Once you know which terminal is meant, other tools accept it as "cwd:<part of the path>", "label:<name>", or the session id.`,
		InputSchema: schema(map[string]any{
			"include_ended": map[string]any{
				"type":        "boolean",
				"description": "Include sessions that have already finished. Default true.",
				"default":     true,
			},
			"ended_within_hours": propInt(
				"How far back to include finished sessions. Default 2. Negative means no age limit.", 2),
			"all": map[string]any{
				"type":        "boolean",
				"description": "Return every session regardless of age or state.",
				"default":     false,
			},
		}),
		Annotations: readOnlyAnnotations("List recorded terminal sessions"),
	}, s.toolListSessions)

	s.register(toolDef{
		Name:  "get_recent_output",
		Title: "Read recent terminal output",
		Description: `Return the most recent output from a recorded terminal session.

This reads a continuous byte stream captured at the pseudo-terminal, so nothing is missing between one call and the next no matter how fast output was scrolling. Prefer get_last_error when the user is asking why something failed; this tool is for "what has been happening" questions and for reading further around something already found.`,
		InputSchema: schema(map[string]any{
			"session": prop("string", `Which session to read. "latest" (default) picks the most recently active one, preferring a session that is still open. Also accepts "cwd:<part of the path>", "label:<name>", "host:<name>", or a session id. Use list_sessions to see what is available.`),
			"lines":   propInt("How many lines from the end to return. Default 400.", defaultTailLines),
			"format": propEnum(
				`"cooked" (default) applies carriage returns, backspaces and erase sequences so redrawn progress bars collapse to their final state and colour codes are gone. "raw" is the exact bytes, useful only when the escape sequences themselves matter.`,
				[]string{"cooked", "raw"}, "cooked"),
		}),
		Annotations: readOnlyAnnotations("Read recent terminal output"),
	}, s.toolGetRecentOutput)

	s.register(toolDef{
		Name:  "search_output",
		Title: "Search terminal history",
		Description: `Search a session's recorded output backwards from the newest line and return matching lines with surrounding context.

Searching backwards means the most recent occurrence is found first, which is almost always the one being asked about. Use this to locate an error in a long session before reading around it with get_recent_output.`,
		InputSchema: schema(map[string]any{
			"session":       prop("string", `Which session to search. Defaults to "latest".`),
			"pattern":       prop("string", "A Go (RE2) regular expression. Case-sensitive unless you prefix it with (?i)."),
			"context_lines": propInt("Lines of context to include on each side of a match. Default 3.", 3),
			"max_matches":   propInt("Stop after this many matches. Default 20.", 20),
		}, "pattern"),
		Annotations: readOnlyAnnotations("Search terminal history"),
	}, s.toolSearchOutput)

	s.register(toolDef{
		Name:  "get_command_history",
		Title: "List commands run in a session",
		Description: `List the commands run in a session with their exit codes and durations.

This is available only for shells shao could instrument (bash, zsh, PowerShell). For other shells the list is empty while the output itself is still fully recorded. Use only_failed to go straight to what went wrong.`,
		InputSchema: schema(map[string]any{
			"session":     prop("string", `Which session. Defaults to "latest".`),
			"limit":       propInt("How many commands to return, most recent last. Default 50.", 50),
			"only_failed": map[string]any{"type": "boolean", "description": "Return only commands that exited non-zero.", "default": false},
		}),
		Annotations: readOnlyAnnotations("List commands run in a session"),
	}, s.toolGetCommandHistory)

	s.register(toolDef{
		Name:  "get_last_error",
		Title: "Find the most recent failure",
		Description: `Find the most recent command that failed and return it with its exit code and its own output.

This is the tool for "why did that just fail". Because shao records where each command started and ended, the output returned is that command's output specifically, not the last N lines of a busy terminal. If the shell could not be instrumented, this falls back to searching recent output for error-shaped text and says so.`,
		InputSchema: schema(map[string]any{
			"session":   prop("string", `Which session. Defaults to "latest".`),
			"max_lines": propInt("Cap on output lines returned. Default 300.", defaultErrorOutput),
			"skip":      propInt("Skip this many failures to reach an earlier one. Default 0, the most recent.", 0),
		}),
		Annotations: readOnlyAnnotations("Find the most recent failure"),
	}, s.toolGetLastError)
}

type sessionArgs struct {
	Session string `json:"session"`
}

func (s *Server) resolve(selector string) (store.Info, error) {
	return store.Resolve(s.cfg.SessionsDir(), selector)
}

// sessionHeader describes what is being read, so the model can tell whether
// the answer covers the period in question or the buffer had already rolled.
func sessionHeader(info store.Info) string {
	var b strings.Builder
	state := "ended"
	if info.Live() {
		state = "live"
	}
	fmt.Fprintf(&b, "session %s (%s", info.Meta.ID, state)
	if info.Meta.Label != "" {
		fmt.Fprintf(&b, ", label %q", info.Meta.Label)
	}
	if info.Meta.Host != "" {
		fmt.Fprintf(&b, ", host %s", info.Meta.Host)
	}
	fmt.Fprintf(&b, ", shell %s)\n", info.Meta.Shell)
	// With several terminals recorded at once, saying which one this is
	// matters as much as what it contains.
	if info.Meta.CWD != "" {
		fmt.Fprintf(&b, "cwd %s\n", info.Meta.CWD)
	}
	if info.Meta.Title != "" {
		fmt.Fprintf(&b, "title %q\n", info.Meta.Title)
	}
	if info.Running != "" {
		fmt.Fprintf(&b, "currently running: %s (started %s)\n",
			info.Running, humanAgo(info.RunningSince))
	}
	fmt.Fprintf(&b, "buffer holds %s, last activity %s\n",
		humanBytes(info.RawSpan.Bytes()), humanAgo(info.LastActivity))

	switch {
	case len(info.Meta.Argv) > 0:
		// A `shao run` session. It has no shell integration, but it does have
		// one command block covering the whole run with a real exit code, so
		// the generic warning below would contradict what the tools return.
		b.WriteString("note: this session recorded a single command, so there is one command block covering the whole run.\n")
	case info.Commands == 0 && (info.Meta.ShellIntegration == "none" || info.Meta.ShellIntegration == ""):
		b.WriteString("note: this shell could not be instrumented, so command boundaries and exit codes are unavailable for it. Output is still captured in full.\n")
	}
	return b.String()
}

// spanNote surfaces the two conditions a reader must not silently assume away.
func spanNote(sp store.Span) string {
	var notes []string
	if sp.Truncated {
		notes = append(notes, "the ring buffer has discarded older output, so this does not reach back to the start of the session")
	}
	if sp.GapDetected {
		notes = append(notes, "WARNING: a discontinuity was detected in the buffer; some output in the middle of this range is missing")
	}
	if len(notes) == 0 {
		return ""
	}
	return "note: " + strings.Join(notes, "; ") + ".\n"
}

func (s *Server) toolListSessions(raw json.RawMessage) *callToolResult {
	var args struct {
		IncludeEnded     *bool `json:"include_ended"`
		EndedWithinHours *int  `json:"ended_within_hours"`
		All              bool  `json:"all"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return errorResult("%v", err)
	}

	filter := store.DefaultFilter()
	filter.All = args.All
	if args.IncludeEnded != nil {
		filter.IncludeEnded = *args.IncludeEnded
	}
	if args.EndedWithinHours != nil {
		// A negative value is the documented way to ask for no age limit.
		filter.EndedWithin = time.Duration(*args.EndedWithinHours) * time.Hour
	}

	all, err := store.List(s.cfg.SessionsDir())
	if err != nil {
		return errorResult("could not list sessions: %v", err)
	}
	sessions, omitted := filter.Apply(all)

	if len(sessions) == 0 {
		if omitted > 0 {
			return textResult(fmt.Sprintf(
				"No terminal is recording, and no session finished recently. %d older session(s) exist; pass all=true or ended_within_hours to see them.",
				omitted))
		}
		return textResult("No recorded sessions. A session is created by running `shao start`; terminals opened without it are not recorded.")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d session(s), newest first:\n\n", len(sessions))
	for _, info := range sessions {
		b.WriteString(s.describeSessionForList(info))
	}
	// Never let a filtered listing read as if it were everything.
	if omitted > 0 {
		fmt.Fprintf(&b, "\n%d older finished session(s) are not listed. Pass all=true, or "+
			"ended_within_hours=<n>, or name one by id.\n", omitted)
	}
	return textResult(b.String())
}

// describeSessionForList renders one session with enough identity to tell it
// apart from the others.
//
// The working directory leads because it is what a person uses to refer to a
// terminal, and what it is doing comes next. Without these a list of ten
// recorded terminals is ten rows that differ only by timestamp, which is no
// use to anyone deciding which one to look at.
func (s *Server) describeSessionForList(info store.Info) string {
	var b strings.Builder

	state := "ended"
	switch {
	case info.Live():
		state = "live"
	case info.Stale:
		state = "stale"
	}
	fmt.Fprintf(&b, "- id=%s  %s", info.Meta.ID, state)
	if info.Meta.Label != "" {
		fmt.Fprintf(&b, "  label=%s", info.Meta.Label)
	}
	if info.Meta.Host != "" {
		fmt.Fprintf(&b, "  host=%s", info.Meta.Host)
	}
	b.WriteString("\n")

	if cwd := info.Meta.CWD; cwd != "" {
		fmt.Fprintf(&b, "    cwd=%s\n", s.scrub(cwd))
	}
	if title := info.Meta.Title; title != "" {
		fmt.Fprintf(&b, "    title=%q\n", s.scrub(title))
	}

	switch {
	case info.Running != "":
		fmt.Fprintf(&b, "    running=%s  (started %s)\n", s.scrub(info.Running), humanAgo(info.RunningSince))
	case info.LastCommand != "":
		status := "still running"
		if info.LastExit != nil {
			if *info.LastExit == 0 {
				status = "ok"
			} else {
				status = fmt.Sprintf("exit %d", *info.LastExit)
			}
		}
		fmt.Fprintf(&b, "    last=%s -> %s\n", s.scrub(info.LastCommand), status)
	}

	fmt.Fprintf(&b, "    shell=%s  buffered=%s  rate=%.1fMB/min  reaches_back=~%s  commands=%d  activity=%s\n",
		info.Meta.Shell,
		humanBytes(info.RawSpan.Bytes()),
		info.BytesPerMinute/(1<<20),
		humanDuration(info.RetentionEstimate),
		info.Commands,
		humanAgo(info.LastActivity),
	)
	return b.String()
}

func (s *Server) toolGetRecentOutput(raw json.RawMessage) *callToolResult {
	var args struct {
		sessionArgs
		Lines  int    `json:"lines"`
		Format string `json:"format"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return errorResult("%v", err)
	}
	if args.Lines <= 0 {
		args.Lines = defaultTailLines
	}
	if args.Lines > maxTailLines {
		args.Lines = maxTailLines
	}
	stream := store.StreamCooked
	if args.Format == "raw" {
		stream = store.StreamRaw
	}

	info, err := s.resolve(args.Session)
	if err != nil {
		return errorResult("%v", err)
	}

	res, err := store.ReadTailLines(store.StreamDir(info.Dir, stream), args.Lines, defaultTailBudget)
	if err != nil {
		return errorResult("could not read session %s: %v", info.Meta.ID, err)
	}
	if len(res.Lines) == 0 {
		return textResult(sessionHeader(info) + "\nNo output recorded yet.")
	}

	var b strings.Builder
	b.WriteString(sessionHeader(info))
	b.WriteString(spanNote(res.Span))
	if res.FirstLinePartial {
		b.WriteString("note: the first line below is cut off at the start.\n")
	}
	fmt.Fprintf(&b, "\nlast %d line(s) of the %s stream:\n\n", len(res.Lines), stream)
	b.WriteString(s.scrub(strings.Join(res.Lines, "\n")))
	return textResult(b.String())
}

func (s *Server) toolSearchOutput(raw json.RawMessage) *callToolResult {
	var args struct {
		sessionArgs
		Pattern      string `json:"pattern"`
		ContextLines *int   `json:"context_lines"`
		MaxMatches   int    `json:"max_matches"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return errorResult("%v", err)
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return errorResult("pattern is required")
	}
	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return errorResult("invalid regular expression %q: %v", args.Pattern, err)
	}
	contextLines := 3
	if args.ContextLines != nil {
		contextLines = *args.ContextLines
	}
	if args.MaxMatches <= 0 {
		args.MaxMatches = 20
	}

	info, err := s.resolve(args.Session)
	if err != nil {
		return errorResult("%v", err)
	}

	res, err := store.Search(store.StreamDir(info.Dir, store.StreamCooked),
		re, args.MaxMatches, contextLines, store.DefaultSearchBudget)
	if err != nil {
		return errorResult("search failed: %v", err)
	}

	var b strings.Builder
	b.WriteString(sessionHeader(info))
	b.WriteString(spanNote(res.Span))
	if !res.ReachedOldest {
		fmt.Fprintf(&b, "note: searched the most recent %s only; older matches may exist.\n", humanBytes(res.Scanned))
	}
	if res.Capped {
		b.WriteString("note: stopped at max_matches; there are more matches further back.\n")
	}
	if len(res.Matches) == 0 {
		fmt.Fprintf(&b, "\nNo match for %q in %s of output.", args.Pattern, humanBytes(res.Scanned))
		return textResult(b.String())
	}

	fmt.Fprintf(&b, "\n%d match(es), most recent first:\n", len(res.Matches))
	for _, m := range res.Matches {
		fmt.Fprintf(&b, "\n--- %d line(s) from the end ---\n", m.LinesFromEnd)
		for _, l := range m.Before {
			fmt.Fprintf(&b, "  %s\n", s.scrub(l))
		}
		fmt.Fprintf(&b, "> %s\n", s.scrub(m.Line))
		for _, l := range m.After {
			fmt.Fprintf(&b, "  %s\n", s.scrub(l))
		}
	}
	return textResult(b.String())
}

func (s *Server) toolGetCommandHistory(raw json.RawMessage) *callToolResult {
	var args struct {
		sessionArgs
		Limit      int  `json:"limit"`
		OnlyFailed bool `json:"only_failed"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return errorResult("%v", err)
	}
	if args.Limit <= 0 {
		args.Limit = 50
	}

	info, err := s.resolve(args.Session)
	if err != nil {
		return errorResult("%v", err)
	}
	entries, err := store.ReadIndex(info.Dir)
	if err != nil {
		return errorResult("could not read the command index: %v", err)
	}

	var cmds []store.IndexEntry
	for _, e := range entries {
		if e.Kind != store.IndexCmd {
			continue
		}
		if args.OnlyFailed && !e.Failed() {
			continue
		}
		cmds = append(cmds, e)
	}
	if len(cmds) == 0 {
		msg := sessionHeader(info) + "\nNo commands recorded."
		if info.Meta.ShellIntegration == "none" {
			msg += " This shell could not be instrumented; use get_recent_output or search_output instead."
		} else if args.OnlyFailed {
			msg = sessionHeader(info) + "\nNo failed commands in this session."
		}
		return textResult(msg)
	}
	if len(cmds) > args.Limit {
		cmds = cmds[len(cmds)-args.Limit:]
	}

	var b strings.Builder
	b.WriteString(sessionHeader(info))
	fmt.Fprintf(&b, "\n%d command(s), oldest first:\n\n", len(cmds))
	for _, e := range cmds {
		b.WriteString("  " + describeCommand(s.scrub(e.Cmd), e) + "\n")
	}
	return textResult(b.String())
}

func describeCommand(cmd string, e store.IndexEntry) string {
	status := "still running"
	if e.ExitCode != nil {
		if *e.ExitCode == 0 {
			status = "ok"
		} else {
			status = fmt.Sprintf("exit %d", *e.ExitCode)
		}
	} else if e.EndedAt != nil {
		status = "exit status unknown"
	}
	dur := ""
	if e.EndedAt != nil {
		dur = fmt.Sprintf(" in %s", e.EndedAt.Sub(e.StartedAt).Round(time.Millisecond))
	}
	remote := ""
	if e.RemoteHost != "" {
		remote = fmt.Sprintf(" [on %s]", e.RemoteHost)
	}
	return fmt.Sprintf("#%d %s -> %s%s%s", e.Seq, cmd, status, dur, remote)
}

func (s *Server) toolGetLastError(raw json.RawMessage) *callToolResult {
	var args struct {
		sessionArgs
		MaxLines int `json:"max_lines"`
		Skip     int `json:"skip"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return errorResult("%v", err)
	}
	if args.MaxLines <= 0 {
		args.MaxLines = defaultErrorOutput
	}
	if args.Skip < 0 {
		args.Skip = 0
	}

	info, err := s.resolve(args.Session)
	if err != nil {
		return errorResult("%v", err)
	}
	entries, _ := store.ReadIndex(info.Dir)

	var failures []store.IndexEntry
	for _, e := range entries {
		if e.Failed() {
			failures = append(failures, e)
		}
	}
	if len(failures) == 0 {
		return s.lastErrorFallback(info, len(entries) > 0)
	}
	idx := len(failures) - 1 - args.Skip
	if idx < 0 {
		return errorResult("this session has %d recorded failure(s); skip=%d goes past the earliest one", len(failures), args.Skip)
	}
	e := failures[idx]

	var b strings.Builder
	b.WriteString(sessionHeader(info))
	fmt.Fprintf(&b, "\nmost recent failed command%s:\n", skipNote(args.Skip))
	fmt.Fprintf(&b, "  command : %s\n", s.scrub(e.Cmd))
	fmt.Fprintf(&b, "  exit    : %d\n", *e.ExitCode)
	fmt.Fprintf(&b, "  started : %s (%s)\n", e.StartedAt.Format(time.RFC3339), humanAgo(e.StartedAt))
	if e.EndedAt != nil {
		fmt.Fprintf(&b, "  duration: %s\n", e.EndedAt.Sub(e.StartedAt).Round(time.Millisecond))
	}
	if e.RemoteHost != "" {
		fmt.Fprintf(&b, "  ran on  : %s (this command entered a remote host or container)\n", e.RemoteHost)
	}

	res, err := store.ReadRange(store.StreamDir(info.Dir, store.StreamCooked), e.CookedOff, int64(e.CookedLen))
	if err != nil {
		return errorResult("could not read the output of that command: %v", err)
	}
	b.WriteString(spanNote(res.Span))

	lines := strings.Split(strings.TrimRight(string(res.Data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		b.WriteString("\nThat command produced no output of its own.\n")
		return textResult(b.String())
	}
	trimmedNote := ""
	if len(lines) > args.MaxLines {
		trimmedNote = fmt.Sprintf(" (showing the last %d of %d lines)", args.MaxLines, len(lines))
		lines = lines[len(lines)-args.MaxLines:]
	}
	fmt.Fprintf(&b, "\noutput of that command%s:\n\n", trimmedNote)
	b.WriteString(s.scrub(strings.Join(lines, "\n")))
	return textResult(b.String())
}

func skipNote(skip int) string {
	if skip == 0 {
		return ""
	}
	return fmt.Sprintf(" (skipping the %d most recent)", skip)
}

// lastErrorFallback handles sessions with no usable command index: either the
// shell was not instrumented, or nothing has failed. It searches for
// error-shaped output rather than returning nothing, and is explicit that the
// result is a guess rather than an exit code.
func (s *Server) lastErrorFallback(info store.Info, haveIndex bool) *callToolResult {
	var b strings.Builder
	b.WriteString(sessionHeader(info))

	if haveIndex && info.Meta.ShellIntegration != "none" {
		b.WriteString("\nNo command in this session exited non-zero. Searching for error-shaped output anyway:\n")
	} else {
		b.WriteString("\nThis shell has no command index, so exit codes are unavailable. Falling back to searching recent output for error-shaped text; treat the result as a strong hint rather than a confirmed failure.\n")
	}

	res, err := store.Search(store.StreamDir(info.Dir, store.StreamCooked),
		errorPattern, 5, 6, store.DefaultSearchBudget)
	if err != nil {
		return errorResult("fallback search failed: %v", err)
	}
	if len(res.Matches) == 0 {
		b.WriteString("\nNo error-shaped output found in the recent buffer either.")
		return textResult(b.String())
	}
	b.WriteString(spanNote(res.Span))
	for _, m := range res.Matches {
		fmt.Fprintf(&b, "\n--- %d line(s) from the end ---\n", m.LinesFromEnd)
		for _, l := range m.Before {
			fmt.Fprintf(&b, "  %s\n", s.scrub(l))
		}
		fmt.Fprintf(&b, "> %s\n", s.scrub(m.Line))
		for _, l := range m.After {
			fmt.Fprintf(&b, "  %s\n", s.scrub(l))
		}
	}
	return textResult(b.String())
}
