//go:build !windows

package record

import (
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// posixPTY wraps a Unix pty master and the shell running on its slave side.
type posixPTY struct {
	f    *os.File
	cmd  *exec.Cmd
	once sync.Once
}

func startPlatformPTY(exe string, args, env []string, cols, rows int) (PTY, error) {
	cmd := exec.Command(exe, args...)
	cmd.Env = mergedEnv(env)

	f, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		return nil, err
	}
	return &posixPTY{f: f, cmd: cmd}, nil
}

func (p *posixPTY) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *posixPTY) Write(b []byte) (int, error) { return p.f.Write(b) }

func (p *posixPTY) Resize(cols, rows int) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (p *posixPTY) Close() error {
	var err error
	p.once.Do(func() { err = p.f.Close() })
	return err
}

func (p *posixPTY) Wait() (int, error) {
	err := p.cmd.Wait()
	if p.cmd.ProcessState != nil {
		return p.cmd.ProcessState.ExitCode(), nil
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}
