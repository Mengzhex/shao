package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Mengzhex/shao/internal/config"
	"github.com/Mengzhex/shao/internal/hostcfg"
	"github.com/Mengzhex/shao/internal/probe"
)

func cmdHost(cfg *config.Config, args []string) {
	if len(args) == 0 {
		fail("usage: shao host add|list|verify|query ...")
	}
	switch args[0] {
	case "add":
		cmdHostAdd(cfg, args[1:])
	case "list":
		cmdHostList(cfg)
	case "verify":
		cmdHostVerify(cfg, args[1:])
	case "query":
		cmdHostQuery(cfg, args[1:])
	default:
		fail("unknown host subcommand %q (want add, list, verify or query)", args[0])
	}
}

func cmdHostAdd(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("host add", flag.ExitOnError)
	address := fs.String("address", "", "hostname or IP of the target (required)")
	user := fs.String("user", "", "SSH account to connect as (defaults to aiview for the root tier)")
	port := fs.Int("port", 22, "SSH port")
	tier := fs.String("tier", "user", `enforcement tier: "root" for a dedicated account plus an sshd_config Match block, "user" for a forced-command key in your own authorized_keys (no root needed)`)
	probePath := fs.String("probe-path", "", "where the probe binary will live on the target")
	positional := parseFlags(fs, args)

	if len(positional) < 1 {
		fail("usage: shao host add NAME --address ADDR [--user U] [--tier root|user]")
	}
	name := positional[0]
	if *address == "" {
		fail("--address is required")
	}
	if _, exists := cfg.Host(name); exists {
		fail("a host named %q is already configured; edit %s or pick another name", name, cfg.ConfigPath())
	}

	var t hostcfg.Tier
	switch *tier {
	case "root":
		t = hostcfg.TierRoot
	case "user":
		t = hostcfg.TierUser
	default:
		fail(`--tier must be "root" or "user"`)
	}

	account := *user
	if account == "" {
		if t == hostcfg.TierRoot {
			account = "aiview"
		} else {
			fail("--user is required for the no-root tier: it is the account whose authorized_keys gains the read-only key")
		}
	}

	privPath, publicKey, err := hostcfg.GenerateKey(cfg, name)
	check(err)

	host := config.HostConfig{
		Name:         name,
		Address:      *address,
		Port:         *port,
		User:         account,
		Enforcement:  t.Enforcement(),
		IdentityFile: privPath,
		ProbePath:    *probePath,
		Aspects:      probe.Verbs(),
	}

	localProbe := findProbeBinary()
	plan := hostcfg.BuildInstallPlan(host, t, publicKey, localProbe)
	check(plan.Save(cfg))
	host.ProbePath = plan.ProbePath

	// The host is written with no VerifiedAt, so it is not queryable yet.
	// Verification, not configuration, is what makes a host usable.
	cfg.Hosts = append(cfg.Hosts, host)
	check(cfg.Save())

	fmt.Printf("Added host %q, and generated a dedicated read-only key at:\n  %s\n\n", name, privPath)
	fmt.Println("This host is NOT queryable yet. Two steps remain, both on the target machine,")
	fmt.Println("because shao has no write access there and is not going to acquire any.")
	fmt.Println()

	fmt.Println("Step 1. Copy the probe binary and the install script to the target:")
	if localProbe == "" {
		fmt.Println()
		fmt.Printf("  The Linux probe binary was not found next to shao or in %s.\n", filepath.Join(cfg.Root(), "probe"))
		fmt.Println("  Build it with:")
		fmt.Println("    GOOS=linux GOARCH=amd64 go build -o shao-probe ./cmd/shao")
		fmt.Println("  (the same binary serves as the probe; sshd invokes it under the name shao-probe)")
		fmt.Println()
		fmt.Printf("  scp shao-probe %s@%s:/tmp/shao-probe\n", account, *address)
	} else {
		fmt.Printf("\n  scp %s %s@%s:/tmp/shao-probe\n", localProbe, account, *address)
	}
	fmt.Printf("  scp %s %s@%s:/tmp/shao-install.sh\n", plan.ScriptPath, account, *address)
	fmt.Println()

	fmt.Println("Step 2. Run the install script on the target:")
	if t == hostcfg.TierRoot {
		fmt.Printf("  ssh %s@%s 'sudo sh /tmp/shao-install.sh'\n", account, *address)
	} else {
		fmt.Printf("  ssh %s@%s 'sh /tmp/shao-install.sh'\n", account, *address)
	}
	fmt.Println()
	fmt.Printf("Then prove it worked:\n  shao host verify %s\n\n", name)
	fmt.Println("Until that check passes, shao refuses to query this host at all.")
	fmt.Printf("\nThe script is readable at %s; it installs one forced-command key and nothing else.\n", plan.ScriptPath)
}

// findProbeBinary looks for a Linux build of the probe to upload.
func findProbeBinary() string {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "shao-probe"),
			filepath.Join(dir, "shao-probe-linux-amd64"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".shao", "probe", "shao-probe"),
			filepath.Join(home, ".shao", "probe", "shao-probe-linux-amd64"),
		)
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

func cmdHostList(cfg *config.Config) {
	if len(cfg.Hosts) == 0 {
		fmt.Println("No hosts configured. Add one with `shao host add NAME --address ADDR`.")
		return
	}
	for _, h := range cfg.Hosts {
		state := "queryable"
		switch {
		case !h.Enforcement.Queryable():
			state = "disabled (no sshd-enforced read-only access)"
		case h.VerifiedAt == "":
			state = "unverified (run `shao host verify " + h.Name + "`)"
		}
		fmt.Printf("%-16s %s@%s:%d  %-22s %s\n",
			h.Name, h.User, h.Address, h.SSHPort(), h.Enforcement, state)
	}
}

func cmdHostVerify(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("host verify", flag.ExitOnError)
	positional := parseFlags(fs, args)
	if len(positional) < 1 {
		fail("usage: shao host verify NAME")
	}
	name := positional[0]

	host, ok := cfg.Host(name)
	if !ok {
		fail("no host named %q is configured", name)
	}
	if !host.Enforcement.Queryable() {
		fail("host %q has enforcement %q, so there is nothing to verify", name, host.Enforcement)
	}

	fmt.Printf("Verifying that %s refuses everything except the read-only probe.\n", name)
	fmt.Println("This deliberately attempts to run commands, read a sensitive file, allocate a")
	fmt.Println("terminal and forward a port. Every one of them must fail.")
	fmt.Println()

	report, err := hostcfg.Verify(*host)
	if err != nil {
		fail("could not verify %s: %v", name, err)
	}

	for _, c := range report.Checks {
		mark := "FAIL"
		if c.Pass {
			mark = "PASS"
		}
		if c.Pass && !c.Tested {
			mark = "n/a "
		}
		fmt.Printf("  [%s] %-28s %s\n", mark, c.Name, c.Detail)
	}
	fmt.Println()

	if !report.Passed {
		// A failure must not leave a half-trusted host behind.
		host.VerifiedAt = ""
		host.Enforcement = config.EnforceDisabled
		check(cfg.Save())
		fail("verification FAILED. %s has been marked disabled and will not be queried.\n"+
			"Re-run the install script on the target, make sure sshd was reloaded, then verify again.", name)
	}

	host.VerifiedAt = time.Now().UTC().Format(time.RFC3339)
	if host.HostKey == "" {
		if fp, err := hostcfg.FetchHostKey(*host); err == nil {
			host.HostKey = fp
			fmt.Printf("Pinned this host's key: %s\n", fp)
		}
	}
	check(cfg.Save())
	fmt.Printf("Verification passed. %s is now queryable, read-only.\n", name)
}

// cmdHostQuery runs a probe query from the command line, so the read-only
// path can be exercised without involving an AI client at all.
func cmdHostQuery(cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("host query", flag.ExitOnError)
	aspects := fs.String("aspects", "os,disk,mem,ports", "comma-separated aspects to read")
	check(fs.Parse(args))
	if fs.NArg() < 1 {
		fail("usage: shao host query NAME [--aspects os,disk,...]\nsupported aspects: %s",
			strings.Join(probe.Verbs(), ", "))
	}

	host, ok := cfg.Host(fs.Arg(0))
	if !ok {
		fail("no host named %q is configured", fs.Arg(0))
	}

	verbs := strings.Split(*aspects, ",")
	for i := range verbs {
		verbs[i] = strings.TrimSpace(verbs[i])
	}

	resp, err := hostcfg.Query(*host, verbs)
	check(err)

	fmt.Printf("host %s (probe %s, root=%v)\n", resp.Host, resp.ProbeVersion, resp.Root)
	for _, r := range resp.Results {
		fmt.Printf("\n=== %s ===\n", r.Verb)
		for _, sec := range r.Sections {
			fmt.Printf("\n$ %s\n", strings.Join(sec.Command, " "))
			if sec.Unavailable != "" {
				fmt.Printf("  (unavailable: %s)\n", sec.Unavailable)
			}
			if sec.Error != "" {
				fmt.Printf("  (error: %s)\n", sec.Error)
			}
			if out := strings.TrimRight(sec.Output, "\n"); out != "" {
				fmt.Println(out)
			}
		}
	}
}
