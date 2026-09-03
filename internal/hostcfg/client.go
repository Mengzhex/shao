package hostcfg

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"tmon/internal/config"
	"tmon/internal/probe"
)

const dialTimeout = 15 * time.Second

// forcedCommandPlaceholder is what tmon asks sshd to run.
//
// With a forced command configured, sshd discards this entirely and runs the
// probe. It is sent as a readable string rather than an empty one so that
// anything logging SSH_ORIGINAL_COMMAND on the target shows plainly what
// tmon requested and that it was replaced.
const forcedCommandPlaceholder = "tmon-probe"

// Dial opens an SSH connection using the host's dedicated read-only key.
//
// The host key is pinned when the config records a fingerprint. Without
// pinning, an attacker who can redirect the connection sees the probe
// requests; they still cannot use the key for anything else, because the
// restriction lives on the real server, but the exposure is real enough to
// warn about.
func Dial(h config.HostConfig) (*ssh.Client, error) {
	keyPath := config.ExpandUser(h.IdentityFile)
	pemBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read identity file %s: %w", keyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parse identity file %s: %w", keyPath, err)
	}

	cb, err := hostKeyCallback(h)
	if err != nil {
		return nil, err
	}

	user := h.User
	if user == "" {
		user = "aiview"
	}
	addr := net.JoinHostPort(h.Address, strconv.Itoa(h.SSHPort()))
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: cb,
		Timeout:         dialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	return client, nil
}

func hostKeyCallback(h config.HostConfig) (ssh.HostKeyCallback, error) {
	want := strings.TrimSpace(h.HostKey)
	if want == "" {
		// Unpinned. Accepted so a host can be set up before its fingerprint
		// is known, and reported as a warning by config.Validate.
		return func(string, net.Addr, ssh.PublicKey) error { return nil }, nil
	}
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		got := ssh.FingerprintSHA256(key)
		if got != want {
			return fmt.Errorf("host key mismatch for %s: expected %s, got %s", h.Name, want, got)
		}
		return nil
	}, nil
}

// FetchHostKey connects once to learn a host's key fingerprint, so it can be
// pinned. It is used by `tmon host add`, never on the query path.
func FetchHostKey(h config.HostConfig) (string, error) {
	var fingerprint string
	user := h.User
	if user == "" {
		user = "aiview"
	}
	addr := net.JoinHostPort(h.Address, strconv.Itoa(h.SSHPort()))
	_, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: user,
		// No auth method: the handshake reaches the host key and then fails
		// authentication, which is all this needs.
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fingerprint = ssh.FingerprintSHA256(key)
			return nil
		},
		Timeout: dialTimeout,
	})
	if fingerprint != "" {
		return fingerprint, nil
	}
	return "", fmt.Errorf("could not read host key from %s: %w", addr, err)
}

// Query runs one probe request against a host and returns its response.
//
// The verbs are validated against the local verb list before the connection
// is made, so a bad request costs nothing and an unknown verb can never be
// forwarded to a host.
func Query(h config.HostConfig, verbs []string) (probe.Response, error) {
	if !h.Enforcement.Queryable() {
		return probe.Response{}, fmt.Errorf(
			"host %q has enforcement %q: environment queries are disabled for it", h.Name, h.Enforcement)
	}
	if h.VerifiedAt == "" {
		return probe.Response{}, fmt.Errorf(
			"host %q has never passed `tmon host verify`; refusing to query it", h.Name)
	}
	known := map[string]bool{}
	for _, v := range probe.Verbs() {
		known[v] = true
	}
	for _, v := range verbs {
		if !known[v] {
			return probe.Response{}, fmt.Errorf("unknown verb %q; supported: %s",
				v, strings.Join(probe.Verbs(), ", "))
		}
	}

	client, err := Dial(h)
	if err != nil {
		return probe.Response{}, err
	}
	defer client.Close()
	return queryOn(client, verbs)
}

func queryOn(client *ssh.Client, verbs []string) (probe.Response, error) {
	session, err := client.NewSession()
	if err != nil {
		return probe.Response{}, err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return probe.Response{}, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return probe.Response{}, err
	}

	if err := session.Start(forcedCommandPlaceholder); err != nil {
		return probe.Response{}, fmt.Errorf("start probe: %w", err)
	}

	req, err := json.Marshal(probe.Request{Verbs: verbs})
	if err != nil {
		return probe.Response{}, err
	}
	if _, err := stdin.Write(append(req, '\n')); err != nil {
		return probe.Response{}, err
	}
	stdin.Close()

	var resp probe.Response
	decodeErr := json.NewDecoder(stdout).Decode(&resp)
	waitErr := session.Wait()

	if decodeErr != nil {
		// A non-JSON reply is the signature of a host where the forced
		// command is not actually in place: something else answered.
		if waitErr != nil {
			return probe.Response{}, fmt.Errorf("probe did not return JSON (%v); session error: %v", decodeErr, waitErr)
		}
		return probe.Response{}, fmt.Errorf("probe did not return JSON: %w", decodeErr)
	}
	if !resp.OK && resp.Error != "" {
		return resp, fmt.Errorf("probe error: %s", resp.Error)
	}
	return resp, nil
}
