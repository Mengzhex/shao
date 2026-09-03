//go:build !windows

package main

import "syscall"

// detachedAttr puts the child in its own session, so it has no controlling
// terminal and does not receive the signals sent to the foreground job of the
// terminal it was started from.
func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
