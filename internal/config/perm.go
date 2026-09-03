package config

import (
	"os"
	"path/filepath"
)

// Terminal buffers hold whatever scrolled past your eyes, and redaction is a
// best effort rather than a guarantee. So every file tmon creates is
// owner-only. On POSIX that is 0600/0700 directly. On Windows the mode bits
// are largely ignored by the filesystem, and what actually keeps the tree
// private is that it lives under the user profile directory and inherits its
// ACL; chmod there only toggles the read-only attribute, so failures are not
// actionable and are swallowed.

const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// EnsurePrivateDir creates dir and any parents with owner-only permissions.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	// MkdirAll applies the mode only to directories it creates, and umask
	// can still loosen it, so tighten explicitly.
	chmodBestEffort(dir, dirMode)
	return nil
}

// WritePrivateFile writes data to path with owner-only permissions via a
// temporary file and a rename, so a reader never observes a half-written
// config.
func WritePrivateFile(path string, data []byte) error {
	if err := EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// os.Rename replaces an existing destination on both POSIX and Windows.
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	chmodBestEffort(path, fileMode)
	return nil
}

// CreatePrivateFile opens a file for writing with owner-only permissions,
// creating the parent directory if needed.
func CreatePrivateFile(path string, flag int) (*os.File, error) {
	if err := EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, flag, fileMode)
	if err != nil {
		return nil, err
	}
	chmodBestEffort(path, fileMode)
	return f, nil
}

func chmodBestEffort(path string, mode os.FileMode) {
	_ = os.Chmod(path, mode)
}
