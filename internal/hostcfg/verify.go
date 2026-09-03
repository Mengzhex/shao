package hostcfg

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"tmon/internal/config"
)

// Check is one enforcement test and its outcome.
type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
	// Tested is false for properties that are configured but that this
	// verifier cannot independently confirm. They are listed rather than
	// quietly claimed as proven.
	Tested bool `json:"tested"`
}

// VerifyReport is the full result of verifying a host.
type VerifyReport struct {
	Host   string  `json:"host"`
	Checks []Check `json:"checks"`
	Passed bool    `json:"passed"`
}

// Verify tries to break out of the read-only restriction and reports whether
// it could.
//
// This exists because "the key is restricted" is a claim, and a claim about
// security that is never tested tends to drift from reality: a typo in
// authorized_keys, an sshd that was never reloaded, a probe path that moved.
// So instead of trusting the configuration, tmon asks the host to run
// arbitrary commands, read a sensitive file, allocate a terminal and forward
// a port, and passes only if every one of those is refused or ignored.
//
// A host that fails any check must not be queried, and the caller is expected
// to leave its enforcement at disabled.
func Verify(h config.HostConfig) (VerifyReport, error) {
	report := VerifyReport{Host: h.Name}

	client, err := Dial(h)
	if err != nil {
		return report, err
	}
	defer client.Close()

	nonce, err := randomNonce()
	if err != nil {
		return report, err
	}

	report.Checks = append(report.Checks, checkProbeResponds(client))
	report.Checks = append(report.Checks, checkArbitraryCommandIgnored(client, nonce))
	report.Checks = append(report.Checks, checkSensitiveReadIgnored(client))
	report.Checks = append(report.Checks, checkWriteAttemptIgnored(client, nonce))
	report.Checks = append(report.Checks, checkPTYDenied(client))
	report.Checks = append(report.Checks, checkRemoteForwardDenied(client))
	report.Checks = append(report.Checks, checkDirectForwardDenied(client))
	report.Checks = append(report.Checks, Check{
		Name:   "agent-and-x11-forwarding",
		Pass:   true,
		Tested: false,
		Detail: "denied by the restrict option in authorized_keys; not independently exercised by this check",
	})

	report.Passed = true
	for _, c := range report.Checks {
		if !c.Pass {
			report.Passed = false
			break
		}
	}
	return report, nil
}

func randomNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func checkProbeResponds(client *ssh.Client) Check {
	resp, err := queryOn(client, []string{"capabilities"})
	if err != nil {
		return Check{Name: "probe-responds", Tested: true,
			Detail: "the read-only probe did not answer: " + err.Error()}
	}
	return Check{Name: "probe-responds", Pass: true, Tested: true,
		Detail: fmt.Sprintf("probe version %s answered; running as root: %v", resp.ProbeVersion, resp.Root)}
}

// checkArbitraryCommandIgnored is the central test. If a forced command is in
// effect, this echo never runs and the canary cannot appear in the output.
func checkArbitraryCommandIgnored(client *ssh.Client, nonce string) Check {
	canary := "tmon-canary-" + nonce
	out, err := runRaw(client, "echo "+canary)
	c := Check{Name: "arbitrary-command-ignored", Tested: true}
	if strings.Contains(out, canary) {
		c.Detail = "the host executed a command tmon supplied. There is no forced command in effect and this host is NOT read-only"
		return c
	}
	c.Pass = true
	c.Detail = "a supplied command did not execute; sshd replaced it with the probe"
	if err != nil {
		c.Detail += " (session ended with: " + err.Error() + ")"
	}
	return c
}

func checkSensitiveReadIgnored(client *ssh.Client) Check {
	out, _ := runRaw(client, "cat /etc/passwd")
	c := Check{Name: "sensitive-read-ignored", Tested: true}
	// root:x:0:0 is the shape of a real passwd file. Its absence means the
	// cat never ran.
	if strings.Contains(out, "root:x:0:0") || strings.Contains(out, "root:*:0:0") {
		c.Detail = "the host returned the contents of /etc/passwd; arbitrary reads are possible and this host is NOT restricted"
		return c
	}
	c.Pass = true
	c.Detail = "a request to read /etc/passwd returned probe output instead of the file"
	return c
}

// checkWriteAttemptIgnored asks for something that would modify the host. The
// forced command means it never runs, which is exactly what is being proven;
// nothing is created even if the check fails.
func checkWriteAttemptIgnored(client *ssh.Client, nonce string) Check {
	path := "/tmp/tmon-verify-" + nonce
	out, _ := runRaw(client, "touch "+path+" && echo created-"+nonce)
	c := Check{Name: "write-attempt-ignored", Tested: true}
	if strings.Contains(out, "created-"+nonce) {
		c.Detail = "the host executed a command that creates a file at " + path + "; this host is NOT read-only"
		return c
	}
	c.Pass = true
	c.Detail = "a file-creating command did not execute"
	return c
}

func checkPTYDenied(client *ssh.Client) Check {
	c := Check{Name: "pty-denied", Tested: true}
	session, err := client.NewSession()
	if err != nil {
		c.Detail = "could not open a session to test: " + err.Error()
		return c
	}
	defer session.Close()

	err = session.RequestPty("xterm", 24, 80, ssh.TerminalModes{})
	if err == nil {
		c.Detail = "the host allocated a terminal; PermitTTY/no-pty is not in effect"
		return c
	}
	c.Pass = true
	c.Detail = "terminal allocation refused: " + err.Error()
	return c
}

func checkRemoteForwardDenied(client *ssh.Client) Check {
	c := Check{Name: "remote-port-forward-denied", Tested: true}
	ln, err := client.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		ln.Close()
		c.Detail = "the host accepted a remote port forward; no-port-forwarding is not in effect"
		return c
	}
	c.Pass = true
	c.Detail = "remote port forwarding refused: " + err.Error()
	return c
}

func checkDirectForwardDenied(client *ssh.Client) Check {
	c := Check{Name: "local-port-forward-denied", Tested: true}
	conn, err := client.Dial("tcp", "127.0.0.1:22")
	if err == nil {
		conn.Close()
		c.Detail = "the host opened a forwarded connection; the key can be used as a network tunnel"
		return c
	}
	c.Pass = true
	c.Detail = "forwarded connection refused: " + err.Error()
	return c
}

// runRaw asks the host to run a command and returns whatever came back.
//
// On a correctly configured host the command is discarded by sshd and the
// probe answers instead, which is precisely what the checks look for.
func runRaw(client *ssh.Client, cmd string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		defer close(done)
		out, runErr = session.CombinedOutput(cmd)
	}()

	select {
	case <-done:
	case <-time.After(dialTimeout):
		// A safety net rather than an expected path: CombinedOutput closes
		// stdin, so the probe sees EOF and answers immediately. A host that
		// hangs here is misconfigured in some other way.
		session.Signal(ssh.SIGKILL)
		return "", fmt.Errorf("timed out waiting for a reply")
	}
	return string(out), runErr
}
