// Package record runs a shell inside a pseudo-terminal and writes everything
// that passes through it into a session's ring buffers.
//
// Capturing at the pty is the decision the rest of shao rests on. A pty sits
// between the shell and the terminal emulator, so every byte the shell writes
// passes through here exactly once, in order, whether it came from a local
// command, a script, or a shell on the far side of an ssh connection. Nothing
// is sampled and nothing is inferred from the screen, so output that scrolls
// past faster than anyone could read is still recorded in full.
//
// It also explains why shao does not care which terminal emulator is in use.
// Windows Terminal, PuTTY, MobaXterm, iTerm, the VS Code panel: they are all
// on the far side of this pty and none of them is involved in the capture.
//
// The trade this makes is that recording starts when the shell starts. A
// terminal window that is already open cannot be recorded retroactively, so
// coverage comes from starting sessions with `shao shell`, or from
// `shao hook install` which makes new terminals do that automatically.
package record

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// PTY is a running shell attached to a pseudo-terminal.
//
// Read returns what the shell writes to its terminal, Write delivers
// keystrokes to it.
type PTY interface {
	io.ReadWriteCloser
	// Resize tells the shell its terminal changed size, so full-screen
	// programs redraw correctly.
	Resize(cols, rows int) error
	// Wait blocks until the shell exits and returns its exit status.
	Wait() (int, error)
}

// startPTY is implemented per platform: a Unix pty pair on POSIX, a
// pseudoconsole on Windows.
//
// env entries are added to the current environment rather than replacing it,
// so the recorded shell keeps the user's environment.
func startPTY(exe string, args, env []string, cols, rows int) (PTY, error) {
	return startPlatformPTY(exe, args, env, cols, rows)
}

// ResolveShell picks the shell to record.
//
// Preference order is an explicit choice, then the environment's idea of the
// user's shell, then the platform default. The point is that a recorded
// session should feel like the shell the user already uses, not a different
// one that happens to be easier to instrument.
func ResolveShell(override string) string {
	if override != "" {
		return override
	}
	if v := os.Getenv("SHAO_SHELL"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		for _, candidate := range []string{"pwsh.exe", "powershell.exe", "cmd.exe"} {
			if p, err := exec.LookPath(candidate); err == nil {
				return p
			}
		}
		return "cmd.exe"
	}
	if v := os.Getenv("SHELL"); v != "" {
		return v
	}
	for _, candidate := range []string{"/bin/bash", "/usr/bin/bash", "/bin/zsh", "/bin/sh"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "/bin/sh"
}

// mergedEnv returns the process environment with extra KEY=VALUE entries
// applied, replacing any existing entry with the same key.
func mergedEnv(extra []string) []string {
	if len(extra) == 0 {
		return os.Environ()
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	override := make(map[string]bool, len(extra))
	for _, e := range extra {
		if k, _, ok := strings.Cut(e, "="); ok {
			override[strings.ToUpper(k)] = true
		}
	}
	for _, e := range base {
		k, _, _ := strings.Cut(e, "=")
		if override[strings.ToUpper(k)] {
			continue
		}
		out = append(out, e)
	}
	return append(out, extra...)
}
