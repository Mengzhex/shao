// Command shao records terminal sessions and serves them, read-only, to an
// AI assistant over the Model Context Protocol.
//
// The shape of the tool in one view:
//
//	shao start          turn monitoring on: automatic recording for new
//	                    terminals, the AI client configuration, and recording
//	                    of this terminal
//	shao end            turn monitoring off, keeping everything recorded so far
//	shao shell          record just this terminal
//	shao run -- CMD     record one command
//	shao sessions       what has been recorded
//	shao tail           read a session yourself, without an AI
//	shao mcp            speak MCP on stdin/stdout, for a client that launches it
//	shao serve          serve MCP on a loopback HTTP port instead
//	shao host add       set up read-only access to a target host
//	shao host verify    prove that host really does refuse everything else
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/Mengzhex/shao/internal/config"
	"github.com/Mengzhex/shao/internal/probe"
)

// usage is deliberately short. Almost everyone needs three commands, and a
// wall of options is its own kind of failure: it makes a tool look like it
// demands decisions it does not. The rest is one `shao help --all` away.
const usage = `shao records your terminal and lets an AI read it back, read-only.

  shao start                 record this terminal, make new terminals record
                             themselves, and print the AI client configuration
  shao start --port 7337     the same, plus serve MCP at a URL for clients
                             that need one (stays up in the background)
  shao end                   stop all of it; everything recorded is kept

Then just ask your AI why something failed.

That is the whole thing. "shao serve" exists to run the URL endpoint on its
own, without recording, but --port above covers the usual case.

  shao help --all     every command and option
`

const usageAll = `shao records your terminal and lets an AI read it back, read-only.

Turning it on and off:
  shao start [--label NAME]                 monitoring on: hook, config, and
                                            record this terminal
  shao start --port N [--bind A] [--allow C]   the same, plus a URL endpoint
  shao end [--keep-hook] [--keep-shells]    monitoring off; history is kept

Recording one thing only:
  shao shell [--label NAME] [--quiet]       record just this terminal
  shao run -- CMD ...                       record a single command
  shao hook install | uninstall | status    control automatic recording

Reading it yourself:
  shao sessions [--all|--live|--hours N]    what has been recorded
  shao tail [SESSION] [--lines N] [--raw]   print the end of a session
  shao last-error [SESSION] [--lines N]     the most recent failed command

AI client plumbing:
  shao mcp                                  speak MCP on stdin/stdout
  shao serve [--port N] [--foreground]      serve MCP over HTTP
  shao serve --status | --stop              check on, or stop, that endpoint
  shao serve --bind ADDR [--allow CIDR]     let another machine connect
  shao mcp-config [--format claude|json|http]
  shao token rotate                         replace the HTTP bearer token

Remote hosts (read-only environment facts):
  shao host add NAME --address ADDR [--user U] [--tier root|user]
  shao host list
  shao host verify NAME                     prove the host refuses anything else
  shao host query NAME [--aspects ...]

Other:
  shao config path | show | init
  shao version

Global:
  --home DIR    use a different state directory (default ~/.shao, or $SHAO_HOME)

Sessions are named with --label and selected as "latest" (the default),
"cwd:SUBSTRING", "label:NAME", "host:NAME", or a session id. With several
terminals open, cwd: is usually the one you want: "cwd:webapp" beats copying
an id. A session is attributed to a host through the sessions: rules in the
config file.
`

func main() {
	// A copy of this binary installed on a target host as "shao-probe" is
	// launched by sshd as a forced command with no arguments, so the name it
	// was invoked under is what selects probe mode.
	if strings.HasPrefix(filepath.Base(os.Args[0]), "shao-probe") {
		if err := probe.Serve(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "shao-probe:", err)
			os.Exit(1)
		}
		return
	}

	args := os.Args[1:]
	root := ""

	// --home is accepted before the subcommand so every command shares it.
	for len(args) > 0 && strings.HasPrefix(args[0], "--home") {
		if args[0] == "--home" {
			if len(args) < 2 {
				fail("--home needs a directory")
			}
			root, args = args[1], args[2:]
			continue
		}
		if v, ok := strings.CutPrefix(args[0], "--home="); ok {
			root, args = v, args[1:]
			continue
		}
		break
	}

	if len(args) == 0 {
		fmt.Print(usage)
		return
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "--help", "help":
		if len(rest) > 0 && (rest[0] == "--all" || rest[0] == "-a" || rest[0] == "all") {
			fmt.Print(usageAll)
		} else {
			fmt.Print(usage)
		}
		return
	case "version", "--version":
		fmt.Println("shao", version())
		return
	}

	cfg, err := load(root)
	if err != nil {
		fail("%v", err)
	}

	switch cmd {
	case "shell":
		cmdShell(cfg, rest)
	case "run":
		cmdRun(cfg, rest)
	case "sessions":
		cmdSessions(cfg, rest)
	case "tail":
		cmdTail(cfg, rest)
	case "last-error":
		cmdLastError(cfg, rest)
	case "mcp":
		cmdMCP(cfg, rest)
	case "start":
		cmdStart(cfg, rest)
	case "end", "stop":
		cmdEnd(cfg, rest)
	case "serve":
		cmdServe(cfg, rest)
	case "mcp-config":
		cmdMCPConfig(cfg, rest)
	case "token":
		cmdToken(cfg, rest)
	case "host":
		cmdHost(cfg, rest)
	case "hook":
		cmdHook(cfg, rest)
	case "config":
		cmdConfig(cfg, rest)
	case "probe":
		// Explicit probe mode, for testing the protocol locally.
		if err := probe.Serve(os.Stdin, os.Stdout); err != nil {
			fail("%v", err)
		}
	default:
		fail("unknown command %q\n\n%s", cmd, usage)
	}
}

// buildVersion is overridden at link time by the release build.
var buildVersion = "dev"

// version reports which build this is.
//
// The link-time stamp is only present in binaries produced by
// scripts/release.sh. `go install <module>@v1.2.3` compiles from source
// without those flags, so the tag has to be recovered from the build info the
// toolchain embeds instead -- otherwise a go-installed shao reports "dev" and
// a bug report cannot say which version it came from.
func version() string {
	if buildVersion != "dev" {
		return buildVersion
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return buildVersion
}

// load reads the configuration and reports any warnings to stderr.
//
// Warnings go to stderr rather than stdout because `shao mcp` speaks a
// protocol on stdout, and a stray line there would break the client.
func load(root string) (*config.Config, error) {
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	warnings, err := cfg.Validate()
	if err != nil {
		return nil, fmt.Errorf("configuration problem: %w", err)
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "shao: warning:", w)
	}
	return cfg, nil
}

// parseFlags parses args while allowing flags and positional arguments in any
// order, and returns the positionals.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `shao tail deploy --lines 500` would silently ignore --lines and
// `shao host add web-1 --address 10.0.0.1` would lose every flag. Both read
// perfectly naturally and are the forms people actually type, so the
// arguments are separated here first and the flags handed over on their own.
//
// A literal "--" ends the permutation: everything after it is positional,
// which is what `shao run -- ./deploy.sh --flag` depends on.
func parseFlags(fs *flag.FlagSet, args []string) []string {
	var flagArgs, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		if strings.Contains(a, "=") {
			continue
		}
		// A non-boolean flag written as "--lines 500" takes the next
		// argument with it; a boolean one does not.
		f := fs.Lookup(strings.TrimLeft(a, "-"))
		if f == nil {
			continue
		}
		if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	check(fs.Parse(flagArgs))
	return positional
}

// boolFlag matches the unexported interface the flag package uses to identify
// flags that do not consume a following argument.
type boolFlag interface {
	IsBoolFlag() bool
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "shao: "+format+"\n", args...)
	os.Exit(1)
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}
