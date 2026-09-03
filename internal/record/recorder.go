package record

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"time"

	"tmon/internal/config"
	"tmon/internal/cook"
	"tmon/internal/redact"
	"tmon/internal/shellint"
	"tmon/internal/store"
)

// Version identifies the recorder that produced a session, so a buffer
// written by an older build can be recognised later.
const Version = "1"

// Options configures one recorded session.
type Options struct {
	Cfg *config.Config
	// Label names the session so it can be asked about by name later,
	// e.g. `tmon shell --label deploy`.
	Label string
	// Host attributes the session to a configured host.
	Host string
	// Shell overrides which shell to record.
	Shell string
	// Args, when set, runs one command instead of an interactive shell,
	// which is what `tmon run -- ./deploy.sh` uses.
	Args []string
	// Quiet suppresses the banner and closing summary.
	Quiet bool
	// Out receives the banner and summary; defaults to stderr so they never
	// pollute piped stdout.
	Out io.Writer
}

// Run records a session and returns the shell's exit code.
func Run(ctx context.Context, opts Options) (int, error) {
	cfg := opts.Cfg
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}

	// Reclaim old sessions before adding another. Doing it here rather than on
	// a timer keeps tmon strictly demand-driven: nothing runs in the
	// background when no session is being recorded.
	if days := cfg.Buffer.RetainDays; days > 0 {
		_, _ = store.PruneOlderThan(cfg.SessionsDir(), time.Duration(days)*24*time.Hour)
	}
	_ = store.EnforceGlobalCap(cfg.SessionsDir(), cfg.Buffer.GlobalDiskCap)

	oneShot := len(opts.Args) > 0
	shell := ResolveShell(opts.Shell)

	kind := shellint.KindNone
	var launch shellint.Launch
	if !oneShot {
		kind = shellint.Detect(shell)
		var err error
		if launch, err = shellint.Prepare(kind, cfg.Root()); err != nil {
			return -1, fmt.Errorf("prepare shell integration: %w", err)
		}
		if launch.TempDir != "" {
			defer os.RemoveAll(launch.TempDir)
		}
	}

	con := newConsole()
	cols, rows := con.Size()

	host := opts.Host
	if host == "" {
		host = cfg.HostForLabel(opts.Label)
	}

	red, err := redact.New(cfg.Redact)
	if err != nil {
		return -1, err
	}

	meta := store.Meta{
		ID:               store.NewSessionID(time.Now(), os.Getpid()),
		Label:            opts.Label,
		Host:             host,
		Shell:            shell,
		PID:              os.Getpid(),
		StartedAt:        time.Now(),
		Cols:             cols,
		Rows:             rows,
		Platform:         runtime.GOOS,
		MaxBytes:         cfg.MaxBytesForLabel(opts.Label),
		CookedMaxBytes:   cfg.Buffer.CookedMaxBytes,
		SegmentBytes:     cfg.Buffer.SegmentBytes,
		RedactionOn:      !red.Disabled(),
		ShellIntegration: string(kind),
		RecorderVersion:  Version,
	}
	if oneShot {
		// The argv is stored in metadata, which the stream redactor never
		// sees, so it is scrubbed here. `tmon run -- mysql -psecret ...` is
		// exactly the case this covers.
		meta.Argv = make([]string, len(opts.Args))
		for i, a := range opts.Args {
			meta.Argv[i] = string(red.All([]byte(a)))
		}
	}

	sess, err := store.Create(cfg.SessionsDir(), meta, cfg.Buffer.CookedMaxBytes)
	if err != nil {
		return -1, fmt.Errorf("create session: %w", err)
	}

	exe, args := shell, launch.Args
	if oneShot {
		exe, args = opts.Args[0], opts.Args[1:]
	}

	if !opts.Quiet {
		printBanner(out, meta, kind, oneShot)
	}

	if err := con.Enter(); err != nil {
		sess.Close(nil)
		return -1, fmt.Errorf("put terminal in raw mode: %w", err)
	}
	defer con.Leave()

	// The recorded shell is marked so that a profile hook installed by
	// `tmon hook install` does not try to record it again and recurse.
	childEnv := append(append([]string{}, launch.Env...),
		"TMON_RECORDING=1",
		"TMON_SESSION_ID="+meta.ID,
	)

	pty, err := startPTY(exe, args, childEnv, cols, rows)
	if err != nil {
		con.Leave()
		sess.Close(nil)
		return -1, err
	}

	r := &recorder{
		sess:   sess,
		red:    red,
		cooker: cook.New(),
		parser: shellint.NewParser(),
		stdout: os.Stdout,
		// Seeded so idleFor() is meaningful before the first byte arrives.
		lastWrite: time.Now(),
	}
	if oneShot {
		// DetectRemoteHost reads the unredacted command, because a host name
		// is not a secret and redaction could obscure it; only what gets
		// stored is scrubbed.
		raw := strings.Join(opts.Args, " ")
		r.sess.CommandStarted(string(red.All([]byte(raw))), shellint.DetectRemoteHost(raw))
		r.cmdOpen = true
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Ctrl+C reaches the shell as a byte because the terminal is raw, so
	// these signals only arrive if something else sends them. Restoring the
	// terminal on the way out matters more than a clean shutdown.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	go func() {
		select {
		case <-runCtx.Done():
		case <-sigs:
			pty.Close()
		}
		signal.Stop(sigs)
	}()

	con.WatchResize(runCtx, func(c, rw int) {
		_ = pty.Resize(c, rw)
		_ = sess.Resize(c, rw)
	})

	// Watch for `tmon end` in another process. Closing the pty ends the shell
	// and the pump loop drains normally, so the session is finalised the same
	// way as if the user had typed exit.
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if store.StopRequested(sess.Dir()) {
					_ = sess.Note("stopped by `tmon end`")
					pty.Close()
					return
				}
			}
		}
	}()

	// Keystrokes to the shell. This goroutine outlives the loop below: a
	// blocking read on the real stdin cannot be interrupted portably, and the
	// process exits immediately afterwards anyway.
	go func() { _, _ = io.Copy(pty, con.in) }()

	go r.flushLoop(runCtx, cfg.Buffer)

	// Reaping has to happen in parallel with the pump below, not after it.
	//
	// On Windows the pseudoconsole holds its own end of the output pipe open
	// until it is closed, so the read loop never sees EOF by itself: waiting
	// for the loop to finish before closing would deadlock. Closing is
	// therefore the waiter's job.
	type waitResult struct {
		code int
		err  error
	}
	waitCh := make(chan waitResult, 1)
	go func() {
		code, werr := pty.Wait()
		// Do not close the instant the process exits: output it already
		// wrote can still be in flight through the console host, and closing
		// would discard the tail of the session, which is the part most
		// likely to be asked about.
		//
		// Both conditions are needed. A minimum grace period covers the case
		// where nothing has been read yet, where an "idle" measurement would
		// otherwise report a long silence and close immediately. The idle
		// check then covers the opposite case, a process that exits while
		// still flushing a lot of output.
		exitedAt := time.Now()
		deadline := exitedAt.Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if time.Since(exitedAt) >= 150*time.Millisecond && r.idleFor() >= 100*time.Millisecond {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		pty.Close()
		waitCh <- waitResult{code, werr}
	}()

	// The pump. Reading here, rather than in a goroutine, keeps the ordering
	// of everything downstream trivially correct.
	buf := make([]byte, 64<<10)
	for {
		n, err := pty.Read(buf)
		if n > 0 {
			r.process(buf[:n])
		}
		if err != nil {
			break // EOF, once the shell has exited and the pty is closed
		}
	}

	res := <-waitCh
	exitCode, waitErr := res.code, res.err
	cancel()

	r.finish(exitCode)
	con.Leave()

	if !opts.Quiet {
		printSummary(out, sess, exitCode)
	}
	return exitCode, waitErr
}

// recorder owns the write side of a session. Every field here is touched
// under mu, because the flush loop runs concurrently with the pump.
type recorder struct {
	mu     sync.Mutex
	sess   *store.Session
	red    *redact.Redactor
	cooker *cook.Cooker
	parser *shellint.Parser
	stdout io.Writer
	// screen filters what reaches the user's terminal. The recording keeps
	// every byte; the screen must not receive the pseudoconsole's private
	// negotiation with tmon.
	screen modeFilter

	lastWrite time.Time
	lastFlush time.Time

	pendingCmd  string
	cmdOpen     bool
	promptKnown bool
	promptRaw   uint64
	promptCook  uint64
	promptAt    time.Time
}

// process handles one chunk read from the pty.
func (r *recorder) process(chunk []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// The user's terminal is served first: recording is a bystander, and if
	// anything below were slow or broken the session would still behave like
	// an ordinary shell. The only thing withheld is the pseudoconsole's
	// private mode negotiation, which is addressed to tmon and would break
	// the terminal if forwarded (see modefilter.go).
	if visible := r.screen.Write(chunk); len(visible) > 0 {
		_, _ = r.stdout.Write(visible)
	}

	events := r.parser.Feed(chunk)
	pos := 0
	for _, ev := range events {
		r.writeSpan(chunk[pos:ev.Pos])
		// Release anything the redactor is holding before reading offsets,
		// so a command block's byte range lines up exactly with the marker
		// rather than trailing it by a partial line. Markers land at prompt
		// boundaries, where there is nothing sensitive to hold back anyway.
		r.releaseHold()
		r.handleEvent(ev)
		pos = ev.Pos
	}
	r.writeSpan(chunk[pos:])
	r.lastWrite = time.Now()
}

// idleFor reports how long it has been since output last arrived. The exit
// path uses it to tell "the shell is done" from "the shell is done but its
// last output has not come through yet".
func (r *recorder) idleFor() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Since(r.lastWrite)
}

func (r *recorder) writeSpan(b []byte) {
	if len(b) == 0 {
		return
	}
	r.emit(r.red.Process(b))
}

func (r *recorder) releaseHold() {
	r.emit(r.red.Flush())
}

// emit writes redacted bytes to both streams. The cooked stream is derived
// from the redacted bytes, never from the original, so a secret cannot
// survive in one stream after being removed from the other.
func (r *recorder) emit(b []byte) {
	if len(b) == 0 {
		return
	}
	_, _ = r.sess.WriteRaw(b)
	if cooked := r.cooker.Write(b); len(cooked) > 0 {
		_, _ = r.sess.WriteCooked(cooked)
	}
}

func (r *recorder) handleEvent(ev shellint.Event) {
	switch ev.Kind {
	case shellint.EvVersion:
		// Integration confirmed live; nothing to record beyond metadata.

	case shellint.EvCommandText:
		// Redact the command line itself, not just the output it produces.
		// A credential passed as an argument would otherwise be stored in
		// the clear in the command index, which redaction of the stream
		// never touches.
		r.pendingCmd = string(r.red.All([]byte(ev.Cmd)))

	case shellint.EvCWD:
		// Redacted like the command line, and for the same reason: these end
		// up in metadata, which the stream redactor never sees. A checkout
		// directory named after a token would otherwise be stored in the
		// clear. This exact gap was already found once on command text.
		r.sess.SetCWD(string(r.red.All([]byte(ev.CWD))))

	case shellint.EvTitle:
		r.sess.SetTitle(string(r.red.All([]byte(ev.Title))))

	case shellint.EvPromptStart:
		r.promptRaw = r.sess.RawOffset()
		r.promptCook = r.sess.CookedOffset()
		r.promptAt = time.Now()
		r.promptKnown = true

	case shellint.EvOutputStart:
		remote := shellint.DetectRemoteHost(r.pendingCmd)
		r.sess.CommandStarted(r.pendingCmd, remote)
		r.cmdOpen = true
		r.pendingCmd = ""

	case shellint.EvCommandEnd:
		var exit *int
		if ev.ExitKnown {
			code := ev.ExitCode
			exit = &code
		}
		switch {
		case r.cmdOpen:
			if exit != nil {
				r.sess.CommandFinished(*exit)
			} else {
				r.sess.CommandFinished(-1)
			}
			r.cmdOpen = false
		case r.promptKnown && r.pendingCmd != "":
			// A shell with no output-start marker (PowerShell). The command
			// ran between the previous prompt and here.
			r.sess.RecordCommandSpan(r.pendingCmd, shellint.DetectRemoteHost(r.pendingCmd),
				r.promptRaw, r.promptCook, r.promptAt, exit)
		}
		r.pendingCmd = ""
	}
}

// flushLoop bounds how stale a reader in another process can be.
//
// Two rules run together. A steady flush keeps a busy session no more than
// FlushInterval behind. An idle flush fires as soon as output stops, which is
// the case that actually matters: the moment a command finishes and someone
// turns to ask what went wrong, the buffer is already complete.
func (r *recorder) flushLoop(ctx context.Context, buf config.BufferConfig) {
	idle := time.Duration(buf.IdleFlushMS) * time.Millisecond
	interval := time.Duration(buf.FlushIntervalMS) * time.Millisecond
	if idle <= 0 {
		idle = 50 * time.Millisecond
	}
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}

	ticker := time.NewTicker(idle)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			r.mu.Lock()
			pending := r.sess.Buffered() + r.red.Pending()
			shouldFlush := pending > 0 &&
				(now.Sub(r.lastWrite) >= idle || now.Sub(r.lastFlush) >= interval)
			if shouldFlush {
				// Hold-back bytes are released here too, otherwise the last
				// line before an idle prompt would sit in memory unread.
				r.releaseHold()
				_ = r.sess.Flush()
				r.lastFlush = now
			}
			r.mu.Unlock()
		}
	}
}

// finish drains everything still buffered and closes the session.
func (r *recorder) finish(exitCode int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.releaseHold()
	// Release any partially recognised escape sequence to the screen so the
	// last thing displayed is not silently swallowed.
	if tail := r.screen.Flush(); len(tail) > 0 {
		_, _ = r.stdout.Write(tail)
	}
	// The final line often has no trailing newline (a prompt, or output cut
	// short by the shell exiting), so the cooker is drained explicitly.
	if tail := r.cooker.Flush(); len(tail) > 0 {
		_, _ = r.sess.WriteCooked(tail)
	}
	if r.cmdOpen {
		r.sess.CommandFinished(exitCode)
		r.cmdOpen = false
	}
	_ = r.sess.Flush()
	_ = r.sess.Close(&exitCode)
}

func printBanner(w io.Writer, meta store.Meta, kind shellint.Kind, oneShot bool) {
	what := "shell"
	if oneShot {
		what = "command"
	}
	fmt.Fprintf(w, "tmon: recording this %s as session %s", what, meta.ID)
	if meta.Label != "" {
		fmt.Fprintf(w, " (label %s)", meta.Label)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "tmon: buffer %s, redaction %s, command index %s\r\n",
		humanSize(meta.MaxBytes),
		onOff(meta.RedactionOn),
		integrationNote(kind),
	)
	fmt.Fprintf(w, "tmon: exit the %s to stop recording\r\n\r\n", what)
}

func integrationNote(kind shellint.Kind) string {
	if kind == shellint.KindNone {
		return "off (shell not recognised; output is still captured in full)"
	}
	return "on (" + string(kind) + ")"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func printSummary(w io.Writer, sess *store.Session, exitCode int) {
	info, err := store.Describe(sess.Dir())
	if err != nil {
		fmt.Fprintf(w, "\r\ntmon: session %s ended (exit %d)\r\n", sess.Meta().ID, exitCode)
		return
	}
	fmt.Fprintf(w, "\r\ntmon: session %s ended (exit %d), %s captured, %d commands indexed\r\n",
		info.Meta.ID, exitCode, humanSize(info.RawSpan.Bytes()), info.Commands)
	fmt.Fprintf(w, "tmon: ask about it with the MCP tools, or run `tmon tail %s`\r\n", info.Meta.ID)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
