package mcpsrv

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"tmon/internal/config"
	"tmon/internal/hostcfg"
	"tmon/internal/probe"
)

// deployAspects is what get_deploy_context gathers. It is the set of facts
// that actually decide a deployment: whether there is room, whether the port
// is free, what container and web tooling is already there, and whether
// something like SELinux is going to silently block it.
var deployAspects = []string{"os", "cpu", "mem", "disk", "ports", "docker", "compose", "nginx", "systemd", "selinux"}

func (s *Server) registerHostTools() {
	aspectNames := probe.Verbs()

	s.register(toolDef{
		Name:  "list_hosts",
		Title: "List configured hosts",
		Description: `List the remote hosts tmon can read facts from, and how read-only access to each is enforced.

A host appears as queryable only when its target machine pins tmon's SSH key to a read-only probe and that restriction has been verified. Hosts that could not be set up that way are listed as disabled and cannot be queried at all; there is no weaker mode.`,
		InputSchema: schema(map[string]any{}),
		Annotations: readOnlyAnnotations("List configured hosts"),
	}, s.toolListHosts)

	s.register(toolDef{
		Name:  "get_host_env",
		Title: "Read host environment facts",
		Description: `Read facts about a configured host: disk space, memory, CPU, listening ports, docker, nginx, systemd units.

Each aspect maps to a fixed set of read-only commands chosen on the target machine, not composed here. The response echoes the exact command that produced each section, so what ran is visible rather than assumed.

Use this to ground deployment advice in what is actually on the host. Report the commands the user should run themselves; tmon cannot run them.`,
		InputSchema: schema(map[string]any{
			"host": prop("string", "Which configured host to read. Use list_hosts to see the names."),
			"aspects": map[string]any{
				"type":        "array",
				"description": "Which aspects to read. Defaults to a general set covering the whole machine.",
				"items":       map[string]any{"type": "string", "enum": aspectNames},
			},
		}, "host"),
		Annotations: readOnlyAnnotations("Read host environment facts"),
	}, s.toolGetHostEnv)

	s.register(toolDef{
		Name:  "get_deploy_context",
		Title: "Gather everything needed to plan a deployment",
		Description: `Gather in one call the facts a deployment decision depends on: OS and kernel, CPU and memory, free disk, listening ports, docker and compose state, nginx configuration, running and failed services, and SELinux or AppArmor enforcement.

Call this when the user is planning to deploy something to a configured host, then write out the commands for them to run. tmon has no ability to execute any of them.`,
		InputSchema: schema(map[string]any{
			"host": prop("string", "Which configured host to inspect."),
		}, "host"),
		Annotations: readOnlyAnnotations("Gather deployment context"),
	}, s.toolGetDeployContext)
}

func (s *Server) toolListHosts(_ json.RawMessage) *callToolResult {
	if len(s.cfg.Hosts) == 0 {
		return textResult("No hosts are configured. A host is added with `tmon host add <name> --address <addr>`, which generates a dedicated read-only key and prints a setup script to run on the target.")
	}

	var queryable, blocked []string
	for _, h := range s.cfg.Hosts {
		line := fmt.Sprintf("- %s (%s@%s:%d) enforcement=%s",
			h.Name, h.User, h.Address, h.SSHPort(), h.Enforcement)
		switch {
		case !h.Enforcement.Queryable():
			blocked = append(blocked, line+" -- not queryable: this host has no sshd-enforced read-only access")
		case h.VerifiedAt == "":
			blocked = append(blocked, line+" -- not queryable: enforcement has never been verified (`tmon host verify "+h.Name+"`)")
		default:
			detail := line + fmt.Sprintf(" verified=%s", h.VerifiedAt)
			if h.Enforcement == config.EnforceForcedCommandUser {
				detail += " (no-root tier: commands needing privilege report as unavailable)"
			}
			queryable = append(queryable, detail)
		}
	}

	var b strings.Builder
	if len(queryable) > 0 {
		b.WriteString("Queryable hosts:\n")
		b.WriteString(strings.Join(queryable, "\n"))
		b.WriteString("\n")
	} else {
		b.WriteString("No hosts are currently queryable.\n")
	}
	if len(blocked) > 0 {
		b.WriteString("\nNot queryable:\n")
		b.WriteString(strings.Join(blocked, "\n"))
		b.WriteString("\n")
	}
	return textResult(b.String())
}

func (s *Server) toolGetHostEnv(raw json.RawMessage) *callToolResult {
	var args struct {
		Host    string   `json:"host"`
		Aspects []string `json:"aspects"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return errorResult("%v", err)
	}
	if strings.TrimSpace(args.Host) == "" {
		return errorResult("host is required; call list_hosts to see the configured names")
	}
	aspects := args.Aspects
	if len(aspects) == 0 {
		aspects = []string{"os", "cpu", "mem", "disk", "ports", "docker", "nginx"}
	}
	return s.queryHost(args.Host, aspects, false)
}

func (s *Server) toolGetDeployContext(raw json.RawMessage) *callToolResult {
	var args struct {
		Host string `json:"host"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return errorResult("%v", err)
	}
	if strings.TrimSpace(args.Host) == "" {
		return errorResult("host is required; call list_hosts to see the configured names")
	}
	return s.queryHost(args.Host, deployAspects, true)
}

// queryHost is the single path to a remote host. Everything about the
// restriction is enforced before and beyond this point: the verb list is
// validated locally, the host must be a verified queryable one, and the key
// used cannot run anything but the probe.
func (s *Server) queryHost(name string, aspects []string, deployFraming bool) *callToolResult {
	host, ok := s.cfg.Host(name)
	if !ok {
		return errorResult("no host named %q is configured; call list_hosts to see the names", name)
	}
	if !host.Enforcement.Queryable() {
		return errorResult(
			"host %q is configured with enforcement=%s, so environment queries are disabled for it. tmon deliberately has no mode that queries a host without the target machine enforcing read-only access.",
			name, host.Enforcement)
	}
	if host.VerifiedAt == "" {
		return errorResult(
			"host %q has never passed `tmon host verify`, so tmon will not query it. Run that command to confirm the target actually refuses anything but the read-only probe.",
			name)
	}

	// Deduplicate while preserving the caller's order.
	seen := map[string]bool{}
	verbs := make([]string, 0, len(aspects))
	for _, a := range aspects {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		verbs = append(verbs, a)
	}

	resp, err := hostcfg.Query(*host, verbs)
	if err != nil {
		return errorResult("querying %s failed: %v", name, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "host %s (%s), enforcement %s\n", host.Name, host.Address, host.Enforcement)
	if resp.Host != "" {
		fmt.Fprintf(&b, "reported hostname: %s\n", resp.Host)
	}
	if resp.Root {
		b.WriteString("probe runs as root, so privileged read-only commands are available\n")
	} else {
		b.WriteString("probe runs unprivileged; sections needing root are reported as unavailable rather than attempted\n")
	}
	b.WriteString("every command below was chosen on the target host from a fixed list and only reads state\n")

	if deployFraming {
		b.WriteString("\nThis is the state of the machine as it is now. Use it to write deployment commands for the user to run; tmon cannot execute anything.\n")
	}

	for _, r := range resp.Results {
		fmt.Fprintf(&b, "\n=== %s ===\n", r.Verb)
		for _, sec := range r.Sections {
			fmt.Fprintf(&b, "\n$ %s\n", strings.Join(sec.Command, " "))
			switch {
			case sec.Unavailable != "":
				fmt.Fprintf(&b, "  (unavailable: %s)\n", sec.Unavailable)
			case sec.Error != "":
				fmt.Fprintf(&b, "  (error: %s)\n", sec.Error)
			}
			if out := strings.TrimRight(sec.Output, "\n"); out != "" {
				b.WriteString(s.scrub(out))
				b.WriteString("\n")
			}
			if sec.Truncated {
				b.WriteString("  (output truncated)\n")
			}
		}
	}
	return textResult(b.String())
}

// sortedAspects is used by help output to present the menu deterministically.
func sortedAspects() []string {
	out := append([]string(nil), deployAspects...)
	sort.Strings(out)
	return out
}
