package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tmon/internal/config"
	"tmon/internal/cook"
	"tmon/internal/redact"
	"tmon/internal/shellint"
	"tmon/internal/store"
)

// newTestRecorder builds the write side of a recorder without a pty, which is
// all the identity handling needs.
func newTestRecorder(t *testing.T) (*recorder, *store.Session, string) {
	t.Helper()
	root := t.TempDir()

	red, err := redact.New(config.RedactConfig{Builtin: true})
	if err != nil {
		t.Fatal(err)
	}
	meta := store.Meta{
		ID: "s", StartedAt: time.Now(), Shell: "pwsh", PID: os.Getpid(),
		MaxBytes: 1 << 20, CookedMaxBytes: 1 << 20, SegmentBytes: 4096,
	}
	sess, err := store.Create(filepath.Join(root, "sessions"), meta, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close(nil) })

	r := &recorder{
		sess:      sess,
		red:       red,
		cooker:    cook.New(),
		parser:    shellint.NewParser(),
		stdout:    io_Discard{},
		lastWrite: time.Now(),
	}
	return r, sess, sess.Dir()
}

// io_Discard avoids pulling in io just for one field.
type io_Discard struct{}

func (io_Discard) Write(p []byte) (int, error) { return len(p), nil }

// The working directory and title go into metadata, which the stream redactor
// never sees. A checkout directory named after a token would be stored in the
// clear, and the only thing standing between that and an AI would be the
// read-time pass -- one layer where there should be two.
//
// This is the same shape as a bug already found on command text: the read-time
// test passed while the capture side was leaking to disk.
func TestIdentityIsRedactedBeforeItReachesDisk(t *testing.T) {
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123"

	r, _, dir := newTestRecorder(t)

	r.handleEvent(shellint.Event{
		Kind: shellint.EvCWD,
		CWD:  `C:\builds\deploy-` + token,
	})
	r.handleEvent(shellint.Event{
		Kind:  shellint.EvTitle,
		Title: "session token=" + token,
	})

	// meta.json holds the latest values.
	metaBytes, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metaBytes), token) {
		t.Errorf("a token reached meta.json in the clear:\n%s", metaBytes)
	}
	if !strings.Contains(string(metaBytes), "REDACTED") {
		t.Errorf("meta.json shows no redaction happened:\n%s", metaBytes)
	}

	// index.jsonl holds the change history and must be scrubbed too.
	idxBytes, err := os.ReadFile(filepath.Join(dir, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(idxBytes), token) {
		t.Errorf("a token reached index.jsonl in the clear:\n%s", idxBytes)
	}

	// And nothing anywhere under the session directory.
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(data), token) {
			t.Errorf("a token was stored in the clear in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Ordinary paths must survive untouched: redaction that mangled every
// directory would destroy the identity it is protecting.
func TestOrdinaryIdentityIsNotMangled(t *testing.T) {
	r, _, dir := newTestRecorder(t)

	const cwd = `C:\HUIXIN\cursor_local_project\looklook`
	const title = "Administrator: C:\\Program Files\\PowerShell\\7\\pwsh.exe"
	r.handleEvent(shellint.Event{Kind: shellint.EvCWD, CWD: cwd})
	r.handleEvent(shellint.Event{Kind: shellint.EvTitle, Title: title})

	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		CWD   string `json:"cwd"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.CWD != cwd {
		t.Errorf("cwd was altered: got %q, want %q", meta.CWD, cwd)
	}
	if meta.Title != title {
		t.Errorf("title was altered: got %q, want %q", meta.Title, title)
	}
}
