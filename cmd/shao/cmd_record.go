package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Mengzhex/shao/internal/config"
	"github.com/Mengzhex/shao/internal/record"
	"github.com/Mengzhex/shao/internal/store"
)

func cmdShell(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("shell", flag.ExitOnError)
	label := fs.String("label", "", "name this session so it can be asked about by name later")
	shell := fs.String("shell", "", "record a specific shell instead of your default")
	quiet := fs.Bool("quiet", false, "suppress the banner and closing summary")
	check(fs.Parse(args))

	if os.Getenv("SHAO_RECORDING") == "1" {
		fail("this shell is already being recorded as session %s", os.Getenv("SHAO_SESSION_ID"))
	}

	code, err := record.Run(context.Background(), record.Options{
		Cfg:   cfg,
		Label: *label,
		Shell: *shell,
		Quiet: *quiet,
	})
	if err != nil {
		fail("%v", err)
	}
	os.Exit(code)
}

func cmdRun(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	label := fs.String("label", "", "name this session")
	quiet := fs.Bool("quiet", false, "suppress the banner and closing summary")
	check(fs.Parse(args))

	cmdArgs := fs.Args()
	if len(cmdArgs) == 0 {
		fail("nothing to run: use `shao run -- ./deploy.sh`")
	}

	code, err := record.Run(context.Background(), record.Options{
		Cfg:   cfg,
		Label: *label,
		Args:  cmdArgs,
		Quiet: *quiet,
	})
	if err != nil {
		fail("%v", err)
	}
	os.Exit(code)
}

func cmdSessions(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("sessions", flag.ExitOnError)
	all := fs.Bool("all", false, "include every session regardless of age")
	live := fs.Bool("live", false, "only sessions still recording")
	hours := fs.Int("hours", 0, "include sessions that finished within this many hours (default 2)")
	parseFlags(fs, args)

	everything, err := store.List(cfg.SessionsDir())
	check(err)
	if len(everything) == 0 {
		fmt.Println("No recorded sessions yet. Start one with `shao start`.")
		fmt.Println("Terminals opened without shao are not recorded; `shao start` also makes new ones record themselves.")
		return
	}

	// The same filter the MCP tool uses, so the CLI and the AI never describe
	// the machine differently.
	filter := store.DefaultFilter()
	filter.All = *all
	if *live {
		filter.IncludeEnded = false
	}
	if *hours != 0 {
		filter.EndedWithin = time.Duration(*hours) * time.Hour
	}
	sessions, omitted := filter.Apply(everything)

	for _, s := range sessions {
		fmt.Println(s.Describe())
	}
	if len(sessions) == 0 {
		fmt.Println("No terminal is recording, and none finished recently.")
	}
	if omitted > 0 {
		fmt.Printf("\n%d older finished session(s) not shown. Use --all, or --hours N.\n", omitted)
	}
}

func cmdTail(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("tail", flag.ExitOnError)
	lines := fs.Int("lines", 200, "how many lines to print")
	raw := fs.Bool("raw", false, "print the exact captured bytes instead of the readable stream")
	positional := parseFlags(fs, args)

	selector := ""
	if len(positional) > 0 {
		selector = positional[0]
	}
	info, err := store.Resolve(cfg.SessionsDir(), selector)
	check(err)

	stream := store.StreamCooked
	if *raw {
		stream = store.StreamRaw
	}
	res, err := store.ReadTailLines(store.StreamDir(info.Dir, stream), *lines, 8<<20)
	check(err)

	if res.Span.Truncated {
		fmt.Fprintln(os.Stderr, "shao: note: the ring buffer has discarded older output")
	}
	if res.Span.GapDetected {
		fmt.Fprintln(os.Stderr, "shao: WARNING: a discontinuity was detected in this buffer")
	}
	fmt.Println(strings.Join(res.Lines, "\n"))
}

func cmdLastError(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("last-error", flag.ExitOnError)
	maxLines := fs.Int("lines", 200, "cap on output lines to print")
	positional := parseFlags(fs, args)

	selector := ""
	if len(positional) > 0 {
		selector = positional[0]
	}
	info, err := store.Resolve(cfg.SessionsDir(), selector)
	check(err)

	entries, err := store.ReadIndex(info.Dir)
	check(err)

	var last *store.IndexEntry
	for i := range entries {
		if entries[i].Failed() {
			last = &entries[i]
		}
	}
	if last == nil {
		if info.Meta.ShellIntegration == "none" || info.Meta.ShellIntegration == "" {
			fail("session %s has no command index (its shell could not be instrumented); try `shao tail %s`",
				info.Meta.ID, info.Meta.ID)
		}
		fmt.Printf("No failed commands in session %s.\n", info.Meta.ID)
		return
	}

	fmt.Printf("command : %s\n", last.Cmd)
	fmt.Printf("exit    : %d\n", *last.ExitCode)
	fmt.Printf("started : %s\n", last.StartedAt.Format("2006-01-02 15:04:05"))
	if last.EndedAt != nil {
		fmt.Printf("duration: %s\n", last.EndedAt.Sub(last.StartedAt).Round(time.Millisecond))
	}
	if last.RemoteHost != "" {
		fmt.Printf("ran on  : %s\n", last.RemoteHost)
	}

	res, err := store.ReadRange(store.StreamDir(info.Dir, store.StreamCooked), last.CookedOff, int64(last.CookedLen))
	check(err)
	if res.Span.Truncated && res.StartOffset > last.CookedOff {
		fmt.Fprintln(os.Stderr, "shao: note: part of this command's output has already been discarded by the ring buffer")
	}

	out := strings.TrimRight(string(res.Data), "\n")
	if out == "" {
		fmt.Println("\n(that command produced no output of its own)")
		return
	}
	lines := strings.Split(out, "\n")
	if len(lines) > *maxLines {
		fmt.Printf("\noutput (last %d of %d lines):\n\n", *maxLines, len(lines))
		lines = lines[len(lines)-*maxLines:]
	} else {
		fmt.Printf("\noutput:\n\n")
	}
	fmt.Println(strings.Join(lines, "\n"))
}
