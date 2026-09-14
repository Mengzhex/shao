package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mengzhex/shao/internal/config"
	"github.com/Mengzhex/shao/internal/mcpsrv"
	"github.com/Mengzhex/shao/internal/record"
	"github.com/Mengzhex/shao/internal/store"
)

// cmdMCP speaks MCP on stdin/stdout. An AI client launches this itself, so
// the process lives exactly as long as the client keeps the pipe open.
func cmdMCP(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	check(fs.Parse(args))

	srv, err := mcpsrv.New(cfg)
	check(err)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Stdout carries the protocol; anything else written there would corrupt
	// the stream, so diagnostics go to stderr.
	if err := srv.ServeStdio(ctx, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
		fail("%v", err)
	}
}

// cmdStart is the one command a new user needs.
//
// It does the three things that have to happen for shao to be useful, so that
// none of them has to be discovered separately: it makes every new terminal
// record itself from now on, it prints the configuration to paste into an AI
// client, and it starts recording the terminal it was run in.
//
// The last part matters for expectations. Recording can only begin when a
// shell begins, so a command that claims to "start" monitoring has to hand
// back a recorded shell, otherwise the terminal it was typed into is the one
// terminal still not being recorded.
func cmdStart(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	label := fs.String("label", "", "name this session so it can be asked about by name later")
	shell := fs.String("shell", "", "record a specific shell instead of your default")
	noHook := fs.Bool("no-hook", false, "do not touch shell startup files; only record this terminal")
	port := fs.Int("port", noEndpoint, "serve the URL endpoint on this port instead of the default")
	bind := fs.String("bind", "", "address for the URL endpoint. Loopback unless set. 0.0.0.0 lets another machine connect")
	allow := fs.String("allow", "", "comma-separated IPs or CIDRs permitted to connect to the URL endpoint")
	noEndpointFlag := fs.Bool("no-endpoint", false, "do not start or reuse the shared URL endpoint")
	parseFlags(fs, args)

	// The endpoint is shared: one of them serves every recorded terminal, so
	// the first `shao start` brings it up and every later one finds it already
	// running. That is the whole answer to "how do I watch several terminals" --
	// there is nothing per-terminal to configure, and nothing to clean up.
	//
	// It is on by default because the alternative was remembering a flag on
	// whichever terminal happened to be first.
	wantEndpoint := !*noEndpointFlag
	if wantEndpoint && *port == noEndpoint {
		*port = cfg.MCP.HTTP.Port
	}

	// Running `shao start` inside an already-recorded shell is not a mistake
	// worth an error: it is what someone types to check whether monitoring is
	// on. So answer that question rather than just refusing.
	//
	// An endpoint request still has to be honoured. This early return has
	// swallowed that flag twice now -- once as --http, then again as --port --
	// which is what an early return does if it is written before the flags it
	// has to account for. Anything added here needs handling above it.
	if os.Getenv("SHAO_RECORDING") == "1" {
		if wantEndpoint {
			serveDetached(cfg, *port, *bind, *allow)
			fmt.Println()
		}
		reportAlreadyOn(cfg, os.Getenv("SHAO_SESSION_ID"))
		return
	}

	// Step 1: cover every future terminal, once.
	if !*noHook {
		ensureHook()
	}

	// Step 2: bring up the URL endpoint if one was asked for. It is started
	// detached, so it outlives the shell recorded below: someone who asked
	// for an endpoint wants it to stay up, not to vanish when they type exit.
	if wantEndpoint {
		serveDetached(cfg, *port, *bind, *allow)
		fmt.Println()
	}

	// Step 3: tell the user what to paste, once.
	fmt.Println("Connect your AI client once:")
	fmt.Println()
	printStdioConfig()
	fmt.Println()
	if wantEndpoint {
		fmt.Println("Every terminal you record shows up through that one endpoint. Ask your AI to")
		fmt.Println("list them; there is nothing per-terminal to configure.")
	} else {
		fmt.Println("If your client needs a URL instead, drop --no-endpoint.")
	}
	fmt.Println()
	fmt.Println("Nothing is monitored and nothing is pushed: the tools run only when you ask")
	fmt.Println("your AI a question and it calls them.")
	fmt.Println()

	// Step 4: record this terminal too, so the window this was typed into is
	// not the one blind spot.
	code, err := record.Run(context.Background(), record.Options{
		Cfg:   cfg,
		Label: *label,
		Shell: *shell,
	})
	if err != nil {
		fail("%v", err)
	}
	os.Exit(code)
}

// reportAlreadyOn answers "is monitoring on?" for someone who typed
// `shao start` in a shell that is already recorded.
//
// It is a status report, not onboarding, so it does not reprint the client
// configuration. Someone checking whether monitoring is on has already
// connected their client, or can ask for the configuration by name; dumping a
// JSON block they did not request on every check is noise, and it buries the
// three lines they actually wanted.
func reportAlreadyOn(cfg *config.Config, sessionID string) {
	fmt.Println("Monitoring is on.")
	fmt.Println()

	fmt.Printf("  this terminal   recording as %s\n", sessionID)

	hookOn := false
	for _, t := range hookTargets("") {
		if data, err := os.ReadFile(t.path); err == nil && containsHook(string(data)) {
			hookOn = true
			break
		}
	}
	if hookOn {
		fmt.Println("  new terminals   record themselves automatically")
	} else {
		fmt.Println("  new terminals   NOT covered; run `shao hook install` to include them")
	}

	// How much is actually captured matters more than the fact that a session
	// exists, so report it rather than leaving it to be looked up.
	if sessions, err := store.List(cfg.SessionsDir()); err == nil {
		var live, total int
		for _, s := range sessions {
			total++
			if s.Live() {
				live++
			}
		}
		fmt.Printf("  sessions        %d recording, %d recorded in total\n", live, total)
	}

	// The endpoint is the part most likely to be the actual question, since
	// it is the half that can be running or not independently of recording.
	if state, running := mcpsrv.ReadServeState(cfg); running {
		fmt.Printf("  URL endpoint    %s\n", state.URL)
	} else {
		fmt.Println("  URL endpoint    not running (it should be; check `shao serve --status`)")
	}

	fmt.Println()
	fmt.Println("Just ask your AI about what happened here. To stop, run `shao end`.")
	fmt.Println("Client configuration, if you need it again: `shao mcp-config`.")
}

// cmdServe runs only the HTTP endpoint, for the case where the machine should
// answer an AI client without a shell being recorded in this window.
func cmdServe(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", cfg.MCP.HTTP.Port, "port to listen on; 0 lets the OS choose")
	bind := fs.String("bind", "", "address to listen on. Empty means loopback only. Pass 0.0.0.0 or a LAN address to let another machine connect")
	allow := fs.String("allow", "", "comma-separated IPs or CIDRs permitted to connect, e.g. 192.168.1.0/24. Empty means anything that can reach the port")
	stop := fs.Bool("stop", false, "stop the running endpoint and exit")
	status := fs.Bool("status", false, "report whether an endpoint is running, and exit")
	foreground := fs.Bool("foreground", false, "stay in the foreground instead of backgrounding, for watching the log live")
	parseFlags(fs, args)

	switch {
	case *status:
		serveStatus(cfg)
		return
	case *stop:
		serveStop(cfg)
		return
	case *foreground:
		serveUntilInterrupt(cfg, *port, *bind, *allow)
		return
	}

	// Backgrounding is the default: an endpoint has nothing more to say once
	// it has printed its URL, and holding the terminal it was started from is
	// pure cost to whoever wanted to type the next command.
	serveDetached(cfg, *port, *bind, *allow)
}

// noEndpoint is the --port value meaning "do not start a URL endpoint". It is
// negative because 0 already means "let the OS choose a port".
const noEndpoint = -1

// serveChildEnv marks the backgrounded server process, so it can recognise
// itself and refuse to background again.
const serveChildEnv = "SHAO_SERVE_CHILD"

// serveDetached starts the endpoint as a background process and returns.
//
// A serving process has nothing to say after it has printed its URL, so
// occupying the terminal it was started from is pure cost: the obvious next
// thing anyone wants to do is type another command. The child is detached from
// this console so it neither writes into a terminal someone is using nor dies
// when that terminal closes.
func serveDetached(cfg *config.Config, port int, bind, allow string) {
	// The child must never take this path itself. Backgrounding is the
	// default, so a child that reached it would spawn a child of its own, and
	// so on: the first version of this spawned 51 processes in a few seconds.
	// The child is passed --foreground, and this marker makes a mistake in
	// that argument list an error rather than a fork bomb.
	if os.Getenv(serveChildEnv) == "1" {
		fail("internal error: a backgrounded endpoint tried to background itself.\n" +
			"Run `shao serve --foreground` directly.")
	}

	if state, running := mcpsrv.ReadServeState(cfg); running {
		fmt.Printf("An endpoint is already running on %s (pid %d).\n", state.URL, state.PID)
		fmt.Println("Stop it first with `shao serve --stop`, or leave it as it is.")
		return
	}

	exe, err := os.Executable()
	check(err)

	// --foreground is what stops the recursion: the child serves, it does not
	// background itself again.
	childArgs := []string{"--home", cfg.Root(), "serve", "--foreground", "--port", fmt.Sprint(port)}
	if bind != "" {
		childArgs = append(childArgs, "--bind", bind)
	}
	if allow != "" {
		childArgs = append(childArgs, "--allow", allow)
	}

	// The child has no terminal, so its output goes to a log. Without it, a
	// server that failed to start would fail invisibly.
	logPath := filepath.Join(cfg.Root(), "serve.log")
	logFile, err := config.CreatePrivateFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
	check(err)
	defer logFile.Close()

	cmd := exec.Command(exe, childArgs...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), serveChildEnv+"=1")
	cmd.SysProcAttr = detachedAttr()
	if err := cmd.Start(); err != nil {
		fail("could not start the background endpoint: %v", err)
	}
	// Release the child rather than waiting for it.
	_ = cmd.Process.Release()

	// Report what actually happened, not what was requested.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if state, running := mcpsrv.ReadServeState(cfg); running {
			token, err := mcpsrv.LoadOrCreateToken(cfg)
			check(err)
			fmt.Printf("Serving in the background, pid %d. Log: %s\n", state.PID, logPath)
			fmt.Println()
			printHTTPConfigs(state.URL, token)
			printDetachedReach(state, token)
			fmt.Println()
			fmt.Println("This endpoint records nothing. It only serves what was already")
			fmt.Println("recorded, and only when your AI calls a tool. Recording is `shao start`,")
			fmt.Println("run in each terminal you want captured.")
			fmt.Println()
			fmt.Println("The terminal is yours again. Stop the endpoint with `shao serve --stop`.")
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	fail("the background endpoint did not come up within 10s. Its log says:\n%s", tailFile(logPath, 20))
}

// printDetachedReach reports reach from the recorded state, since the parent
// process does not hold the listener.
func printDetachedReach(state mcpsrv.ServeState, token string) {
	if len(state.LANURLs) == 0 {
		fmt.Println()
		fmt.Println("Reachable from this machine only.")
		return
	}
	fmt.Println()
	fmt.Println("Reachable from your network. For an agent on another machine:")
	fmt.Println()
	for _, base := range state.LANURLs {
		fmt.Printf("  %s/mcp   (Streamable HTTP)\n", base)
		fmt.Printf("  %s/sse?token=%s   (SSE)\n", base, token)
	}
	fmt.Println()
	fmt.Println("This is plain HTTP: recorded output and the token are readable by anything")
	fmt.Println("that can observe the network. Treat the reach of this port as the reach of")
	fmt.Println("your scrollback.")
	if state.Allow != "" {
		fmt.Printf("Only these clients may connect: %s\n", state.Allow)
	}
}

func tailFile(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no log available: " + err.Error() + ")"
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

func serveStatus(cfg *config.Config) {
	state, running := mcpsrv.ReadServeState(cfg)
	if !running {
		if state.PID != 0 {
			fmt.Printf("No endpoint running. A record of pid %d was left behind by a server that was killed.\n", state.PID)
			mcpsrv.ClearServeState(cfg)
			return
		}
		fmt.Println("No endpoint running. Start one with `shao serve`.")
		return
	}
	fmt.Printf("Serving on %s (pid %d, since %s).\n", state.URL, state.PID,
		state.StartedAt.Format("2006-01-02 15:04:05"))
	fmt.Println("Stop it with `shao serve --stop`.")
}

// serveStop asks the endpoint to shut down and waits to confirm it did, rather
// than reporting success on the strength of having written a file.
func serveStop(cfg *config.Config) {
	state, running := mcpsrv.ReadServeState(cfg)
	if !running {
		if state.PID != 0 {
			mcpsrv.ClearServeState(cfg)
			fmt.Println("No endpoint running; cleared a record left behind by a server that was killed.")
			return
		}
		fmt.Println("No endpoint running.")
		return
	}

	check(mcpsrv.RequestServeStop(cfg))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, still := mcpsrv.ReadServeState(cfg); !still {
			fmt.Printf("Stopped the endpoint on %s.\n", state.URL)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	fail("the endpoint on %s (pid %d) did not stop within 5s.\n"+
		"Stop that one process by pid if it is wedged. Do not kill shao by image name:\n"+
		"every recorded terminal runs a shao process too, and killing those ends those shells.",
		state.URL, state.PID)
}

// serveUntilInterrupt starts the HTTP endpoint, prints how to reach it, and
// blocks until interrupted.
//
// Shared by `shao serve` and by `shao start --http` in a shell that is already
// recorded, so that both print the same thing and neither can drift into
// starting the endpoint differently.
func serveUntilInterrupt(cfg *config.Config, port int, bind, allow string) {
	srv, err := mcpsrv.New(cfg)
	check(err)
	token, err := mcpsrv.LoadOrCreateToken(cfg)
	check(err)

	httpCfg := cfg.MCP.HTTP
	httpCfg.Port = port
	if bind != "" {
		httpCfg.Bind = bind
		// Typing --bind is the consent; a config file value alone is not
		// enough to publish a terminal to the network.
		httpCfg.AllowRemote = true
	}
	if allow != "" {
		httpCfg.Allow = strings.Split(allow, ",")
	}
	h, err := srv.ListenHTTP(httpCfg, token)
	check(err)

	// Record where this endpoint is so it can be stopped precisely later.
	if err := mcpsrv.WriteServeState(cfg, h, allow); err != nil {
		fmt.Fprintf(os.Stderr, "shao: warning: could not record the endpoint state: %v\n", err)
	}
	defer mcpsrv.ClearServeState(cfg)

	// The URL is what this command exists to produce, so it comes first.
	fmt.Println("shao is serving MCP over HTTP. Point your AI client at one of these.")
	fmt.Println()
	printHTTPConfigs(h.URL(), token)
	printReach(h, token, allow)
	fmt.Println()
	fmt.Println("This command only reads, and only when a tool is called. Recording is a")
	fmt.Println("separate thing: `shao start` in a terminal, which also covers new ones.")
	fmt.Println()
	fmt.Println("Press Ctrl+C to stop serving, or `shao serve --stop` from anywhere.")
	fmt.Println("Never stop shao by image name: every recorded terminal runs a shao")
	fmt.Println("process too, and killing those ends those shells.")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Also watch for `shao serve --stop`, so stopping the endpoint never
	// requires finding and killing a process.
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if mcpsrv.ServeStopRequested(cfg) {
					stop()
					return
				}
			}
		}
	}()

	<-ctx.Done()

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.Close(shutdown)
	fmt.Println("\nshao: stopped serving.")
}

// printReach explains what can actually reach this endpoint, and what that
// costs when it is more than this machine.
func printReach(h *mcpsrv.HTTPServer, token, allow string) {
	lan := h.LANURLs()
	if len(lan) == 0 {
		fmt.Println()
		fmt.Println("Reachable from this machine only. It requires the bearer token above and")
		fmt.Println("rejects browser origins.")
		return
	}

	fmt.Println()
	fmt.Println("Reachable from your network. For an agent on another machine, use one of:")
	fmt.Println()
	for _, base := range lan {
		fmt.Printf("  %s/mcp   (Streamable HTTP)\n", base)
		fmt.Printf("  %s/sse?token=%s   (SSE, token in the URL)\n", base, token)
	}

	fmt.Println()
	fmt.Println("Be clear about what this exposes. Recorded terminal output is served over")
	fmt.Println("plain HTTP, so anything able to observe the network sees both that output")
	fmt.Println("and the token. Redaction catches credential shapes it recognises, not all of")
	fmt.Println("them. Treat the reach of this port as the reach of your scrollback.")
	if allow == "" {
		fmt.Println()
		fmt.Println("Any host that can reach this port may connect if it has the token. To narrow")
		fmt.Println("that, restart with e.g. --allow 192.168.1.0/24, or name the one machine:")
		fmt.Println("  --allow 192.168.1.42")
	} else {
		fmt.Printf("\nOnly these clients may connect: %s\n", allow)
	}
	fmt.Println()
	fmt.Println("Where the agent's machine can open an SSH tunnel, that is the better option:")
	fmt.Println("it needs no exposed port and is encrypted. Run this on the agent's machine,")
	fmt.Println("then point it at 127.0.0.1:")
	_, port, _ := net.SplitHostPort(h.Addr)
	fmt.Printf("  ssh -N -L %s:127.0.0.1:%s <user>@<this-machine>\n", port, port)
}

func cmdMCPConfig(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("mcp-config", flag.ExitOnError)
	format := fs.String("format", "all", "one of: all, claude, cursor, json, http")
	check(fs.Parse(args))

	exe := executablePath()
	switch *format {
	case "all":
		printConfigs(cfg, "", "")
	case "claude", "cursor", "json":
		fmt.Println(stdioConfigJSON(exe))
	case "http":
		token, err := mcpsrv.LoadOrCreateToken(cfg)
		check(err)
		fmt.Println(httpConfigJSON(fmt.Sprintf("http://%s:%d/mcp", cfg.MCP.HTTP.Bind, cfg.MCP.HTTP.Port), token))
		fmt.Fprintln(os.Stderr, "shao: note: the port is only known while an HTTP endpoint is running; use `shao serve` or `shao start --http` to get the live URL.")
	default:
		fail("unknown format %q", *format)
	}
}

// printConfigs prints the ways of connecting, stdio first. url and token may
// be empty when no HTTP endpoint is running.
func printConfigs(cfg *config.Config, url, token string) {
	printStdioConfig()
	if url == "" {
		return
	}
	fmt.Println()
	printHTTPConfigs(url, token)
}

func printStdioConfig() {
	fmt.Println("Option A: the client launches shao itself (simplest, no port, no token).")
	fmt.Println("Paste into your MCP client configuration, for example Claude Desktop's")
	fmt.Println("claude_desktop_config.json, a project .mcp.json, or Cursor's MCP settings:")
	fmt.Println()
	fmt.Println(indent(stdioConfigJSON(executablePath()), "  "))
}

// printHTTPConfigs prints the two URL-based transports.
//
// Streamable HTTP comes first because it is the current one; SSE follows
// because it is what a lot of hosted platforms still ask for, and some of them
// have nowhere to put a header, hence the ready-made URL with the token in it.
func printHTTPConfigs(url, token string) {
	fmt.Println("Streamable HTTP (current MCP transport):")
	fmt.Println()
	fmt.Printf("  %s\n", url)
	fmt.Println()
	fmt.Println(indent(httpConfigJSON(url, token), "  "))

	sse := strings.Replace(url, "/mcp", "/sse", 1)
	fmt.Println()
	fmt.Println("SSE (older transport, still the only one some platforms offer). If the")
	fmt.Println("platform gives you nowhere to put a token, use the URL with it embedded:")
	fmt.Println()
	fmt.Printf("  %s?token=%s\n", sse, token)
	fmt.Println()
	fmt.Println(indent(sseConfigJSON(sse, token), "  "))
}

func sseConfigJSON(sseURL, token string) string {
	return mustJSON(map[string]any{
		"mcpServers": map[string]any{
			"shao": map[string]any{
				"type": "sse",
				"url":  sseURL,
				"headers": map[string]any{
					"Authorization": "Bearer " + token,
				},
			},
		},
	})
}

func stdioConfigJSON(exe string) string {
	return mustJSON(map[string]any{
		"mcpServers": map[string]any{
			"shao": map[string]any{
				"command": exe,
				"args":    []string{"mcp"},
			},
		},
	})
}

func httpConfigJSON(url, token string) string {
	return mustJSON(map[string]any{
		"mcpServers": map[string]any{
			"shao": map[string]any{
				"type": "http",
				"url":  url,
				"headers": map[string]any{
					"Authorization": "Bearer " + token,
				},
			},
		},
	})
}

func mustJSON(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "shao"
	}
	return exe
}

func cmdToken(cfg *config.Config, args []string) {
	if len(args) == 0 {
		token, err := mcpsrv.LoadOrCreateToken(cfg)
		check(err)
		fmt.Println(token)
		return
	}
	switch args[0] {
	case "rotate":
		token, err := mcpsrv.RotateToken(cfg)
		check(err)
		fmt.Println(token)
		fmt.Fprintln(os.Stderr, "shao: the previous token no longer works; update any client configured with it.")
	case "show":
		token, err := mcpsrv.LoadOrCreateToken(cfg)
		check(err)
		fmt.Println(token)
	default:
		fail("unknown token subcommand %q (want show or rotate)", args[0])
	}
}

func cmdConfig(cfg *config.Config, args []string) {
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "path":
		fmt.Println(cfg.ConfigPath())
	case "show":
		data, err := os.ReadFile(cfg.ConfigPath())
		if err != nil {
			fmt.Printf("No config file at %s; shao is using its defaults.\n", cfg.ConfigPath())
			fmt.Println("Run `shao config init` to write one you can edit.")
			return
		}
		fmt.Print(string(data))
	case "init":
		check(cfg.Save())
		fmt.Println("wrote", cfg.ConfigPath())
	default:
		fail("unknown config subcommand %q (want path, show or init)", sub)
	}
}
