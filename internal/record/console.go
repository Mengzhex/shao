package record

import (
	"context"
	"os"

	"golang.org/x/term"
)

// console owns the real terminal tmon was launched in: the one the user
// types into and watches.
//
// While a session is recorded, that terminal is put in raw mode and becomes a
// straight conduit to the pty. Nothing is interpreted on the way through, so
// colours, full-screen programs and interactive prompts behave exactly as
// they would without tmon in the middle. Recording taps the same bytes on
// their way past; it never sits in front of them.
type console struct {
	in                         *os.File
	out                        *os.File
	state                      *term.State
	restore                    func()
	rawMode                    bool
	isTTY                      bool
	fallbackCols, fallbackRows int
}

func newConsole() *console {
	c := &console{
		in:           os.Stdin,
		out:          os.Stdout,
		fallbackCols: 120,
		fallbackRows: 30,
	}
	c.isTTY = term.IsTerminal(int(c.in.Fd())) && term.IsTerminal(int(c.out.Fd()))
	return c
}

// Enter puts the terminal into raw mode and turns on VT processing where the
// platform needs it asked for.
//
// When tmon is not attached to a real terminal (output piped to a file, run
// from a script) this is a no-op and recording still works; only the
// interactive experience is absent.
func (c *console) Enter() error {
	c.restore = enablePlatformVT(c.in.Fd(), c.out.Fd())
	if !c.isTTY {
		return nil
	}
	state, err := term.MakeRaw(int(c.in.Fd()))
	if err != nil {
		return err
	}
	c.state = state
	c.rawMode = true
	return nil
}

// Leave restores the terminal. It is safe to call more than once, which
// matters because it runs both on the normal path and from a signal handler.
func (c *console) Leave() {
	if c.rawMode && c.state != nil {
		_ = term.Restore(int(c.in.Fd()), c.state)
		c.rawMode = false
	}
	if c.restore != nil {
		c.restore()
		c.restore = nil
	}
}

// Size reports the terminal size, falling back to a sane default when there
// is no terminal to ask.
func (c *console) Size() (cols, rows int) {
	if c.isTTY {
		if w, h, err := term.GetSize(int(c.out.Fd())); err == nil && w > 0 && h > 0 {
			return w, h
		}
	}
	return c.fallbackCols, c.fallbackRows
}

// WatchResize calls fn whenever the terminal size changes, until ctx ends.
func (c *console) WatchResize(ctx context.Context, fn func(cols, rows int)) {
	if !c.isTTY {
		return
	}
	watchResize(ctx, c, fn)
}
