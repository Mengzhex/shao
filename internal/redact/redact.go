// Package redact strips credential-shaped text out of captured terminal
// output.
//
// Two things are worth being honest about up front. First, redaction is a
// filter, not a guarantee: it catches shapes it recognises, and a secret in
// an unusual shape gets through. That is why it is paired with owner-only
// file permissions rather than relied on alone. Second, interactive password
// prompts turn echo off, so those characters never reach the pty output in
// the first place; what this package is actually defending against is the
// credential you typed on a command line, or one a script printed.
//
// Redaction runs twice: once on the way to disk, and again on the way out to
// the AI when config.Redact.ReadTimeSecondPass is on, so a rule added after
// a session was recorded still applies to it.
package redact

import (
	"bytes"
	"fmt"
	"regexp"

	"tmon/internal/config"
)

// maxHold bounds the hold-back buffer. A pty delivers arbitrary chunks, so a
// pattern can straddle a chunk boundary; the fix is to hold back the trailing
// partial line until its newline arrives. Progress bars redraw with carriage
// returns and never emit a newline, so an unbounded hold would grow forever
// and stall output. 8 KiB is far longer than any credential-bearing line.
const maxHold = 8 << 10

type rule struct {
	name string
	re   *regexp.Regexp
	// repl is a Go regexp expansion template. Rules keep the identifying
	// prefix (the variable name, the flag) and replace only the value, so
	// the output still reads as a command instead of turning into mush.
	repl string
}

// builtinRules covers the shapes that actually show up in a terminal.
var builtinRules = []rule{
	{
		name: "assignment",
		re:   regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API[_-]?KEY|APIKEY|ACCESS[_-]?KEY|PRIVATE[_-]?KEY|CREDENTIAL)[A-Z0-9_]*)(\s*[:=]\s*)("[^"\n]*"|'[^'\n]*'|[^\s;&|]+)`),
		repl: `${1}${2}«REDACTED:assignment»`,
	},
	{
		name: "authorization-header",
		re:   regexp.MustCompile(`(?i)\b(authorization\s*:\s*)(bearer|basic|token)(\s+)([A-Za-z0-9._~+/=-]{8,})`),
		repl: `${1}${2}${3}«REDACTED:authorization»`,
	},
	{
		name: "aws-access-key-id",
		re:   regexp.MustCompile(`\b((?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA)[0-9A-Z]{16})\b`),
		repl: `«REDACTED:aws-key-id»`,
	},
	{
		name: "github-token",
		re:   regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,})\b`),
		repl: `«REDACTED:github-token»`,
	},
	{
		name: "anthropic-key",
		re:   regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{16,}`),
		repl: `«REDACTED:anthropic-key»`,
	},
	{
		name: "openai-key",
		re:   regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`),
		repl: `«REDACTED:api-key»`,
	},
	{
		name: "slack-token",
		re:   regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`),
		repl: `«REDACTED:slack-token»`,
	},
	{
		name: "jwt",
		re:   regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
		repl: `«REDACTED:jwt»`,
	},
	{
		name: "private-key-block",
		re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
		repl: `«REDACTED:private-key»`,
	},
	{
		// mysql -pHunter2 / redis-cli -a hunter2: the password sits in the
		// argument list, which is exactly the case echo-off cannot help with.
		name: "db-client-password-flag",
		re:   regexp.MustCompile(`(?i)\b(mysql|mysqldump|mariadb|mysqladmin)\b([^\n]{0,120}?)(\s-p|\s--password=)([^\s;&|]+)`),
		repl: `${1}${2}${3}«REDACTED:db-password»`,
	},
	{
		name: "redis-auth-flag",
		re:   regexp.MustCompile(`(?i)\b(redis-cli)\b([^\n]{0,120}?)(\s-a\s+|\s--pass\s+)([^\s;&|]+)`),
		repl: `${1}${2}${3}«REDACTED:redis-password»`,
	},
	{
		name: "curl-basic-auth",
		re:   regexp.MustCompile(`(?i)\b(curl)\b([^\n]{0,120}?)(\s-u\s+|\s--user\s+)([^\s;&|]+)`),
		repl: `${1}${2}${3}«REDACTED:basic-auth»`,
	},
	{
		// Credentials embedded in a URL, e.g. https://user:pw@host/.
		name: "url-userinfo",
		re:   regexp.MustCompile(`\b([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+):([^\s/@]+)@`),
		repl: `${1}:«REDACTED:url-password»@`,
	},
	{
		name: "sshpass",
		re:   regexp.MustCompile(`(?i)\b(sshpass\s+-p\s*)([^\s;&|]+)`),
		repl: `${1}«REDACTED:ssh-password»`,
	},
}

// Redactor applies redaction rules to a stream. It is not safe for
// concurrent use; the recorder owns one per stream.
type Redactor struct {
	rules []rule
	hold  []byte
}

// New builds a Redactor from configuration. User patterns are appended after
// the built-ins so a site-specific shape can be caught without editing code.
// A user pattern replaces its whole match, since tmon cannot know which of
// its groups is the secret.
func New(cfg config.RedactConfig) (*Redactor, error) {
	r := &Redactor{}
	if cfg.Builtin {
		r.rules = append(r.rules, builtinRules...)
	}
	for i, p := range cfg.Patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redact: pattern %d (%q): %w", i, p, err)
		}
		r.rules = append(r.rules, rule{
			name: fmt.Sprintf("custom-%d", i),
			re:   re,
			repl: "«REDACTED:custom»",
		})
	}
	return r, nil
}

// Disabled reports whether this Redactor would change anything.
func (r *Redactor) Disabled() bool { return r == nil || len(r.rules) == 0 }

// All applies every rule to a complete buffer. Use it when the whole text is
// in hand: the read-time second pass, and tests.
func (r *Redactor) All(p []byte) []byte {
	if r.Disabled() || len(p) == 0 {
		return p
	}
	out := p
	for _, ru := range r.rules {
		out = ru.re.ReplaceAll(out, []byte(ru.repl))
	}
	return out
}

// Process redacts as much of the stream as can be judged safely and returns
// it, holding back a trailing partial line so a credential split across two
// pty chunks is still matched.
//
// The held-back bytes are released by the next Process call that sees a
// newline, or by Flush. The recorder calls Flush when output goes idle, so a
// partial line is never stuck for longer than the idle flush interval.
func (r *Redactor) Process(p []byte) []byte {
	if r.Disabled() {
		return p
	}
	if len(r.hold) > 0 {
		p = append(r.hold, p...)
		r.hold = nil
	}

	cut := bytes.LastIndexByte(p, '\n')
	if cut < 0 {
		if len(p) <= maxHold {
			// No line boundary yet: hold everything and wait.
			r.hold = append(r.hold[:0], p...)
			return nil
		}
		// A very long line with no newline in sight, most likely a redrawing
		// progress bar. Release all but the tail so output keeps flowing.
		cut = len(p) - maxHold - 1
	}

	complete, partial := p[:cut+1], p[cut+1:]
	if len(partial) > maxHold {
		complete, partial = p, nil
	}
	if len(partial) > 0 {
		r.hold = append(r.hold[:0], partial...)
	}
	return r.All(complete)
}

// Flush releases any held-back bytes, redacted on their own.
//
// Releasing early is a deliberate trade: a secret split across an idle
// boundary can escape the filter, but holding output back indefinitely would
// mean the AI could not see the very last line before a prompt, which is
// usually the error message being asked about.
func (r *Redactor) Flush() []byte {
	if r.Disabled() || len(r.hold) == 0 {
		return nil
	}
	out := r.All(r.hold)
	r.hold = nil
	return out
}

// Pending reports how many bytes are held back.
func (r *Redactor) Pending() int {
	if r == nil {
		return 0
	}
	return len(r.hold)
}
