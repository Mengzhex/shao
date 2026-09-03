//go:build windows

package store

import "golang.org/x/sys/windows"

// stillActive is STILL_ACTIVE from the Win32 headers: the exit code reported
// for a process that has not exited.
const stillActive = 259

// ProcessAlive reports whether a recorder process is still running.
//
// A handle can often be opened for a process that has already exited but not
// yet been reaped, so the exit code has to be checked rather than trusting
// that OpenProcess succeeded.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
