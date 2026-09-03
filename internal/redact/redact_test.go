package redact

import (
	"strings"
	"testing"

	"tmon/internal/config"
)

func newTestRedactor(t *testing.T, patterns ...string) *Redactor {
	t.Helper()
	r, err := New(config.RedactConfig{Builtin: true, Patterns: patterns})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestBuiltinShapes(t *testing.T) {
	r := newTestRedactor(t)
	cases := []struct {
		name   string
		in     string
		leaked string
		kept   string // something that should survive, so output stays readable
	}{
		{"env assignment", "export API_TOKEN=abc123secret", "abc123secret", "API_TOKEN"},
		{"quoted assignment", `DB_PASSWORD="hunter2"`, "hunter2", "DB_PASSWORD"},
		{"aws key id", "using AKIAIOSFODNN7EXAMPLE now", "AKIAIOSFODNN7EXAMPLE", "using"},
		{"github token", "ghp_abcdefghijklmnopqrstuvwxyz0123", "ghp_abcdefghijklmnopqrstuvwxyz0123", ""},
		{"authorization header", "Authorization: Bearer eyJabc.def.ghi", "eyJabc.def.ghi", "Authorization"},
		{"url credentials", "git clone https://bob:s3cr3t@example.com/r.git", "s3cr3t", "example.com"},
		{"mysql flag", "mysql -u root -phunter2 mydb", "hunter2", "mysql"},
		{"curl basic auth", "curl -u admin:letmein https://x/", "letmein", "curl"},
		{"sshpass", "sshpass -p mypassword ssh host", "mypassword", "ssh host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(r.All([]byte(tc.in)))
			if strings.Contains(got, tc.leaked) {
				t.Errorf("secret survived redaction: %q -> %q", tc.in, got)
			}
			if !strings.Contains(got, "REDACTED") {
				t.Errorf("nothing was redacted in %q -> %q", tc.in, got)
			}
			if tc.kept != "" && !strings.Contains(got, tc.kept) {
				t.Errorf("redaction destroyed context %q: %q -> %q", tc.kept, tc.in, got)
			}
		})
	}
}

func TestPrivateKeyBlock(t *testing.T) {
	r := newTestRedactor(t)
	in := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"
	got := string(r.All([]byte(in)))
	if strings.Contains(got, "b3BlbnNzaC1rZXktdjEA") {
		t.Errorf("key body survived: %q", got)
	}
}

// A pty delivers arbitrary chunks, so a secret is routinely split in the
// middle. Redaction has to hold back the trailing partial line rather than
// matching each chunk in isolation.
func TestSecretSplitAcrossChunks(t *testing.T) {
	r := newTestRedactor(t)

	var out strings.Builder
	out.Write(r.Process([]byte("export API_TO")))
	out.Write(r.Process([]byte("KEN=supersecret")))
	out.Write(r.Process([]byte("value\n")))
	out.Write(r.Flush())

	got := out.String()
	if strings.Contains(got, "supersecretvalue") {
		t.Errorf("a secret split across chunks was not redacted: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("expected a redaction marker in %q", got)
	}
}

// The hold-back must not stall output forever when a program never emits a
// newline, which is exactly what a progress bar does.
func TestLongLineWithoutNewlineIsReleased(t *testing.T) {
	r := newTestRedactor(t)
	big := strings.Repeat("=", maxHold*2)
	out := r.Process([]byte(big))
	if len(out) == 0 {
		t.Fatal("a line longer than the hold-back budget was not released")
	}
	if r.Pending() > maxHold {
		t.Errorf("hold-back grew to %d bytes, above the %d cap", r.Pending(), maxHold)
	}
}

func TestFlushReleasesHeldBytes(t *testing.T) {
	r := newTestRedactor(t)
	if got := r.Process([]byte("no newline yet")); len(got) != 0 {
		t.Errorf("partial line released early: %q", got)
	}
	if got := string(r.Flush()); got != "no newline yet" {
		t.Errorf("flush returned %q, want the held bytes", got)
	}
	if r.Pending() != 0 {
		t.Error("flush left bytes behind")
	}
}

func TestCustomPattern(t *testing.T) {
	r := newTestRedactor(t, `INTERNAL-[0-9]{6}`)
	got := string(r.All([]byte("ticket INTERNAL-123456 filed")))
	if strings.Contains(got, "INTERNAL-123456") {
		t.Errorf("custom pattern did not apply: %q", got)
	}
	if !strings.Contains(got, "ticket") || !strings.Contains(got, "filed") {
		t.Errorf("custom pattern removed too much: %q", got)
	}
}

func TestOrdinaryOutputIsUntouched(t *testing.T) {
	r := newTestRedactor(t)
	in := "go build ./... && ./server --port 8080\nlistening on :8080\n"
	if got := string(r.All([]byte(in))); got != in {
		t.Errorf("ordinary output was altered:\n got %q\nwant %q", got, in)
	}
}

func TestDisabledRedactorIsPassThrough(t *testing.T) {
	r, err := New(config.RedactConfig{Builtin: false})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Disabled() {
		t.Fatal("a redactor with no rules should report itself disabled")
	}
	in := []byte("API_TOKEN=abc123secret")
	if got := string(r.Process(in)); got != string(in) {
		t.Errorf("disabled redactor changed or withheld output: %q", got)
	}
}
