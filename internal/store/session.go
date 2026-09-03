package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tmon/internal/config"
)

const (
	metaFile  = "meta.json"
	indexFile = "index.jsonl"
	// stopFile is how `tmon end` asks a recorder in another process to shut
	// down. A file rather than a signal, because a recorder has to finish
	// cleanly -- flush both rings, close the open command block, mark the
	// session ended -- and killing the process would lose exactly the last
	// output anyone would want to ask about. It is also the one mechanism
	// that behaves identically on Windows and POSIX.
	stopFile = "stop"
)

// RequestStop asks the recorder owning this session to finish and exit.
func RequestStop(sessionDir string) error {
	return config.WritePrivateFile(filepath.Join(sessionDir, stopFile), []byte("stop\n"))
}

// StopRequested reports whether a stop has been asked for. The recorder polls
// this.
func StopRequested(sessionDir string) bool {
	_, err := os.Stat(filepath.Join(sessionDir, stopFile))
	return err == nil
}

// Meta is the durable description of a recorded session. It is written when
// recording starts and rewritten when it ends, so a reader can tell a live
// session from a finished one even if the recorder was killed.
type Meta struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Host attributes a session to a configured host, so "what went wrong on
	// web-prod-1" can pick the right session out of several open windows.
	Host      string     `json:"host,omitempty"`
	Shell     string     `json:"shell"`
	Argv      []string   `json:"argv,omitempty"`
	PID       int        `json:"pid"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Cols      int        `json:"cols"`
	Rows      int        `json:"rows"`
	Platform  string     `json:"platform"`

	// CWD and Title are the latest observed values, kept here so listing many
	// sessions does not have to scan every index. They are what makes one
	// recorded terminal tellable from another: without them a list of ten
	// sessions is ten identical rows differing only by timestamp.
	CWD   string `json:"cwd,omitempty"`
	Title string `json:"title,omitempty"`

	MaxBytes       int64 `json:"max_bytes"`
	CookedMaxBytes int64 `json:"cooked_max_bytes"`
	SegmentBytes   int64 `json:"segment_bytes"`

	RedactionOn bool `json:"redaction_on"`
	// ShellIntegration records which command-boundary hook was installed
	// (bash, zsh, pwsh) or "none" when the shell was not recognised. When it
	// is "none" the index stays empty and only byte- and line-oriented reads
	// are available, which is a precision downgrade, not a loss of capture.
	ShellIntegration string `json:"shell_integration"`
	RecorderVersion  string `json:"recorder_version"`
}

// Live reports whether the session is still being recorded.
func (m Meta) Live() bool { return m.EndedAt == nil }

// IndexKind distinguishes the entry types in index.jsonl.
type IndexKind string

const (
	// IndexCmd is one shell command with its output range and exit status.
	IndexCmd IndexKind = "cmd"
	// IndexResize records a terminal size change, so a later replay can use
	// the width that was actually in effect.
	IndexResize IndexKind = "resize"
	// IndexNote records recorder-side events worth explaining to a reader.
	IndexNote IndexKind = "note"
	// IndexCWD records a working-directory change. The history matters as
	// well as the latest value: it answers which directory a given command
	// actually ran in.
	IndexCWD IndexKind = "cwd"
	// IndexTitle records a terminal title change.
	IndexTitle IndexKind = "title"
)

// IndexEntry is one line of index.jsonl.
type IndexEntry struct {
	Kind IndexKind `json:"kind"`
	Seq  int       `json:"seq,omitempty"`

	Cmd       string     `json:"cmd,omitempty"`
	StartedAt time.Time  `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// ExitCode is nil when the command never finished, which is exactly the
	// case for the command that was running when a session died.
	ExitCode *int `json:"exit_code,omitempty"`

	RawOff    uint64 `json:"raw_off"`
	RawLen    uint64 `json:"raw_len"`
	CookedOff uint64 `json:"cooked_off"`
	CookedLen uint64 `json:"cooked_len"`

	// RemoteHost is set when the command sequence entered another machine or
	// container (ssh, docker exec), so remote output can be attributed.
	RemoteHost string `json:"remote_host,omitempty"`

	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
	Note  string `json:"note,omitempty"`
	CWD   string `json:"cwd,omitempty"`
	Title string `json:"title,omitempty"`
}

// Failed reports whether this entry is a command that finished non-zero.
func (e IndexEntry) Failed() bool {
	return e.Kind == IndexCmd && e.ExitCode != nil && *e.ExitCode != 0
}

// Session is the writer side of a recorded session.
type Session struct {
	dir  string
	meta Meta

	raw    *ringWriter
	cooked *ringWriter

	mu    sync.Mutex
	index *os.File
	seq   int
	// open is the command currently running, held in memory until its exit
	// status is known, at which point one complete line is appended.
	open *IndexEntry
}

// NewSessionID builds a lexically sortable, collision-resistant id.
func NewSessionID(now time.Time, pid int) string {
	return fmt.Sprintf("%s-%d", now.Format("20060102T150405.000"), pid)
}

// Create starts a new session under root/sessions/<id>.
func Create(sessionsDir string, meta Meta, cookedMax int64) (*Session, error) {
	if meta.ID == "" {
		return nil, fmt.Errorf("store: session id is required")
	}
	dir := filepath.Join(sessionsDir, meta.ID)
	if err := config.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}

	raw, err := newRingWriter(StreamDir(dir, StreamRaw), meta.SegmentBytes, meta.MaxBytes)
	if err != nil {
		return nil, err
	}
	if cookedMax <= 0 {
		cookedMax = meta.CookedMaxBytes
	}
	cooked, err := newRingWriter(StreamDir(dir, StreamCooked), meta.SegmentBytes, cookedMax)
	if err != nil {
		raw.Close()
		return nil, err
	}
	idx, err := config.CreatePrivateFile(filepath.Join(dir, indexFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		raw.Close()
		cooked.Close()
		return nil, err
	}

	s := &Session{dir: dir, meta: meta, raw: raw, cooked: cooked, index: idx}
	if err := s.writeMeta(); err != nil {
		s.Close(nil)
		return nil, err
	}
	return s, nil
}

func (s *Session) Dir() string          { return s.dir }
func (s *Session) Meta() Meta           { return s.meta }
func (s *Session) RawOffset() uint64    { return s.raw.Offset() }
func (s *Session) CookedOffset() uint64 { return s.cooked.Offset() }

func (s *Session) WriteRaw(p []byte) (int, error)    { return s.raw.Write(p) }
func (s *Session) WriteCooked(p []byte) (int, error) { return s.cooked.Write(p) }

// Flush pushes both rings and the index to the filesystem. Readers live in
// other processes, so this call is what makes recent output visible to them.
func (s *Session) Flush() error {
	err := s.raw.Flush()
	if cerr := s.cooked.Flush(); err == nil {
		err = cerr
	}
	s.mu.Lock()
	if s.index != nil {
		if serr := s.index.Sync(); err == nil && serr != nil {
			err = serr
		}
	}
	s.mu.Unlock()
	return err
}

// Buffered reports how many bytes are held in memory across both rings, used
// by the recorder to skip needless flushes.
func (s *Session) Buffered() int { return s.raw.Buffered() + s.cooked.Buffered() }

func (s *Session) writeMeta() error {
	data, err := json.MarshalIndent(s.meta, "", "  ")
	if err != nil {
		return err
	}
	return config.WritePrivateFile(filepath.Join(s.dir, metaFile), append(data, '\n'))
}

// appendIndex writes one entry as a single line. Line-per-record means a
// truncated final write costs one entry, never the whole index.
func (s *Session) appendIndex(e IndexEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index == nil {
		return nil
	}
	_, err = s.index.Write(append(data, '\n'))
	return err
}

// AppendIndex records a standalone entry (resize, note).
func (s *Session) AppendIndex(e IndexEntry) error { return s.appendIndex(e) }

// Note records a recorder-side event.
func (s *Session) Note(msg string) error {
	return s.appendIndex(IndexEntry{Kind: IndexNote, StartedAt: time.Now(), Note: msg})
}

// Resize records a terminal size change and updates the session metadata.
func (s *Session) Resize(cols, rows int) error {
	s.meta.Cols, s.meta.Rows = cols, rows
	if err := s.appendIndex(IndexEntry{
		Kind: IndexResize, StartedAt: time.Now(), Cols: cols, Rows: rows,
	}); err != nil {
		return err
	}
	return s.writeMeta()
}

// CommandStarted opens a command block at the current stream offsets.
//
// A block is held in memory until its exit status arrives, because the exit
// code is the whole point of the index: it turns "show me recent output" into
// "show me the command that actually failed".
func (s *Session) CommandStarted(cmd string, remoteHost string) {
	s.mu.Lock()
	prev := s.open
	s.seq++
	seq := s.seq
	s.open = &IndexEntry{
		Kind:       IndexCmd,
		Seq:        seq,
		Cmd:        cmd,
		StartedAt:  time.Now(),
		RawOff:     s.raw.Offset(),
		CookedOff:  s.cooked.Offset(),
		RemoteHost: remoteHost,
	}
	s.mu.Unlock()

	// A second start without an intervening finish means we missed the
	// previous command's exit marker; record it with an unknown status
	// rather than dropping it.
	if prev != nil {
		s.closeEntry(prev, nil)
	}
}

// CommandFinished closes the open command block with its exit status.
func (s *Session) CommandFinished(exitCode int) {
	s.mu.Lock()
	e := s.open
	s.open = nil
	s.mu.Unlock()
	if e == nil {
		return
	}
	s.closeEntry(e, &exitCode)
}

func (s *Session) closeEntry(e *IndexEntry, exitCode *int) {
	now := time.Now()
	e.EndedAt = &now
	e.ExitCode = exitCode
	e.RawLen = s.raw.Offset() - e.RawOff
	e.CookedLen = s.cooked.Offset() - e.CookedOff
	_ = s.appendIndex(*e)
}

// RecordCommandSpan writes a complete command block whose start offsets were
// captured earlier.
//
// Shells differ in what they can tell us. bash and zsh signal the exact
// instant a command begins, so CommandStarted/CommandFinished bracket it.
// PowerShell has no such hook and only reports a finished command from its
// prompt, so the recorder reconstructs the block from the previous prompt
// position and calls this instead. The resulting entry is the same shape
// either way; only its precision differs.
func (s *Session) RecordCommandSpan(cmd, remoteHost string, rawOff, cookedOff uint64, startedAt time.Time, exitCode *int) {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()

	now := time.Now()
	e := IndexEntry{
		Kind:       IndexCmd,
		Seq:        seq,
		Cmd:        cmd,
		StartedAt:  startedAt,
		EndedAt:    &now,
		ExitCode:   exitCode,
		RawOff:     rawOff,
		RawLen:     s.raw.Offset() - rawOff,
		CookedOff:  cookedOff,
		CookedLen:  s.cooked.Offset() - cookedOff,
		RemoteHost: remoteHost,
	}
	_ = s.appendIndex(e)
}

// SetCWD records a working-directory change, if it is actually a change.
//
// Shells report the directory at every prompt, so most reports are the same
// value again; writing those would fill the index with noise and rewrite
// meta.json on every keystroke-to-prompt cycle.
func (s *Session) SetCWD(dir string) {
	if dir == "" {
		return
	}
	s.mu.Lock()
	same := s.meta.CWD == dir
	if !same {
		s.meta.CWD = dir
	}
	s.mu.Unlock()
	if same {
		return
	}
	_ = s.appendIndex(IndexEntry{Kind: IndexCWD, StartedAt: time.Now(), CWD: dir})
	_ = s.writeMeta()
}

// SetTitle records a terminal title change, if it is actually a change.
func (s *Session) SetTitle(title string) {
	if title == "" {
		return
	}
	s.mu.Lock()
	same := s.meta.Title == title
	if !same {
		s.meta.Title = title
	}
	s.mu.Unlock()
	if same {
		return
	}
	_ = s.appendIndex(IndexEntry{Kind: IndexTitle, StartedAt: time.Now(), Title: title})
	_ = s.writeMeta()
}

// SetRemoteHost tags the open command block, used when the recorder notices
// the session has entered a remote host or container.
func (s *Session) SetRemoteHost(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open != nil {
		s.open.RemoteHost = host
	}
}

// Close finalises the session. An in-flight command is flushed with a nil
// exit code so a session killed mid-command still shows what was running.
func (s *Session) Close(exitCode *int) error {
	s.mu.Lock()
	open := s.open
	s.open = nil
	s.mu.Unlock()
	if open != nil {
		s.closeEntry(open, nil)
	}

	now := time.Now()
	s.meta.EndedAt = &now
	s.meta.ExitCode = exitCode

	err := s.raw.Close()
	if cerr := s.cooked.Close(); err == nil {
		err = cerr
	}
	s.mu.Lock()
	if s.index != nil {
		if cerr := s.index.Close(); err == nil {
			err = cerr
		}
		s.index = nil
	}
	s.mu.Unlock()
	if merr := s.writeMeta(); err == nil {
		err = merr
	}
	return err
}

// ReadMeta loads a session's metadata from disk.
func ReadMeta(sessionDir string) (Meta, error) {
	data, err := os.ReadFile(filepath.Join(sessionDir, metaFile))
	if err != nil {
		return Meta{}, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, fmt.Errorf("store: parse %s: %w", filepath.Join(sessionDir, metaFile), err)
	}
	return m, nil
}

// ReadIndex loads index.jsonl. A malformed trailing line is skipped rather
// than failing the read: the recorder may be mid-write in another process.
func ReadIndex(sessionDir string) ([]IndexEntry, error) {
	data, err := os.ReadFile(filepath.Join(sessionDir, indexFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []IndexEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e IndexEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
