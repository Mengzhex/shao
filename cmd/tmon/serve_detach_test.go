package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Backgrounding is the default for `tmon serve`, which means the child it
// spawns would background itself too unless explicitly told not to. The first
// version of this did exactly that and produced 51 processes in a few seconds.
//
// The test builds the real binary and runs it, because the bug lives in the
// arguments handed to a child process, which no unit test of a pure function
// would catch.
func TestServeDetachDoesNotForkBomb(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}

	tmp := t.TempDir()
	exe := uniqueBinary(t, tmp)
	build := exec.Command("go", "build", "-o", exe, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build the binary here: %v\n%s", err, out)
	}

	home := filepath.Join(tmp, "home")
	// Port 0 keeps the test off any port a real endpoint might be using.
	cmd := exec.Command(exe, "--home", home, "serve", "--port", "0")
	cmd.Env = append(os.Environ(), "TMON_HOME="+home)
	out, err := cmd.CombinedOutput()
	t.Cleanup(func() {
		stop := exec.Command(exe, "--home", home, "serve", "--stop")
		stop.Env = cmd.Env
		_ = stop.Run()
	})
	if err != nil {
		t.Fatalf("serve failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Serving in the background") {
		t.Fatalf("did not background itself:\n%s", out)
	}

	// One endpoint, not a family of them. Counting the recorded pid is not
	// enough, because a fork bomb leaves one winner and many losers, so count
	// the processes actually running from this binary.
	time.Sleep(1500 * time.Millisecond)
	n := countProcesses(t, exe)
	if n > 1 {
		// Best effort cleanup before failing, so a regression does not leave
		// a pile of processes behind.
		killAll(t, exe)
		t.Fatalf("%d processes are running from the test binary; backgrounding recursed", n)
	}
	if n == 0 {
		t.Errorf("no endpoint process is running; it exited instead of serving:\n%s", out)
	}
}

// The child is marked, so if it ever reaches the backgrounding path it fails
// loudly instead of spawning.
func TestServeChildRefusesToBackgroundAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}

	tmp := t.TempDir()
	exe := uniqueBinary(t, tmp)
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Skipf("cannot build the binary here: %v\n%s", err, out)
	}

	home := filepath.Join(tmp, "home")
	cmd := exec.Command(exe, "--home", home, "serve", "--port", "0")
	cmd.Env = append(os.Environ(), "TMON_HOME="+home, serveChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		killAll(t, exe)
		t.Fatalf("a marked child was allowed to background itself:\n%s", out)
	}
	if !strings.Contains(string(out), "tried to background itself") {
		t.Errorf("unhelpful failure for a recursive background attempt:\n%s", out)
	}
}

// uniqueBinary gives each run its own executable name.
//
// Process counting is by image name, so a shared name would let a previous
// run's leftovers be counted against this one. That produced a confusing
// false failure once already: a mutation test deliberately caused a fork
// bomb, and the next honest run inherited its survivors and failed too.
// The name is kept short on purpose: tasklist truncates its IMAGENAME column
// at around 25 characters, so a descriptive name would never match and every
// count would come back zero.
func uniqueBinary(t *testing.T, dir string) string {
	t.Helper()
	name := fmt.Sprintf("tmt%06d", time.Now().UnixNano()%1e6)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	t.Cleanup(func() { killAll(t, path) })
	return path
}

func countProcesses(t *testing.T, exe string) int {
	t.Helper()
	name := filepath.Base(exe)
	var out []byte
	var err error
	if runtime.GOOS == "windows" {
		out, err = exec.Command("tasklist", "/FI", "IMAGENAME eq "+name, "/NH").Output()
	} else {
		out, err = exec.Command("pgrep", "-f", exe).Output()
		if err != nil {
			// pgrep exits non-zero when nothing matches.
			return 0
		}
	}
	if err != nil {
		t.Logf("could not count processes: %v", err)
		return 0
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if runtime.GOOS == "windows" {
			if strings.Contains(line, name) {
				n++
			}
			continue
		}
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func killAll(t *testing.T, exe string) {
	t.Helper()
	name := filepath.Base(exe)
	// Safe here only because the name is unique to this test's temp build.
	// Never do this to the real binary: every recorded terminal runs one.
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/F", "/IM", name).Run()
		return
	}
	_ = exec.Command("pkill", "-f", exe).Run()
}

// `tmon start --port N` is the two-command shape the tool is meant to have:
// one command turns everything on, one turns it off. The endpoint must be
// detached, so it outlives the recorded shell rather than dying with it, and
// `tmon end` must take it down again.
func TestStartWithPortBringsUpDetachedEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}

	tmp := t.TempDir()
	exe := uniqueBinary(t, tmp)
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Skipf("cannot build the binary here: %v\n%s", err, out)
	}
	home := filepath.Join(tmp, "home")
	env := append(os.Environ(), "TMON_HOME="+home)

	// --no-hook keeps the machine's startup files out of it; stdin is closed
	// at once so the recorded shell exits immediately.
	start := exec.Command(exe, "--home", home, "start", "--no-hook", "--port", "0")
	start.Env = env
	start.Stdin = strings.NewReader("exit\r")
	out, err := start.CombinedOutput()
	t.Cleanup(func() {
		stop := exec.Command(exe, "--home", home, "serve", "--stop")
		stop.Env = env
		_ = stop.Run()
	})
	if err != nil && !strings.Contains(string(out), "Serving in the background") {
		t.Fatalf("start failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Serving in the background") {
		t.Fatalf("--port did not bring up an endpoint:\n%s", out)
	}

	// The recorded shell has exited by now. The endpoint must not have.
	status := exec.Command(exe, "--home", home, "serve", "--status")
	status.Env = env
	sOut, _ := status.CombinedOutput()
	if !strings.Contains(string(sOut), "Serving on") {
		t.Fatalf("the endpoint died with the recorded shell:\n%s", sOut)
	}

	// And `tmon end` takes it down, so one command really does turn it all off.
	end := exec.Command(exe, "--home", home, "end", "--keep-hook")
	end.Env = env
	eOut, _ := end.CombinedOutput()
	if !strings.Contains(string(eOut), "Stopped the HTTP endpoint") {
		t.Errorf("`tmon end` did not stop the endpoint:\n%s", eOut)
	}

	status2 := exec.Command(exe, "--home", home, "serve", "--status")
	status2.Env = env
	s2Out, _ := status2.CombinedOutput()
	if strings.Contains(string(s2Out), "Serving on") {
		t.Errorf("the endpoint is still running after `tmon end`:\n%s", s2Out)
	}
}

// --keep-shells means "do not stop terminals that are recording". It does not
// mean "leave a false record of a terminal that is already gone", so stale
// sessions are tidied either way.
func TestEndKeepShellsStillTidiesStaleSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}

	tmp := t.TempDir()
	exe := uniqueBinary(t, tmp)
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Skipf("cannot build the binary here: %v\n%s", err, out)
	}
	home := filepath.Join(tmp, "home")
	env := append(os.Environ(), "TMON_HOME="+home)

	// A session left behind by a recorder that vanished: metadata says it is
	// open, no such process exists.
	dir := filepath.Join(home, "sessions", "vanished-one")
	if err := os.MkdirAll(filepath.Join(dir, "raw"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"vanished-one","pid":2147483632,"started_at":"2026-08-27T10:00:00Z",` +
		`"shell":"pwsh","max_bytes":1048576,"cooked_max_bytes":1048576,"segment_bytes":4096}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	end := exec.Command(exe, "--home", home, "end", "--keep-shells", "--keep-hook")
	end.Env = env
	out, err := end.CombinedOutput()
	if err != nil {
		t.Fatalf("end failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Closed the record of 1 session") {
		t.Errorf("--keep-shells skipped tidying a stale session:\n%s", out)
	}

	sessions := exec.Command(exe, "--home", home, "sessions")
	sessions.Env = env
	sOut, _ := sessions.CombinedOutput()
	if strings.Contains(string(sOut), "stale") {
		t.Errorf("the session is still reported stale:\n%s", sOut)
	}
}

// An endpoint request must survive the "already recording" early return.
//
// This has regressed twice: the early return was written before --http
// existed, was fixed for it, and then broke again when --http became --port.
// A test is the only thing that stops a third time.
func TestStartInRecordedShellStillHonoursPort(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}

	tmp := t.TempDir()
	exe := uniqueBinary(t, tmp)
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Skipf("cannot build the binary here: %v\n%s", err, out)
	}
	home := filepath.Join(tmp, "home")

	// TMON_RECORDING=1 is what a recorded shell looks like from inside.
	cmd := exec.Command(exe, "--home", home, "start", "--no-hook", "--port", "0")
	cmd.Env = append(os.Environ(),
		"TMON_HOME="+home,
		"TMON_RECORDING=1",
		"TMON_SESSION_ID=pretend-session",
	)
	out, err := cmd.CombinedOutput()
	t.Cleanup(func() {
		stop := exec.Command(exe, "--home", home, "serve", "--stop")
		stop.Env = cmd.Env
		_ = stop.Run()
	})
	if err != nil {
		t.Fatalf("start failed: %v\n%s", err, out)
	}

	if !strings.Contains(string(out), "Serving in the background") {
		t.Errorf("--port was swallowed by the already-recording path:\n%s", out)
	}
	if !strings.Contains(string(out), "Monitoring is on") {
		t.Errorf("the status report went missing:\n%s", out)
	}

	// The endpoint's own configuration is welcome here: it is what --port was
	// asked for. What must not reappear is the stdio block, which is
	// onboarding for a different transport and is noise on every status check.
	// Only the stdio form carries an "args" list.
	if strings.Contains(string(out), `"args"`) {
		t.Errorf("the stdio client configuration was reprinted in status output:\n%s", out)
	}
	if !strings.Contains(string(out), `/mcp`) {
		t.Errorf("the endpoint URL was not shown even though --port was given:\n%s", out)
	}

	status := exec.Command(exe, "--home", home, "serve", "--status")
	status.Env = cmd.Env
	sOut, _ := status.CombinedOutput()
	if !strings.Contains(string(sOut), "Serving on") {
		t.Errorf("no endpoint is actually running:\n%s", sOut)
	}
}
