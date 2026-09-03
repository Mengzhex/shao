//go:build !windows

package store

import (
	"os"
	"syscall"
)

// ProcessAlive reports whether a recorder process is still running.
//
// On POSIX os.FindProcess always succeeds, so liveness has to be probed with
// the null signal, which performs the permission and existence checks without
// delivering anything.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
