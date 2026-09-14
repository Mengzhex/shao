// Package shellint installs command-boundary markers into the shell shao
// launches, and parses them back out of the recorded stream.
//
// Without this, "why did that fail" can only be answered with "here are the
// last N lines", which on a busy terminal is mostly unrelated noise. With it,
// shao knows where each command started, what it was, and what it exited
// with, so the question becomes a lookup: the last command whose exit code
// was not zero, and exactly its output.
//
// The markers are OSC escape sequences. Two families are used:
//
//	OSC 133 ; A            ST   prompt drawn
//	OSC 133 ; C            ST   command output begins
//	OSC 133 ; D ; <exit>   ST   command finished
//	OSC 7331 ; cmd ; <b64> ST   the command line, base64 encoded
//	OSC 7331 ; cwd ; <b64> ST   the working directory, base64 encoded
//
// Two more are read but never written, because the terminal supplies them:
//
//	OSC 0 / OSC 2          ST   the window title
//	OSC 7 ; file://host/p  ST   the working directory, where a terminal emits it
//
// The working directory is what makes one recorded terminal distinguishable
// from another, and a pseudoconsole on Windows never emits OSC 7, so shao
// reports it from the prompt itself.
//
// OSC 133 is the widely implemented semantic-prompt convention, so emitting
// it also gives well-behaved terminals working prompt navigation. OSC 7331 is
// shao's own; terminals ignore OSC codes they do not know. The command line is
// base64 encoded so that a semicolon, a newline or an escape character inside
// a command cannot break the framing.
//
// Because shao starts the shell itself, none of this requires editing the
// user's rc files. Their real rc is sourced first and the hooks are appended
// afterwards, so their prompt and aliases keep working.
//
// When the shell is not one of the three recognised families the session is
// still recorded in full; only the command index is empty. That is a
// precision downgrade, not a loss of capture.
package shellint

import (
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Mengzhex/shao/internal/config"
)

// Kind identifies which integration snippet applies to a shell.
type Kind string

const (
	KindBash Kind = "bash"
	KindZsh  Kind = "zsh"
	KindPwsh Kind = "pwsh"
	KindNone Kind = "none"
)

// Detect maps a shell executable path to an integration kind.
func Detect(shellPath string) Kind {
	base := strings.ToLower(filepath.Base(shellPath))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "bash", "sh":
		// A plain sh is very often bash in disguise; the snippet degrades
		// harmlessly if it is not, because it only sets variables and a trap.
		return KindBash
	case "zsh":
		return KindZsh
	case "pwsh", "powershell":
		return KindPwsh
	default:
		return KindNone
	}
}

// Launch describes how to start a shell with integration installed.
type Launch struct {
	Args []string // arguments after the executable
	Env  []string // additional KEY=VALUE entries
	// TempDir holds generated rc files and must be removed when the session
	// ends.
	TempDir string
	Kind    Kind
}

// Prepare writes whatever rc scaffolding the shell needs and returns how to
// launch it.
//
// The caller must remove Launch.TempDir when the session ends.
func Prepare(kind Kind, root string) (Launch, error) {
	l := Launch{Kind: kind}
	if kind == KindNone {
		return l, nil
	}

	dir, err := os.MkdirTemp(rootTempDir(root), "shellint-")
	if err != nil {
		return l, err
	}
	l.TempDir = dir

	switch kind {
	case KindBash:
		rc := filepath.Join(dir, "bashrc")
		if err := config.WritePrivateFile(rc, []byte(bashSnippet)); err != nil {
			return l, err
		}
		// --rcfile applies to interactive non-login shells, which is what a
		// recorded session is.
		l.Args = []string{"--rcfile", rc, "-i"}

	case KindZsh:
		// zsh has no --rcfile, so the documented approach is to point ZDOTDIR
		// at a directory holding our .zshrc. That .zshrc restores ZDOTDIR
		// before doing anything else, so nested shells behave normally.
		rc := filepath.Join(dir, ".zshrc")
		if err := config.WritePrivateFile(rc, []byte(zshSnippet)); err != nil {
			return l, err
		}
		orig := os.Getenv("ZDOTDIR")
		if orig == "" {
			orig, _ = os.UserHomeDir()
		}
		l.Env = []string{"ZDOTDIR=" + dir, "SHAO_ORIG_ZDOTDIR=" + orig}
		l.Args = []string{"-i"}

	case KindPwsh:
		ps := filepath.Join(dir, "shellint.ps1")
		if err := config.WritePrivateFile(ps, []byte(pwshSnippet)); err != nil {
			return l, err
		}
		// The user profile is deliberately still loaded; only the prompt is
		// wrapped afterwards.
		l.Args = []string{"-NoExit", "-ExecutionPolicy", "Bypass", "-File", ps}
	}
	return l, nil
}

func rootTempDir(root string) string {
	dir := filepath.Join(root, "tmp")
	if err := config.EnsurePrivateDir(dir); err != nil {
		return ""
	}
	return dir
}

// bashSnippet installs preexec and precmd hooks.
//
// The DEBUG trap fires for every simple command, including ones run inside
// PROMPT_COMMAND itself, so it is gated on a flag that only the prompt sets.
// Without that gate a single interactive command would emit several starts.
const bashSnippet = `# shao shell integration (generated; not a file you need to keep)
if [ -f /etc/bash.bashrc ]; then . /etc/bash.bashrc; fi
if [ -f "$HOME/.bashrc" ]; then . "$HOME/.bashrc"; fi

__shao_b64() {
  if command -v base64 >/dev/null 2>&1; then
    printf '%s' "$1" | base64 | tr -d '\n'
  else
    printf 'RAW'
  fi
}

__shao_emit_cmd() {
  local enc
  enc=$(__shao_b64 "$1")
  if [ "$enc" = "RAW" ]; then
    # No base64 available: send the text with framing characters removed.
    printf '\033]7331;cmdraw;%s\007' "$(printf '%s' "$1" | tr -d '\033\007\n\r')"
  else
    printf '\033]7331;cmd;%s\007' "$enc"
  fi
}

__shao_at_prompt=1

__shao_preexec() {
  [ -n "$COMP_LINE" ] && return          # tab completion, not a command
  [ "$__shao_at_prompt" != "1" ] && return
  __shao_at_prompt=0
  __shao_emit_cmd "$BASH_COMMAND"
  printf '\033]133;C\007'
}

__shao_emit_cwd() {
  local enc
  enc=$(__shao_b64 "$PWD")
  if [ "$enc" = "RAW" ]; then
    printf '\033]7331;cwdraw;%s\007' "$(printf '%s' "$PWD" | tr -d '\033\007\n\r')"
  else
    printf '\033]7331;cwd;%s\007' "$enc"
  fi
}

__shao_precmd() {
  local __shao_ec=$?
  if [ "$__shao_at_prompt" != "1" ]; then
    printf '\033]133;D;%s\007' "$__shao_ec"
  fi
  __shao_at_prompt=1
  # Reported every prompt rather than once at startup: cd is exactly what
  # distinguishes one terminal from another, so a stale value is worse than
  # none.
  __shao_emit_cwd
  printf '\033]133;A\007'
  return $__shao_ec
}

trap '__shao_preexec' DEBUG
# Ours runs first so it observes the real exit status.
PROMPT_COMMAND="__shao_precmd${PROMPT_COMMAND:+; $PROMPT_COMMAND}"
printf '\033]7331;ver;1\007'
`

// zshSnippet uses zsh's native preexec and precmd hooks, which are precise
// and need none of the DEBUG-trap gymnastics bash requires.
const zshSnippet = `# shao shell integration (generated; not a file you need to keep)
ZDOTDIR="${SHAO_ORIG_ZDOTDIR:-$HOME}"
export ZDOTDIR
[ -f "$ZDOTDIR/.zshrc" ] && source "$ZDOTDIR/.zshrc"

__shao_emit_cmd() {
  local enc
  if command -v base64 >/dev/null 2>&1; then
    enc=$(printf '%s' "$1" | base64 | tr -d '\n')
    printf '\033]7331;cmd;%s\007' "$enc"
  else
    printf '\033]7331;cmdraw;%s\007' "$(printf '%s' "$1" | tr -d '\033\007\n\r')"
  fi
}

__shao_preexec() {
  __shao_emit_cmd "$1"
  printf '\033]133;C\007'
}

__shao_precmd() {
  local ec=$?
  printf '\033]133;D;%s\007' "$ec"
  if command -v base64 >/dev/null 2>&1; then
    printf '\033]7331;cwd;%s\007' "$(printf '%s' "$PWD" | base64 | tr -d '\n')"
  else
    printf '\033]7331;cwdraw;%s\007' "$(printf '%s' "$PWD" | tr -d '\033\007\n\r')"
  fi
  printf '\033]133;A\007'
}

autoload -Uz add-zsh-hook 2>/dev/null
if command -v add-zsh-hook >/dev/null 2>&1; then
  add-zsh-hook preexec __shao_preexec
  add-zsh-hook precmd __shao_precmd
else
  preexec_functions+=(__shao_preexec)
  precmd_functions+=(__shao_precmd)
fi
printf '\033]7331;ver;1\007'
`

// pwshSnippet wraps the prompt function.
//
// PowerShell has no preexec hook, so unlike bash and zsh there is no marker
// at the instant a command starts. The prompt emits the finished command and
// its status instead, and the recorder treats the previous prompt as the
// start of that command's output. The practical difference is that a
// PowerShell command block also contains the echoed command line, which is
// usually welcome rather than a problem.
const pwshSnippet = `# shao shell integration (generated; not a file you need to keep)
$global:__shaoLastHistoryId = -1

function global:__ShaoEmit([string]$s) {
  [Console]::Out.Write($s)
}

function global:__ShaoPrompt([bool]$ok, $lec) {
  # Only a failed command carries a meaningful exit code. $LASTEXITCODE is
  # left over from the last native executable and is not reset by cmdlets, so
  # trusting it when $? is true reports a long-finished failure against a
  # command that just succeeded.
  if ($ok) {
    $ec = 0
  } elseif ($null -ne $lec -and $lec -ne 0) {
    $ec = $lec
  } else {
    $ec = 1
  }

  $esc = [char]27
  $bel = [char]7
  $h = Get-History -Count 1 -ErrorAction SilentlyContinue
  if ($null -ne $h -and $h.Id -ne $global:__shaoLastHistoryId) {
    $global:__shaoLastHistoryId = $h.Id
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($h.CommandLine)
    $b64 = [Convert]::ToBase64String($bytes)
    __ShaoEmit "$esc]7331;cmd;$b64$bel"
    __ShaoEmit "$esc]133;D;$ec$bel"
  }
  $cwd = (Get-Location).Path
  $cwdB64 = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($cwd))
  __ShaoEmit "$esc]7331;cwd;$cwdB64$bel"
  __ShaoEmit "$esc]133;A$bel"
}

# Wrap whatever prompt the user profile defined rather than replacing it.
$global:__shaoInnerPrompt = $function:prompt
function global:prompt {
  # These two must be the very first statements: any other statement, an
  # assignment included, overwrites $? with its own success, and the result of
  # the command the user actually ran is then gone for good.
  $ok = $?
  $lec = $global:LASTEXITCODE
  __ShaoPrompt $ok $lec
  if ($null -ne $global:__shaoInnerPrompt) {
    & $global:__shaoInnerPrompt
  } else {
    "PS $($executionContext.SessionState.Path.CurrentLocation)$('>' * ($nestedPromptLevel + 1)) "
  }
}
__ShaoEmit "$([char]27)]7331;ver;1$([char]7)"
`

// DecodeCommand turns a marker payload back into command text.
func DecodeCommand(form, payload string) string {
	switch form {
	case "cmd":
		return decodeMarker(false, payload)
	case "cmdraw":
		return decodeMarker(true, payload)
	default:
		return ""
	}
}

// decodeMarker reads a marker value that is base64 unless the shell had no
// base64 available and fell back to sending it literally.
func decodeMarker(raw bool, payload string) string {
	if raw {
		return strings.TrimSpace(payload)
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// parseFileURL turns the file:// URL an OSC 7 sequence carries into a path.
//
// The hostname is discarded: a remote host's path is not a directory on this
// machine, and treating it as one would mislabel the session. On Windows the
// path arrives as /C:/Users/... and the leading slash has to go.
func parseFileURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "file://") {
		return ""
	}
	rest := strings.TrimPrefix(raw, "file://")
	// Everything up to the next slash is the hostname.
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[i:]
	} else {
		return ""
	}
	decoded, err := url.PathUnescape(rest)
	if err != nil {
		decoded = rest
	}
	// A Windows path is /C:/... once the host is stripped.
	if len(decoded) > 2 && decoded[0] == '/' && decoded[2] == ':' {
		decoded = decoded[1:]
	}
	return strings.TrimSpace(decoded)
}

// remoteEntryPrefixes are commands that move the session onto another machine
// or into a container. Output after one of these is not from the local host,
// and tagging it lets a question naming a host find the right stretch of the
// buffer.
var remoteEntryPrefixes = []struct {
	cmd     string
	hostArg bool // whether the first non-flag argument names the target
}{
	{"ssh", true},
	{"mosh", true},
	{"docker exec", false},
	{"docker attach", false},
	{"kubectl exec", false},
	{"podman exec", false},
}

// DetectRemoteHost reports the host or container a command hands the session
// over to, or "" for an ordinary local command.
//
// This is a heuristic on the command line, so it is used only to tag output,
// never to decide anything about access.
func DetectRemoteHost(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	lower := strings.ToLower(cmd)
	for _, e := range remoteEntryPrefixes {
		if !strings.HasPrefix(lower, e.cmd+" ") {
			continue
		}
		if !e.hostArg {
			return firstNonFlag(fields[2:])
		}
		target := firstNonFlag(fields[1:])
		// Strip a user@ prefix and any :port suffix.
		if at := strings.LastIndex(target, "@"); at >= 0 {
			target = target[at+1:]
		}
		return target
	}
	return ""
}

// firstNonFlag returns the first argument that is not a flag, skipping the
// value that follows a flag taking one.
func firstNonFlag(args []string) string {
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.HasPrefix(a, "-") {
			// ssh flags that consume a following value.
			switch strings.TrimPrefix(a, "-") {
			case "p", "i", "o", "l", "F", "L", "R", "D", "b", "c", "e", "m", "w":
				skipNext = true
			}
			continue
		}
		return a
	}
	return ""
}

// formatExit parses the exit status carried by a D marker.
func formatExit(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return n, true
}
