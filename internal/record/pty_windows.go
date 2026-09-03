//go:build windows

package record

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no pty. Its equivalent is the pseudoconsole (ConPTY),
// introduced in Windows 10 1809, which gives the same shape: a pair of pipes
// carrying VT-encoded traffic with a console host translating for programs
// that still use the old console API. That means a recorded PowerShell
// session produces the same kind of byte stream a recorded bash session does,
// and the rest of tmon needs no Windows-specific handling.
//
// NOTE: this file talks to the Win32 API directly rather than through a
// wrapper library, to keep the dependency surface small. It is the part of
// tmon most in need of an actual compile-and-run check on Windows.

// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, from processthreadsapi.h. It is not
// exported by x/sys/windows, so it is defined here.
const procThreadAttributePseudoConsole uintptr = 0x00020016

var (
	modkernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	procUpdateProcThreadAttribute = modkernel32.NewProc("UpdateProcThreadAttribute")
)

// attachPseudoConsole sets PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE on an
// attribute list so the child process inherits the pseudoconsole.
//
// The attribute's value is the pseudoconsole handle *itself*, passed by value
// rather than as a pointer to it. That makes the obvious call,
// ProcThreadAttributeListContainer.Update, awkward: it takes an
// unsafe.Pointer and retains it in a Go slice of pointers to keep it alive,
// so a handle passed that way ends up sitting in a pointer slice as an
// integer the collector would scan as if it were a heap address. `go vet`
// objects to the conversion for exactly that reason, and it is right to.
//
// Calling UpdateProcThreadAttribute directly keeps the handle as a plain
// uintptr from start to finish, which is what the Win32 API wants anyway.
func attachPseudoConsole(al *windows.ProcThreadAttributeListContainer, hpc windows.Handle) error {
	r1, _, lastErr := procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(al.List())),
		0, // dwFlags, reserved
		procThreadAttributePseudoConsole,
		uintptr(hpc),
		unsafe.Sizeof(hpc),
		0, // lpPreviousValue, unused
		0, // lpReturnSize, unused
	)
	// The attribute list must outlive the call; it is owned by the caller,
	// but say so explicitly since only its address was passed.
	runtime.KeepAlive(al)
	if r1 == 0 {
		return lastErr
	}
	return nil
}

type winPTY struct {
	hpc     windows.Handle
	in      *os.File // keystrokes go in here
	out     *os.File // shell output comes out here
	proc    windows.Handle
	thread  windows.Handle
	attrs   *windows.ProcThreadAttributeListContainer
	closing sync.Once
}

func startPlatformPTY(exe string, args, env []string, cols, rows int) (_ PTY, err error) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	// Two pipes: one the pseudoconsole reads keystrokes from, one it writes
	// output to. We keep the opposite end of each.
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("create input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return nil, fmt.Errorf("create output pipe: %w", err)
	}

	var hpc windows.Handle
	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	if err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &hpc); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		windows.CloseHandle(outWrite)
		return nil, fmt.Errorf("create pseudoconsole: %w", err)
	}
	// The pseudoconsole has duplicated the ends it needs; holding on to our
	// copies would keep the pipes alive after the shell exits and the reader
	// would never see EOF.
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)

	p := &winPTY{
		hpc: hpc,
		in:  os.NewFile(uintptr(inWrite), "conpty-in"),
		out: os.NewFile(uintptr(outRead), "conpty-out"),
	}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, fmt.Errorf("alloc attribute list: %w", err)
	}
	p.attrs = attrs
	if err := attachPseudoConsole(attrs, hpc); err != nil {
		return nil, fmt.Errorf("attach pseudoconsole to process attributes: %w", err)
	}

	var si windows.StartupInfoEx
	si.StartupInfo.Cb = uint32(unsafe.Sizeof(si))
	si.ProcThreadAttributeList = attrs.List()

	// Cut the child off from this process's standard handles.
	//
	// This is the difference between tmon recording anything and recording
	// nothing, and it is not obvious. The pseudoconsole attribute above does
	// attach the child to the pseudoconsole -- a child asking its console for
	// dimensions correctly gets the size passed to CreatePseudoConsole. But
	// its *standard output handle* is a separate matter: without this, the
	// child inherits tmon's own stdout, so anything it prints goes straight
	// there and never passes through the pty. The output still appears on
	// screen, which makes the failure look like a capture bug rather than a
	// handle-inheritance one.
	//
	// Declaring the standard handles as null says the child has none to
	// inherit, so console initialisation points them at its console, which is
	// the pseudoconsole. Microsoft's own sample omits this because a GUI host
	// has no stdio worth inheriting; a command-line tool like tmon does.
	si.StartupInfo.Flags |= windows.STARTF_USESTDHANDLES
	si.StartupInfo.StdInput = 0
	si.StartupInfo.StdOutput = 0
	si.StartupInfo.StdErr = 0

	cmdline, err := windows.UTF16PtrFromString(makeCmdLine(append([]string{exe}, args...)))
	if err != nil {
		return nil, fmt.Errorf("build command line: %w", err)
	}
	envBlock := makeEnvBlock(mergedEnv(env))

	var pi windows.ProcessInformation
	err = windows.CreateProcess(
		nil,
		cmdline,
		nil,
		nil,
		false, // the pseudoconsole passes what the child needs
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		&envBlock[0],
		nil,
		&si.StartupInfo,
		&pi,
	)
	// envBlock must stay alive until CreateProcess has copied it.
	runtime.KeepAlive(envBlock)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", exe, err)
	}
	p.proc, p.thread = pi.Process, pi.Thread
	return p, nil
}

func (p *winPTY) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *winPTY) Write(b []byte) (int, error) { return p.in.Write(b) }

func (p *winPTY) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return windows.ResizePseudoConsole(p.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// Wait blocks until the shell exits, then releases the process handles.
//
// It owns those handles rather than Close, because Close runs concurrently
// with this call and closing the handle out from under WaitForSingleObject
// makes it fail with "The handle is invalid".
func (p *winPTY) Wait() (int, error) {
	if p.proc == 0 {
		return -1, fmt.Errorf("pty: process not started")
	}
	defer func() {
		if p.thread != 0 {
			windows.CloseHandle(p.thread)
			p.thread = 0
		}
		if p.proc != 0 {
			windows.CloseHandle(p.proc)
			p.proc = 0
		}
	}()

	if _, err := windows.WaitForSingleObject(p.proc, windows.INFINITE); err != nil {
		return -1, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.proc, &code); err != nil {
		return -1, err
	}
	return int(int32(code)), nil
}

// Close tears the session down. Closing the pseudoconsole is what makes the
// output pipe reach EOF, so a reader blocked in Read is released.
func (p *winPTY) Close() error {
	p.closing.Do(func() {
		if p.hpc != 0 {
			windows.ClosePseudoConsole(p.hpc)
			p.hpc = 0
		}
		if p.in != nil {
			p.in.Close()
		}
		if p.out != nil {
			p.out.Close()
		}
		if p.attrs != nil {
			p.attrs.Delete()
			p.attrs = nil
		}
		// The process and thread handles are deliberately left open.
		//
		// Close is called concurrently with Wait -- by the signal handler, and
		// by the watcher that implements `tmon end` -- and Wait is blocked in
		// WaitForSingleObject on exactly this process handle. Closing it there
		// makes that call fail with "The handle is invalid", which then
		// surfaced as a spurious error at the end of an otherwise clean
		// session. Wait releases them once it has the exit code, and a process
		// that never calls Wait is about to exit anyway.
	})
	return nil
}

// makeCmdLine joins arguments using the quoting rules CommandLineToArgvW
// applies in reverse, which is what every Windows program uses to split the
// single command-line string back into arguments.
func makeCmdLine(args []string) string {
	var b strings.Builder
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quoteArg(a))
	}
	return b.String()
}

func quoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		// A run of backslashes is only special when it precedes a quote or
		// the end of the argument; otherwise it stands for itself.
		slashes := 0
		for i < len(s) && s[i] == '\\' {
			slashes++
			i++
		}
		if i == len(s) {
			// Trailing backslashes are doubled so they do not escape the
			// closing quote.
			b.WriteString(strings.Repeat(`\`, slashes*2))
			break
		}
		if s[i] == '"' {
			b.WriteString(strings.Repeat(`\`, slashes*2+1))
		} else {
			b.WriteString(strings.Repeat(`\`, slashes))
		}
		b.WriteByte(s[i])
		i++
	}
	b.WriteByte('"')
	return b.String()
}

// makeEnvBlock builds the double-null-terminated UTF-16 environment block
// CreateProcess expects.
func makeEnvBlock(env []string) []uint16 {
	var block []uint16
	for _, e := range env {
		// An entry containing a NUL cannot be represented and is dropped
		// rather than truncating the block.
		u, err := windows.UTF16FromString(e)
		if err != nil {
			continue
		}
		block = append(block, u...)
	}
	return append(block, 0)
}
