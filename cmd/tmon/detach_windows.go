//go:build windows

package main

import "syscall"

// DETACHED_PROCESS, from the Win32 process creation flags. It is not exported
// by the syscall package.
const detachedProcess = 0x00000008

// detachedAttr makes a child outlive this process and own no console.
//
// Detaching the console matters as much as detaching the lifetime: a
// background server that kept this console would write into the terminal the
// user is typing in, and would die with it.
func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
