//go:build !windows

package record

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// POSIX terminals already speak VT, so nothing needs enabling.
func enablePlatformVT(_, _ uintptr) func() { return func() {} }

// watchResize listens for SIGWINCH, which the kernel sends whenever the
// terminal window changes size.
func watchResize(ctx context.Context, c *console, fn func(cols, rows int)) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				cols, rows := c.Size()
				fn(cols, rows)
			}
		}
	}()
}
