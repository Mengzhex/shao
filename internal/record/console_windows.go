//go:build windows

package record

import (
	"context"
	"time"

	"golang.org/x/sys/windows"
)

// enablePlatformVT turns on VT sequence handling for the real console.
//
// A Windows console does not interpret escape sequences unless asked to.
// Without this the pseudoconsole's output would appear as literal escape
// characters on screen, so the user would see garbage even though the
// recording itself was fine. The previous modes are restored on the way out,
// because leaving another program's console reconfigured would be rude.
func enablePlatformVT(inFd, outFd uintptr) func() {
	inHandle := windows.Handle(inFd)
	outHandle := windows.Handle(outFd)

	var prevIn, prevOut uint32
	haveIn := windows.GetConsoleMode(inHandle, &prevIn) == nil
	haveOut := windows.GetConsoleMode(outHandle, &prevOut) == nil

	if haveOut {
		mode := prevOut | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
		_ = windows.SetConsoleMode(outHandle, mode)
	}
	if haveIn {
		// VT input turns keystrokes into the escape sequences a pty-style
		// shell expects; line input and echo have to go so keys reach the
		// shell one at a time instead of a line at a time.
		mode := prevIn | windows.ENABLE_VIRTUAL_TERMINAL_INPUT
		mode &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT
		_ = windows.SetConsoleMode(inHandle, mode)
	}

	return func() {
		if haveOut {
			_ = windows.SetConsoleMode(outHandle, prevOut)
		}
		if haveIn {
			_ = windows.SetConsoleMode(inHandle, prevIn)
		}
	}
}

// watchResize polls for console size changes.
//
// Windows delivers window-resize events through the console input queue,
// which shao cannot read without competing with the shell for keystrokes.
// Polling avoids that entirely: a quarter-second delay before a full-screen
// program reflows is imperceptible, and nothing about the recording depends
// on it.
func watchResize(ctx context.Context, c *console, fn func(cols, rows int)) {
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		lastCols, lastRows := c.Size()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cols, rows := c.Size()
				if cols != lastCols || rows != lastRows {
					lastCols, lastRows = cols, rows
					fn(cols, rows)
				}
			}
		}
	}()
}
