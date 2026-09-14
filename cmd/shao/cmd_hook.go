package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Mengzhex/shao/internal/config"
)

// The hook is the answer to "I want it to just record, without thinking about
// it". Recording has to begin when a shell begins, so the only way to cover
// every new terminal is for the shell's own startup file to hand over to
// shao. That is a change to the user's dotfiles, so it happens only when they
// ask for it, it is fenced with markers, the original is backed up, and
// uninstall puts things back.

const (
	hookBegin = "# >>> shao terminal recording >>>"
	hookEnd   = "# <<< shao terminal recording <<<"
)

// posixHook re-executes the shell under shao.
//
// The guards matter. SHAO_RECORDING is set inside a recorded shell, so the
// rc file being read again by that shell does not recurse. The -t test skips
// non-interactive shells, which is what scp, rsync and remote command
// execution rely on: recording those would corrupt their protocols.
const posixHook = `if [ -z "$SHAO_RECORDING" ] && [ -t 0 ] && [ -t 1 ] && command -v shao >/dev/null 2>&1; then
  case "$-" in
    *i*) exec shao shell ;;
  esac
fi`

// pwshHook is the PowerShell equivalent. Exiting afterwards makes closing the
// recorded shell close the window, matching what exec does on POSIX.
const pwshHook = `if (-not $env:SHAO_RECORDING -and [Environment]::UserInteractive -and (Get-Command shao -ErrorAction SilentlyContinue)) {
  shao shell
  exit
}`

func cmdHook(cfg *config.Config, args []string) {
	if len(args) == 0 {
		fail("usage: shao hook install|uninstall|status")
	}
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	profile := fs.String("profile", "", "modify a specific startup file instead of the detected ones")
	dryRun := fs.Bool("dry-run", false, "show what would change without writing anything")
	check(fs.Parse(args[1:]))

	targets := hookTargets(*profile)
	if len(targets) == 0 {
		fail("could not find a shell startup file to modify; pass one with --profile")
	}

	switch args[0] {
	case "install":
		hookInstall(targets, *dryRun)
	case "uninstall":
		hookUninstall(targets, *dryRun)
	case "status":
		hookStatus(targets)
	default:
		fail("unknown hook subcommand %q (want install, uninstall or status)", args[0])
	}
}

type hookTarget struct {
	path    string
	snippet string
	comment string
}

func hookTargets(explicit string) []hookTarget {
	if explicit != "" {
		snippet := posixHook
		if strings.EqualFold(filepath.Ext(explicit), ".ps1") {
			snippet = pwshHook
		}
		return []hookTarget{{path: explicit, snippet: snippet, comment: "explicit"}}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	var targets []hookTarget
	if runtime.GOOS == "windows" {
		for _, rel := range []string{
			filepath.Join("Documents", "PowerShell", "Microsoft.PowerShell_profile.ps1"),
			filepath.Join("Documents", "WindowsPowerShell", "Microsoft.PowerShell_profile.ps1"),
		} {
			targets = append(targets, hookTarget{
				path: filepath.Join(home, rel), snippet: pwshHook, comment: "PowerShell profile",
			})
		}
		return targets
	}

	// On POSIX only existing rc files are touched: creating a .zshrc for
	// someone who does not use zsh would be presumptuous.
	for _, rel := range []string{".bashrc", ".zshrc"} {
		path := filepath.Join(home, rel)
		if _, err := os.Stat(path); err == nil {
			targets = append(targets, hookTarget{path: path, snippet: posixHook, comment: rel})
		}
	}
	if len(targets) == 0 {
		targets = append(targets, hookTarget{
			path: filepath.Join(home, ".bashrc"), snippet: posixHook, comment: ".bashrc",
		})
	}
	return targets
}

// ensureHook installs the startup hook if it is not there already, and says
// plainly what it did.
//
// This runs as part of `shao start` because "install it once and every new
// terminal is covered" is the behaviour people expect, and leaving it as a
// separate optional step meant most terminals silently went unrecorded. It
// still edits a startup file, so it backs the file up, announces itself, and
// points at the command that undoes it.
func ensureHook() {
	targets := hookTargets("")
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "shao: could not find a shell startup file; only this terminal will be recorded")
		return
	}

	for _, t := range targets {
		data, err := os.ReadFile(t.path)
		if err == nil && strings.Contains(string(data), hookBegin) {
			fmt.Println("New terminals already record themselves automatically.")
			return
		}
	}

	fmt.Println("Setting up automatic recording for new terminals.")
	hookInstall(targets[:1], false)
	fmt.Println("Undo any time with `shao hook uninstall`.")
	fmt.Println()
}

func hookInstall(targets []hookTarget, dryRun bool) {
	for _, t := range targets {
		existing, err := os.ReadFile(t.path)
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "shao: skipping %s: %v\n", t.path, err)
			continue
		}
		if strings.Contains(string(existing), hookBegin) {
			fmt.Printf("already installed in %s\n", t.path)
			continue
		}

		block := fmt.Sprintf("\n%s\n# Added by `shao hook install`. Remove with `shao hook uninstall`.\n%s\n%s\n",
			hookBegin, t.snippet, hookEnd)

		if dryRun {
			fmt.Printf("would append to %s:\n%s\n", t.path, block)
			continue
		}
		if len(existing) > 0 {
			backup := t.path + ".shao-backup-" + time.Now().Format("20060102150405")
			if err := os.WriteFile(backup, existing, 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "shao: could not back up %s: %v\n", t.path, err)
				continue
			}
			fmt.Printf("backed up %s -> %s\n", t.path, backup)
		}
		if err := appendFile(t.path, block); err != nil {
			fmt.Fprintf(os.Stderr, "shao: could not modify %s: %v\n", t.path, err)
			continue
		}
		fmt.Printf("installed in %s\n", t.path)
	}
	if !dryRun {
		fmt.Println()
		fmt.Println("New terminals will record themselves from now on. Terminals already open are")
		fmt.Println("unaffected; run `shao shell` in one to start recording it.")
	}
}

func hookUninstall(targets []hookTarget, dryRun bool) {
	for _, t := range targets {
		data, err := os.ReadFile(t.path)
		if err != nil {
			continue
		}
		text := string(data)
		if !strings.Contains(text, hookBegin) {
			continue
		}
		cleaned, ok := removeBlock(text)
		if !ok {
			fmt.Fprintf(os.Stderr, "shao: %s contains the start marker but not the end marker; leaving it alone\n", t.path)
			continue
		}
		if dryRun {
			fmt.Printf("would remove the shao block from %s\n", t.path)
			continue
		}
		if err := os.WriteFile(t.path, []byte(cleaned), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "shao: could not modify %s: %v\n", t.path, err)
			continue
		}
		fmt.Printf("removed from %s\n", t.path)
	}
}

func hookStatus(targets []hookTarget) {
	for _, t := range targets {
		data, err := os.ReadFile(t.path)
		switch {
		case err != nil:
			fmt.Printf("%-60s (no such file)\n", t.path)
		case strings.Contains(string(data), hookBegin):
			fmt.Printf("%-60s installed\n", t.path)
		default:
			fmt.Printf("%-60s not installed\n", t.path)
		}
	}
}

// removeBlock cuts out the marked region, including the markers.
func removeBlock(text string) (string, bool) {
	start := strings.Index(text, hookBegin)
	if start < 0 {
		return text, false
	}
	end := strings.Index(text[start:], hookEnd)
	if end < 0 {
		return text, false
	}
	end = start + end + len(hookEnd)
	// Remove exactly what was added and nothing more: the leading newline,
	// the block, and the newline after the end marker. Being precise here is
	// what makes uninstall byte-identical to the file before install, rather
	// than leaving a stray newline behind on a file that had none.
	if end < len(text) && text[end] == '\n' {
		end++
	}
	if start > 0 && text[start-1] == '\n' {
		start--
	}
	return text[:start] + text[end:], true
}

func appendFile(path, content string) error {
	if err := config.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(content)
	return err
}
