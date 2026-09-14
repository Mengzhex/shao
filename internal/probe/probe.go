// Package probe is the read-only agent that runs on a target host.
//
// It is the executable half of shao's read-only guarantee. The other half is
// sshd: the key shao uses is installed with a forced command, so no matter
// what an SSH client asks to run, sshd runs this and only this. That is why
// the guarantee holds even if everything on the client side were compromised.
//
// The design rule here is narrow and deliberate: the caller picks a verb from
// a fixed list and supplies nothing else. There is no parameter anywhere in
// this package that becomes part of a command line, no shell is ever invoked,
// and every command is a literal argv written in this file. An AI asking for
// host facts is choosing from a menu, not composing a command, so there is no
// string for a prompt injection to travel through.
//
// Every response echoes the exact argv that ran, so the claim "this only
// reads" is checkable from the output rather than taken on trust.
package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	// maxSectionBytes caps one command's captured output. nginx -T on a busy
	// server can be large, and an unbounded response would be a denial of
	// service against the caller rather than a useful answer.
	maxSectionBytes = 256 << 10
	// commandTimeout bounds a single command. A wedged docker daemon must
	// not hang the whole probe.
	commandTimeout = 10 * time.Second
)

// Request is one line of JSON on stdin.
type Request struct {
	// Verbs are chosen from the fixed set returned by Verbs(). Unknown names
	// are rejected rather than ignored, so a typo is visible instead of
	// silently returning less than was asked for.
	Verbs []string `json:"verbs"`
}

// Section is the result of running one fixed command.
type Section struct {
	// Command is the literal argv that ran. It is echoed so a reader can
	// verify that nothing but a read took place.
	Command []string `json:"command"`
	Output  string   `json:"output,omitempty"`
	Error   string   `json:"error,omitempty"`
	// Unavailable explains why a section produced nothing: the tool is not
	// installed, or it needs privileges this account does not have.
	Unavailable string `json:"unavailable,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// VerbResult groups the sections belonging to one verb.
type VerbResult struct {
	Verb     string    `json:"verb"`
	Sections []Section `json:"sections"`
}

// Response is one line of JSON on stdout.
type Response struct {
	OK      bool         `json:"ok"`
	Error   string       `json:"error,omitempty"`
	Host    string       `json:"host,omitempty"`
	OS      string       `json:"os,omitempty"`
	Root    bool         `json:"root"`
	Results []VerbResult `json:"results,omitempty"`
	// Verbs lists everything this probe supports, returned by the
	// "capabilities" verb so a client can discover an older probe's menu.
	Verbs []string `json:"verbs,omitempty"`
	// ProbeVersion lets a client notice a stale probe binary on a host.
	ProbeVersion string `json:"probe_version,omitempty"`
}

// ProbeVersion is bumped when the verb set changes.
const ProbeVersion = "1"

// command is one literal invocation. Nothing in it is ever built from input.
type command struct {
	argv []string
	// needsRoot marks commands whose useful output requires privilege. They
	// are skipped with an explanation rather than run and failed.
	needsRoot bool
	// optional means the tool being absent is normal, not an error: a host
	// without docker is a fact about the host, not a fault.
	optional bool
}

// verb is a named group of commands.
type verb struct {
	name        string
	description string
	commands    []command
}

// verbs is the entire menu. Adding to this list is the only way to widen what
// a probe can do, which keeps the audit surface to a single readable table.
var verbs = []verb{
	{
		name:        "os",
		description: "distribution, kernel, uptime",
		commands: []command{
			{argv: []string{"cat", "/etc/os-release"}, optional: true},
			{argv: []string{"uname", "-a"}},
			{argv: []string{"uptime"}, optional: true},
		},
	},
	{
		name:        "cpu",
		description: "core count, model, load average",
		commands: []command{
			{argv: []string{"nproc"}, optional: true},
			{argv: []string{"cat", "/proc/loadavg"}, optional: true},
			{argv: []string{"lscpu"}, optional: true},
		},
	},
	{
		name:        "mem",
		description: "memory and swap totals and usage",
		commands: []command{
			{argv: []string{"free", "-b"}, optional: true},
			{argv: []string{"cat", "/proc/meminfo"}, optional: true},
		},
	},
	{
		name:        "disk",
		description: "filesystem capacity and inode usage",
		commands: []command{
			{argv: []string{"df", "-PB1"}},
			{argv: []string{"df", "-PBi"}, optional: true},
		},
	},
	{
		name:        "ports",
		description: "listening TCP and UDP sockets",
		commands: []command{
			// Without privilege the sockets are still listed; only the owning
			// process names are missing. Both are attempted so a non-root
			// probe still answers the question that matters for a deploy:
			// is this port already taken.
			{argv: []string{"ss", "-tulnH"}, optional: true},
			{argv: []string{"ss", "-tulpnH"}, needsRoot: true, optional: true},
		},
	},
	{
		name:        "docker",
		description: "docker daemon, running containers, images",
		commands: []command{
			{argv: []string{"docker", "version", "--format", "{{.Server.Version}}"}, optional: true},
			{argv: []string{"docker", "ps", "--all", "--no-trunc", "--format",
				"{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}"}, optional: true},
			{argv: []string{"docker", "images", "--format", "{{.Repository}}:{{.Tag}}\t{{.Size}}"}, optional: true},
			{argv: []string{"docker", "system", "df"}, optional: true},
		},
	},
	{
		name:        "compose",
		description: "docker compose projects",
		commands: []command{
			{argv: []string{"docker", "compose", "ls", "--all"}, optional: true},
		},
	},
	{
		name:        "nginx",
		description: "nginx version, config test, effective configuration",
		commands: []command{
			{argv: []string{"nginx", "-v"}, optional: true},
			// -t and -T only read and print; neither reloads or restarts.
			{argv: []string{"nginx", "-t"}, needsRoot: true, optional: true},
			{argv: []string{"nginx", "-T"}, needsRoot: true, optional: true},
		},
	},
	{
		name:        "systemd",
		description: "running services and failed units",
		commands: []command{
			{argv: []string{"systemctl", "list-units", "--type=service", "--state=running",
				"--no-pager", "--plain", "--no-legend"}, optional: true},
			{argv: []string{"systemctl", "list-units", "--state=failed",
				"--no-pager", "--plain", "--no-legend"}, optional: true},
		},
	},
	{
		name:        "selinux",
		description: "SELinux or AppArmor enforcement state, which silently breaks deploys",
		commands: []command{
			{argv: []string{"getenforce"}, optional: true},
			{argv: []string{"aa-status", "--enabled"}, optional: true},
		},
	},
}

// Verbs returns the supported verb names, sorted.
func Verbs() []string {
	out := make([]string, 0, len(verbs)+1)
	for _, v := range verbs {
		out = append(out, v.name)
	}
	out = append(out, "capabilities")
	sort.Strings(out)
	return out
}

// Describe returns verb names with their descriptions, for help text and for
// the MCP tool schema.
func Describe() map[string]string {
	out := make(map[string]string, len(verbs))
	for _, v := range verbs {
		out[v.name] = v.description
	}
	return out
}

func lookupVerb(name string) (verb, bool) {
	for _, v := range verbs {
		if v.name == name {
			return v, true
		}
	}
	return verb{}, false
}

// Serve runs the probe protocol: one JSON request line in, one JSON response
// line out, then exit.
//
// A single request per invocation matches how sshd forced commands work, and
// keeps the probe from becoming a long-lived service with state worth
// attacking.
func Serve(in io.Reader, out io.Writer) error {
	// Whatever the client asked sshd to run is reported in SSH_ORIGINAL_COMMAND
	// when a forced command is in effect. It is deliberately never executed;
	// reading it here only documents that it is discarded.
	_ = os.Getenv("SSH_ORIGINAL_COMMAND")

	resp := handle(in)
	enc := json.NewEncoder(out)
	return enc.Encode(resp)
}

func handle(in io.Reader) Response {
	root := isRoot()
	hostname, _ := os.Hostname()
	base := Response{Root: root, Host: hostname, OS: runtime.GOOS, ProbeVersion: ProbeVersion}

	reader := bufio.NewReader(io.LimitReader(in, 64<<10))
	line, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		base.Error = "read request: " + err.Error()
		return base
	}
	line = trimSpaceBytes(line)
	if len(line) == 0 {
		// An empty request is treated as a capability query, which makes a
		// bare `ssh host` a harmless way to check the probe is alive.
		base.OK = true
		base.Verbs = Verbs()
		return base
	}

	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		base.Error = "malformed request: expected one line of JSON"
		return base
	}
	if len(req.Verbs) == 0 {
		base.OK = true
		base.Verbs = Verbs()
		return base
	}

	// Validate every verb before running anything, so a request that is
	// partly wrong fails cleanly instead of half-executing.
	resolved := make([]verb, 0, len(req.Verbs))
	for _, name := range req.Verbs {
		if name == "capabilities" {
			base.Verbs = Verbs()
			continue
		}
		v, ok := lookupVerb(name)
		if !ok {
			base.Error = fmt.Sprintf("unknown verb %q; supported: %s",
				name, strings.Join(Verbs(), ", "))
			return base
		}
		resolved = append(resolved, v)
	}

	ctx := context.Background()
	for _, v := range resolved {
		base.Results = append(base.Results, runVerb(ctx, v, root))
	}
	base.OK = true
	return base
}

func runVerb(ctx context.Context, v verb, root bool) VerbResult {
	res := VerbResult{Verb: v.name}
	for _, c := range v.commands {
		res.Sections = append(res.Sections, runCommand(ctx, c, root))
	}
	return res
}

func runCommand(ctx context.Context, c command, root bool) Section {
	s := Section{Command: c.argv}

	if c.needsRoot && !root {
		s.Unavailable = "requires root; this host is configured for the non-root enforcement tier"
		return s
	}
	path, err := exec.LookPath(c.argv[0])
	if err != nil {
		s.Unavailable = "not installed: " + c.argv[0]
		return s
	}

	runCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	// exec.CommandContext takes an argv directly. No shell is involved, so
	// there is nothing for shell metacharacters to mean.
	cmd := exec.CommandContext(runCtx, path, c.argv[1:]...)
	cmd.Env = minimalEnv()
	output, err := cmd.CombinedOutput()

	if len(output) > maxSectionBytes {
		output = output[:maxSectionBytes]
		s.Truncated = true
	}
	s.Output = string(output)
	if err != nil {
		if runCtx.Err() != nil {
			s.Error = "timed out after " + commandTimeout.String()
		} else if c.optional {
			s.Unavailable = "command failed: " + err.Error()
			s.Error = ""
		} else {
			s.Error = err.Error()
		}
	}
	return s
}

// minimalEnv gives child commands a predictable environment rather than
// inheriting whatever sshd happened to set.
func minimalEnv() []string {
	return []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LC_ALL=C",
		"LANG=C",
		"HOME=" + os.Getenv("HOME"),
	}
}

func isRoot() bool { return os.Geteuid() == 0 }

func trimSpaceBytes(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
