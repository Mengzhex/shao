package mcpsrv

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mengzhex/shao/internal/config"
	"github.com/Mengzhex/shao/internal/store"
)

// These tests drive the server the way a real client does — a scripted
// JSON-RPC conversation over a pipe — so scenario A can be verified end to
// end without a terminal, a shell, or an AI.

// buildFixture writes a recorded session to disk containing a successful
// command followed by a failing one, with a lot of unrelated noise in between.
// The noise is the point: it is what makes "show me the last 500 lines" a bad
// answer and an indexed lookup a good one.
func buildFixture(t *testing.T, root string) string {
	t.Helper()
	sessionsDir := filepath.Join(root, "sessions")

	meta := store.Meta{
		ID:               "20260827T120000.000-1234",
		Label:            "deploy",
		Host:             "web-prod-1",
		Shell:            "/bin/bash",
		PID:              1234,
		StartedAt:        time.Now().Add(-5 * time.Minute),
		Cols:             120,
		Rows:             30,
		Platform:         "linux",
		MaxBytes:         1 << 20,
		CookedMaxBytes:   1 << 20,
		SegmentBytes:     4096,
		RedactionOn:      true,
		ShellIntegration: "bash",
		RecorderVersion:  "1",
	}
	sess, err := store.Create(sessionsDir, meta, 1<<20)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	write := func(s string) {
		if _, err := sess.WriteRaw([]byte(s)); err != nil {
			t.Fatalf("write raw: %v", err)
		}
		if _, err := sess.WriteCooked([]byte(s)); err != nil {
			t.Fatalf("write cooked: %v", err)
		}
	}

	sess.CommandStarted("git pull", "")
	write("Already up to date.\n")
	sess.CommandFinished(0)

	// Noise: a build that succeeds but says a great deal while doing it.
	sess.CommandStarted("make build", "")
	for i := 0; i < 3000; i++ {
		write("compiling module unrelated/pkg" + itoa(i) + "\n")
	}
	sess.CommandFinished(0)

	sess.CommandStarted("docker compose up -d", "")
	write("Creating network app_default\n")
	write("Error response from daemon: driver failed programming external " +
		"connectivity on endpoint app_web: Bind for 0.0.0.0:8080 failed: port is already allocated\n")
	sess.CommandFinished(1)

	// A command that ran on another machine, so host attribution is exercised.
	sess.CommandStarted("ssh web-prod-2 systemctl status app", "web-prod-2")
	write("Unit app.service could not be found.\n")
	sess.CommandFinished(4)

	if err := sess.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	zero := 0
	if err := sess.Close(&zero); err != nil {
		t.Fatalf("close: %v", err)
	}
	return meta.ID
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	id := buildFixture(t, root)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, id
}

// converse runs a scripted exchange through ServeStdio and returns the
// decoded replies, which is exactly the path a real client exercises.
func converse(t *testing.T, srv *Server, requests ...string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	var out strings.Builder
	if err := srv.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatalf("ServeStdio: %v", err)
	}

	var replies []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("reply is not JSON: %q: %v", line, err)
		}
		replies = append(replies, m)
	}
	return replies
}

// toolText pulls the text out of a tools/call reply and reports isError.
func toolText(t *testing.T, reply map[string]any) (string, bool) {
	t.Helper()
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("reply has no result: %+v", reply)
	}
	isErr, _ := result["isError"].(bool)
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("reply has no content: %+v", result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text, isErr
}

func TestHandshake(t *testing.T) {
	srv, _ := newTestServer(t)
	replies := converse(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	)
	// The notification must not draw a reply.
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want 2 (a notification must not be answered): %+v", len(replies), replies)
	}
	result := replies[0]["result"].(map[string]any)
	if got := result["protocolVersion"]; got != "2025-06-18" {
		t.Errorf("negotiated version %v, want the one the client asked for", got)
	}
	caps := result["capabilities"].(map[string]any)
	if _, hasTools := caps["tools"]; !hasTools {
		t.Error("server did not advertise tools")
	}
	// Nothing about shao is push-driven, so it must not claim otherwise.
	for _, forbidden := range []string{"resources", "prompts", "logging", "completions"} {
		if _, present := caps[forbidden]; present {
			t.Errorf("server advertised %q; shao exposes tools only", forbidden)
		}
	}
}

// TestEveryToolIsReadOnly is a structural guard: the read-only promise should
// fail loudly if a tool is ever added that writes.
func TestEveryToolIsReadOnly(t *testing.T) {
	srv, _ := newTestServer(t)
	replies := converse(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	result := replies[0]["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) < 8 {
		t.Fatalf("got %d tools, want at least 8", len(tools))
	}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		ann, ok := tool["annotations"].(map[string]any)
		if !ok {
			t.Errorf("tool %s has no annotations", name)
			continue
		}
		if ann["readOnlyHint"] != true {
			t.Errorf("tool %s is not marked readOnlyHint", name)
		}
		if ann["destructiveHint"] != false {
			t.Errorf("tool %s is not marked non-destructive", name)
		}
		if _, hasSchema := tool["inputSchema"]; !hasSchema {
			t.Errorf("tool %s has no input schema", name)
		}
	}
}

// TestGetLastErrorIsPrecise is scenario A. The failing command is buried under
// 3000 lines of successful build output, so a tail-based answer would return
// none of it.
func TestGetLastErrorIsPrecise(t *testing.T) {
	srv, _ := newTestServer(t)
	replies := converse(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_last_error","arguments":{"session":"label:deploy"}}}`,
	)
	text, isErr := toolText(t, replies[0])
	if isErr {
		t.Fatalf("get_last_error reported a failure: %s", text)
	}

	// The most recent failure is the ssh one, exit 4.
	if !strings.Contains(text, "ssh web-prod-2 systemctl status app") {
		t.Errorf("did not identify the most recent failed command:\n%s", text)
	}
	if !strings.Contains(text, "exit    : 4") {
		t.Errorf("exit code missing or wrong:\n%s", text)
	}
	if !strings.Contains(text, "Unit app.service could not be found") {
		t.Errorf("the failing command's own output is missing:\n%s", text)
	}
	if !strings.Contains(text, "web-prod-2") {
		t.Errorf("remote host attribution missing:\n%s", text)
	}
	// Output from the noisy build must not leak into this command's block.
	if strings.Contains(text, "compiling module unrelated") {
		t.Errorf("output from an unrelated command leaked into the block:\n%s", text)
	}
}

// skip walks back to an earlier failure, which is how a follow-up question
// like "and the one before that" is answered.
func TestGetLastErrorSkip(t *testing.T) {
	srv, _ := newTestServer(t)
	replies := converse(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_last_error","arguments":{"skip":1}}}`,
	)
	text, isErr := toolText(t, replies[0])
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !strings.Contains(text, "docker compose up -d") {
		t.Errorf("skip=1 did not reach the previous failure:\n%s", text)
	}
	if !strings.Contains(text, "port is already allocated") {
		t.Errorf("previous failure output missing:\n%s", text)
	}
}

func TestGetCommandHistoryOnlyFailed(t *testing.T) {
	srv, _ := newTestServer(t)
	replies := converse(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_command_history","arguments":{"only_failed":true}}}`,
	)
	text, _ := toolText(t, replies[0])
	if strings.Contains(text, "git pull") || strings.Contains(text, "make build") {
		t.Errorf("only_failed returned successful commands:\n%s", text)
	}
	if !strings.Contains(text, "docker compose up -d") || !strings.Contains(text, "ssh web-prod-2") {
		t.Errorf("only_failed missed a failure:\n%s", text)
	}
}

func TestSearchOutput(t *testing.T) {
	srv, _ := newTestServer(t)
	replies := converse(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_output","arguments":{"pattern":"(?i)port is already allocated","context_lines":1}}}`,
	)
	text, isErr := toolText(t, replies[0])
	if isErr {
		t.Fatalf("search failed: %s", text)
	}
	if !strings.Contains(text, "port is already allocated") {
		t.Errorf("search did not find a line that is present:\n%s", text)
	}
	if !strings.Contains(text, "Creating network app_default") {
		t.Errorf("context line missing:\n%s", text)
	}
}

func TestListSessions(t *testing.T) {
	srv, id := newTestServer(t)
	replies := converse(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions"}}`)
	text, _ := toolText(t, replies[0])
	for _, want := range []string{id, "label=deploy", "host=web-prod-1"} {
		if !strings.Contains(text, want) {
			t.Errorf("list_sessions output missing %q:\n%s", want, text)
		}
	}
}

// An unconfigured host must be refused, not attempted.
func TestHostQueryRefusedWhenUnverified(t *testing.T) {
	root := t.TempDir()
	buildFixture(t, root)
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Hosts = []config.HostConfig{{
		Name:        "web-prod-1",
		Address:     "10.0.0.11",
		User:        "aiview",
		Enforcement: config.EnforceForcedCommandRoot,
		// VerifiedAt deliberately empty.
	}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	replies := converse(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_host_env","arguments":{"host":"web-prod-1"}}}`,
	)
	text, isErr := toolText(t, replies[0])
	if !isErr {
		t.Errorf("querying an unverified host should be refused, got:\n%s", text)
	}
	if !strings.Contains(text, "verify") {
		t.Errorf("refusal should say verification is missing:\n%s", text)
	}
}

func TestDisabledHostIsRefused(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Hosts = []config.HostConfig{{
		Name:        "legacy-box",
		Address:     "10.0.0.99",
		User:        "root",
		Enforcement: config.EnforceDisabled,
		VerifiedAt:  "2026-08-27T00:00:00Z", // even a stale verification must not help
	}}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	replies := converse(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_deploy_context","arguments":{"host":"legacy-box"}}}`,
	)
	text, isErr := toolText(t, replies[0])
	if !isErr {
		t.Errorf("a disabled host must not be queryable, got:\n%s", text)
	}
}

func TestUnknownToolAndBadJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	replies := converse(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_command","arguments":{"cmd":"rm -rf /"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"no/such/method"}`,
		`not json at all`,
	)
	if len(replies) != 3 {
		t.Fatalf("got %d replies, want 3", len(replies))
	}
	for i, r := range replies {
		if _, hasErr := r["error"]; !hasErr {
			t.Errorf("reply %d should be a protocol error: %+v", i, r)
		}
	}
}

// Redaction must apply on the way out, covering sessions recorded before a
// rule existed.
func TestReadTimeRedaction(t *testing.T) {
	root := t.TempDir()
	sessionsDir := filepath.Join(root, "sessions")
	meta := store.Meta{
		ID: "secretsession", StartedAt: time.Now(), Shell: "/bin/bash",
		MaxBytes: 1 << 20, CookedMaxBytes: 1 << 20, SegmentBytes: 4096,
		ShellIntegration: "bash",
	}
	sess, err := store.Create(sessionsDir, meta, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Written unredacted, as a session recorded before the rule existed would be.
	sess.WriteCooked([]byte("export DEPLOY_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123\n"))
	sess.Flush()
	sess.Close(nil)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	replies := converse(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_recent_output","arguments":{}}}`,
	)
	text, _ := toolText(t, replies[0])
	if strings.Contains(text, "ghp_abcdefghijklmnopqrstuvwxyz0123") {
		t.Errorf("a token stored in the clear was returned to the client:\n%s", text)
	}
	if !strings.Contains(text, "REDACTED") {
		t.Errorf("expected a redaction marker:\n%s", text)
	}
}

// buildIdentifiedSession writes a session with the identity a listing needs.
func buildIdentifiedSession(t *testing.T, root, id, cwd, title string, endedAgo time.Duration) {
	t.Helper()
	meta := store.Meta{
		ID: id, StartedAt: time.Now().Add(-endedAgo - time.Minute), Shell: "pwsh",
		PID: os.Getpid(), MaxBytes: 1 << 20, CookedMaxBytes: 1 << 20, SegmentBytes: 4096,
		ShellIntegration: "pwsh",
	}
	sess, err := store.Create(filepath.Join(root, "sessions"), meta, 1<<20)
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	sess.SetCWD(cwd)
	sess.SetTitle(title)
	sess.CommandStarted("npm run build", "")
	sess.WriteCooked([]byte("building\n"))
	sess.CommandFinished(0)
	sess.Flush()
	sess.Close(nil)

	// Back-date the end so age-based filtering can be exercised.
	path := filepath.Join(sess.Dir(), "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rawMeta map[string]any
	if err := json.Unmarshal(data, &rawMeta); err != nil {
		t.Fatal(err)
	}
	rawMeta["ended_at"] = time.Now().Add(-endedAgo).Format(time.RFC3339Nano)
	out, _ := json.Marshal(rawMeta)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The reason this feature exists: with several terminals recorded, a listing
// has to say which is which. Identity is what makes that possible.
func TestListSessionsCarriesIdentity(t *testing.T) {
	root := t.TempDir()
	buildIdentifiedSession(t, root, "s-look", `C:\proj\looklook`, "build window", time.Minute)
	buildIdentifiedSession(t, root, "s-mon", `C:\proj\terminal_monitoring`, "editor window", time.Minute)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	replies := converse(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions"}}`)
	text, isErr := toolText(t, replies[0])
	if isErr {
		t.Fatalf("list_sessions failed: %s", text)
	}

	for _, want := range []string{
		`cwd=C:\proj\looklook`,
		`cwd=C:\proj\terminal_monitoring`,
		`"build window"`,
		`"editor window"`,
		"last=npm run build -> ok",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("listing is missing %q:\n%s", want, text)
		}
	}
}

// A listing that quietly drops rows reads as "this is everything". The same
// reason Span reports Truncated rather than just returning less.
func TestListSessionsReportsWhatItOmitted(t *testing.T) {
	root := t.TempDir()
	buildIdentifiedSession(t, root, "recent", "/recent", "t", time.Minute)
	buildIdentifiedSession(t, root, "old-1", "/old1", "t", 30*time.Hour)
	buildIdentifiedSession(t, root, "old-2", "/old2", "t", 40*time.Hour)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	replies := converse(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions"}}`)
	text, _ := toolText(t, replies[0])

	if !strings.Contains(text, "/recent") {
		t.Errorf("the recent session is missing:\n%s", text)
	}
	if strings.Contains(text, "/old1") {
		t.Errorf("a 30-hour-old session was listed by default:\n%s", text)
	}
	if !strings.Contains(text, "2 older finished session(s) are not listed") {
		t.Errorf("omitted sessions were hidden without saying so:\n%s", text)
	}

	// all=true must reach them.
	replies = converse(t, srv,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_sessions","arguments":{"all":true}}}`)
	text, _ = toolText(t, replies[0])
	for _, want := range []string{"/recent", "/old1", "/old2"} {
		if !strings.Contains(text, want) {
			t.Errorf("all=true did not include %s:\n%s", want, text)
		}
	}
}

// cwd and title go into metadata, which the stream redactor never sees. A
// checkout directory named after a token would otherwise be stored and served
// in the clear. This gap was already found once on command text.
func TestIdentityIsRedacted(t *testing.T) {
	root := t.TempDir()
	buildIdentifiedSession(t, root,
		"secret", `C:\builds\deploy-ghp_abcdefghijklmnopqrstuvwxyz0123`,
		"token=ghp_abcdefghijklmnopqrstuvwxyz0123", time.Minute)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	replies := converse(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sessions"}}`)
	text, _ := toolText(t, replies[0])
	if strings.Contains(text, "ghp_abcdefghijklmnopqrstuvwxyz0123") {
		t.Errorf("a token in the cwd or title was served in the clear:\n%s", text)
	}
	if !strings.Contains(text, "REDACTED") {
		t.Errorf("expected a redaction marker:\n%s", text)
	}
}

// Naming a terminal by its directory is how a person refers to it.
func TestToolsAcceptCWDSelector(t *testing.T) {
	root := t.TempDir()
	buildIdentifiedSession(t, root, "s-look", `C:\proj\looklook`, "a", time.Minute)
	buildIdentifiedSession(t, root, "s-mon", `C:\proj\terminal_monitoring`, "b", time.Minute)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	replies := converse(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_recent_output","arguments":{"session":"cwd:looklook"}}}`)
	text, isErr := toolText(t, replies[0])
	if isErr {
		t.Fatalf("cwd selector failed: %s", text)
	}
	if !strings.Contains(text, "s-look") {
		t.Errorf("cwd:looklook resolved to the wrong session:\n%s", text)
	}
	if !strings.Contains(text, `cwd=C:\proj\looklook`) && !strings.Contains(text, `cwd C:\proj\looklook`) {
		t.Errorf("the header does not say which terminal this is:\n%s", text)
	}
}
