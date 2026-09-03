package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"tmon/internal/config"
	"tmon/internal/mcpsrv"
	"tmon/internal/store"
)

// cmdEnd is the counterpart to `tmon start`: it turns monitoring off.
//
// There is one thing about it worth being upfront about. Recording and the
// shell are the same object -- tmon records by wrapping the shell in a pty --
// so stopping a recording necessarily ends the shell it was recording. There
// is no arrangement where the shell survives but the recording stops.
//
// So this asks each recorder to finish cleanly rather than killing it. A
// killed recorder would lose whatever was still buffered, which is precisely
// the last output anyone would want to ask about.
//
// Recorded history is never deleted here. Turning monitoring off leaves
// everything already captured readable.
func cmdEnd(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("end", flag.ExitOnError)
	keepHook := fs.Bool("keep-hook", false, "leave the startup hook in place, so new terminals still record")
	keepShells := fs.Bool("keep-shells", false, "leave running recorded shells alone, and only stop future ones")
	parseFlags(fs, args)

	// Stop covering future terminals.
	if !*keepHook {
		targets := hookTargets("")
		installed := false
		for _, t := range targets {
			if data, err := os.ReadFile(t.path); err == nil && containsHook(string(data)) {
				installed = true
			}
		}
		if installed {
			hookUninstall(targets, false)
			fmt.Println("New terminals will no longer record themselves.")
		} else {
			fmt.Println("New terminals were not set to record themselves; nothing to remove.")
		}
	}

	// The endpoint is part of "monitoring is on", so end turns it off too.
	if state, running := mcpsrv.ReadServeState(cfg); running {
		if err := mcpsrv.RequestServeStop(cfg); err == nil {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, still := mcpsrv.ReadServeState(cfg); !still {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			fmt.Printf("Stopped the HTTP endpoint on %s.\n", state.URL)
		}
	}

	sessions, err := store.List(cfg.SessionsDir())
	check(err)

	// Tidy up sessions whose recorder died without marking itself finished.
	//
	// This happens before the --keep-shells check on purpose: a stale session
	// is not a running shell, so "leave running shells alone" is no reason to
	// leave a false record of one in place.
	var stale int
	for _, s := range sessions {
		if !s.Stale {
			continue
		}
		if err := store.Finalize(s.Dir, s.LastActivity); err == nil {
			stale++
		}
	}
	if stale > 0 {
		fmt.Printf("Closed the record of %d session(s) whose recorder had already gone;\n", stale)
		fmt.Println("they were still marked as open. Their captured output is unaffected.")
	}

	if *keepShells {
		fmt.Println("Leaving any running recorded shells alone (--keep-shells).")
		return
	}

	var live []store.Info
	for _, s := range sessions {
		if s.Live() {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		fmt.Println("No sessions are currently recording.")
		return
	}

	fmt.Printf("\nStopping %d recording(s). The shells they wrap will exit; that is the same\n", len(live))
	fmt.Println("thing, because tmon records by wrapping the shell.")
	fmt.Println()

	for _, s := range live {
		if err := store.RequestStop(s.Dir); err != nil {
			fmt.Fprintf(os.Stderr, "tmon: could not signal session %s: %v\n", s.Meta.ID, err)
			continue
		}
	}

	// Recorders poll for the request, so give them a moment and then report
	// what actually happened rather than assuming it worked.
	deadline := time.Now().Add(3 * time.Second)
	stopped := map[string]bool{}
	for time.Now().Before(deadline) && len(stopped) < len(live) {
		time.Sleep(200 * time.Millisecond)
		for _, s := range live {
			if stopped[s.Meta.ID] {
				continue
			}
			if info, err := store.Describe(s.Dir); err == nil && !info.Live() {
				stopped[s.Meta.ID] = true
			}
		}
	}

	for _, s := range live {
		label := s.Meta.Label
		if label == "" {
			label = "-"
		}
		if stopped[s.Meta.ID] {
			fmt.Printf("  stopped  %s  (label %s)\n", s.Meta.ID, label)
			continue
		}
		// The usual cause is a recorder that died earlier without marking the
		// session ended, leaving it looking live forever. Say so instead of
		// reporting a success that did not happen.
		fmt.Printf("  no response  %s  (label %s, recorder pid %d)\n", s.Meta.ID, label, s.Meta.PID)
		fmt.Println("      Either that shell is busy, or its recorder is already gone and the")
		fmt.Println("      session was left marked live. Close the window if it is still open.")
	}

	fmt.Println()
	fmt.Println("Everything already recorded is still readable: `tmon sessions`, `tmon tail`.")
	fmt.Println("Run `tmon start` to turn monitoring back on.")
}

// containsHook reports whether a startup file already has the tmon block.
func containsHook(text string) bool { return strings.Contains(text, hookBegin) }
