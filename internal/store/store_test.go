package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The central claim of this package is that whatever is still on disk is one
// unbroken run of the stream. These tests exercise that claim directly rather
// than testing the pieces that implement it.

func newTestRing(t *testing.T, segmentBytes, maxBytes int64) (*ringWriter, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := newRingWriter(dir, segmentBytes, maxBytes)
	if err != nil {
		t.Fatalf("newRingWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w, dir
}

func TestRingRoundTrip(t *testing.T) {
	w, dir := newTestRing(t, 1024, 8192)

	want := []byte("hello terminal\n")
	if _, err := w.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	res, err := ReadTail(dir, 4096)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(res.Data) != string(want) {
		t.Errorf("got %q, want %q", res.Data, want)
	}
	if res.Span.Truncated || res.Span.GapDetected {
		t.Errorf("unexpected truncation/gap on a ring that never filled: %+v", res.Span)
	}
}

// TestNoLossUnderEviction is the requirement that motivated capturing a byte
// stream instead of sampling the screen: write far more than the ring holds,
// then check that what survives is a contiguous run of the sequence with no
// numbers missing in the middle. Old lines falling off the back is expected;
// a hole is not.
func TestNoLossUnderEviction(t *testing.T) {
	const (
		segmentBytes = 4096
		maxBytes     = 16384
		lines        = 20000
	)
	w, dir := newTestRing(t, segmentBytes, maxBytes)

	for i := 0; i < lines; i++ {
		if _, err := w.Write([]byte(fmt.Sprintf("line %d\n", i))); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	res, err := ReadTail(dir, maxBytes*2)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.Span.GapDetected {
		t.Fatal("gap detected: the surviving buffer is not contiguous")
	}
	if !res.Span.Truncated {
		t.Error("expected the ring to report that it discarded older output")
	}

	// Drop the first line: the byte budget can cut it mid-way.
	got := strings.Split(strings.TrimRight(string(res.Data), "\n"), "\n")
	if len(got) < 3 {
		t.Fatalf("expected several surviving lines, got %d", len(got))
	}
	got = got[1:]

	prev := -1
	for _, line := range got {
		var n int
		if _, err := fmt.Sscanf(line, "line %d", &n); err != nil {
			t.Fatalf("unparsable surviving line %q: %v", line, err)
		}
		if prev >= 0 && n != prev+1 {
			t.Fatalf("sequence jumped from %d to %d: output was lost mid-stream", prev, n)
		}
		prev = n
	}
	if prev != lines-1 {
		t.Errorf("last surviving line is %d, want %d: the newest output should always be present", prev, lines-1)
	}
}

// TestRingStaysUnderCap checks eviction actually bounds the ring, allowing for
// the deliberate overshoot of one segment.
func TestRingStaysUnderCap(t *testing.T) {
	const (
		segmentBytes = 1024
		maxBytes     = 8192
	)
	w, dir := newTestRing(t, segmentBytes, maxBytes)

	payload := strings.Repeat("x", 512)
	for i := 0; i < 500; i++ {
		if _, err := w.Write([]byte(payload)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	w.Flush()

	span, err := ReadSpan(dir)
	if err != nil {
		t.Fatalf("span: %v", err)
	}
	if span.Bytes() > maxBytes+segmentBytes {
		t.Errorf("ring holds %d bytes, cap is %d (+1 segment tolerance)", span.Bytes(), maxBytes)
	}
	if span.Bytes() < maxBytes/2 {
		t.Errorf("ring holds only %d bytes, far below its %d cap: eviction is too aggressive", span.Bytes(), maxBytes)
	}
}

// TestOffsetsSurviveReopen guards against a crashed recorder restarting the
// offset numbering, which would make two different bytes claim the same
// position in the stream.
func TestOffsetsSurviveReopen(t *testing.T) {
	dir := t.TempDir()

	w1, err := newRingWriter(dir, 1024, 8192)
	if err != nil {
		t.Fatal(err)
	}
	w1.Write([]byte("first\n"))
	w1.Close()
	firstEnd := w1.Offset()

	w2, err := newRingWriter(dir, 1024, 8192)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if got := w2.Offset(); got != firstEnd {
		t.Errorf("reopened ring resumed at offset %d, want %d", got, firstEnd)
	}
	w2.Write([]byte("second\n"))
	w2.Flush()

	res, err := ReadTail(dir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Data); got != "first\nsecond\n" {
		t.Errorf("got %q, want %q", got, "first\nsecond\n")
	}
	if res.Span.GapDetected {
		t.Error("reopening the ring introduced a gap")
	}
}

func TestReadRangeClampsToRetained(t *testing.T) {
	w, dir := newTestRing(t, 1024, 4096)
	for i := 0; i < 2000; i++ {
		w.Write([]byte(fmt.Sprintf("%04d\n", i)))
	}
	w.Flush()

	// Asking for long-evicted bytes is not an error; it returns what is left
	// and says the ring had truncated.
	res, err := ReadRange(dir, 0, 100)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !res.Span.Truncated {
		t.Error("expected truncated=true when reading from before the oldest retained byte")
	}
}

func TestReadTailLines(t *testing.T) {
	w, dir := newTestRing(t, 4096, 65536)
	for i := 0; i < 100; i++ {
		w.Write([]byte("line " + strconv.Itoa(i) + "\n"))
	}
	w.Flush()

	res, err := ReadTailLines(dir, 10, 65536)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 10 {
		t.Fatalf("got %d lines, want 10", len(res.Lines))
	}
	if res.Lines[9] != "line 99" {
		t.Errorf("last line is %q, want %q", res.Lines[9], "line 99")
	}
	if res.Lines[0] != "line 90" {
		t.Errorf("first line is %q, want %q", res.Lines[0], "line 90")
	}
}

// TestSearchIsBackward checks that the most recent match comes first, which is
// what makes "the last error" cheap to find.
func TestSearchIsBackward(t *testing.T) {
	w, dir := newTestRing(t, 4096, 1<<20)
	w.Write([]byte("ok\nERROR first\nok\nok\nERROR second\nok\n"))
	w.Flush()

	res, err := Search(dir, regexp.MustCompile(`ERROR`), 10, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(res.Matches))
	}
	if !strings.Contains(res.Matches[0].Line, "second") {
		t.Errorf("first match is %q, want the most recent one", res.Matches[0].Line)
	}
	if len(res.Matches[0].Before) != 1 || len(res.Matches[0].After) != 1 {
		t.Errorf("expected one line of context each side, got before=%d after=%d",
			len(res.Matches[0].Before), len(res.Matches[0].After))
	}
	if !res.ReachedOldest {
		t.Error("a small buffer should have been searched in full")
	}
}

// A recorder that is killed never marks its session ended, so metadata alone
// reports it as still recording forever. That is worse than cosmetic: a dead
// session would shadow a real one when resolving "latest", and would tell the
// AI a terminal is open that the user closed. The recorder process is the
// authority, so liveness is checked against it.
func TestStaleSessionDetectedWhenRecorderIsGone(t *testing.T) {
	sessionsDir := t.TempDir()

	newSession := func(id string, pid int) string {
		meta := Meta{
			ID: id, StartedAt: time.Now(), Shell: "/bin/bash", PID: pid,
			MaxBytes: 1 << 20, CookedMaxBytes: 1 << 20, SegmentBytes: 4096,
		}
		sess, err := Create(sessionsDir, meta, 1<<20)
		if err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		sess.WriteCooked([]byte("output\n"))
		sess.Flush()
		// Close to release the file handles, then strip ended_at from the
		// metadata. That is precisely the on-disk state a killed recorder
		// leaves behind, and unlike simply not closing it, the temporary
		// directory can still be cleaned up afterwards.
		sess.Close(nil)
		stripEndedAt(t, sess.Dir())
		return sess.Dir()
	}

	// This process is certainly alive; a PID that cannot exist certainly is not.
	aliveDir := newSession("alive-session", os.Getpid())
	deadDir := newSession("dead-session", 0x7FFFFFF0)

	alive, err := Describe(aliveDir)
	if err != nil {
		t.Fatal(err)
	}
	if alive.Stale {
		t.Error("session whose recorder is running was reported stale")
	}
	if !alive.Live() {
		t.Error("session whose recorder is running should be live")
	}

	dead, err := Describe(deadDir)
	if err != nil {
		t.Fatal(err)
	}
	if !dead.Stale {
		t.Fatal("session whose recorder is gone was not reported stale")
	}
	if dead.Live() {
		t.Error("a stale session must not report itself live")
	}

	// "latest" must not resolve to the dead one even though its metadata says
	// it is still open.
	got, err := Resolve(sessionsDir, "latest")
	if err != nil {
		t.Fatalf("resolve latest: %v", err)
	}
	if got.Meta.ID != "alive-session" {
		t.Errorf("latest resolved to %q, want the session that is actually recording", got.Meta.ID)
	}
}

// Finalize closes the record of a stale session without inventing an end time
// in the future of when it actually stopped.
func TestFinalizeStaleSession(t *testing.T) {
	sessionsDir := t.TempDir()
	meta := Meta{
		ID: "s", StartedAt: time.Now().Add(-time.Hour), Shell: "/bin/bash", PID: 0x7FFFFFF0,
		MaxBytes: 1 << 20, CookedMaxBytes: 1 << 20, SegmentBytes: 4096,
	}
	sess, err := Create(sessionsDir, meta, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sess.WriteCooked([]byte("x\n"))
	sess.Flush()
	sess.Close(nil)
	stripEndedAt(t, sess.Dir())

	info, err := Describe(sess.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Stale {
		t.Fatal("expected a stale session")
	}

	stoppedAt := info.LastActivity
	if err := Finalize(sess.Dir(), stoppedAt); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	after, err := Describe(sess.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if after.Meta.EndedAt == nil {
		t.Fatal("finalize did not mark the session ended")
	}
	if after.Live() {
		t.Error("finalized session still reports live")
	}
	if after.Meta.EndedAt.After(time.Now()) {
		t.Error("end time is in the future")
	}

	// Finalizing again must not move the end time.
	first := *after.Meta.EndedAt
	if err := Finalize(sess.Dir(), time.Now()); err != nil {
		t.Fatal(err)
	}
	again, _ := Describe(sess.Dir())
	if !again.Meta.EndedAt.Equal(first) {
		t.Errorf("second finalize moved the end time from %v to %v", first, *again.Meta.EndedAt)
	}
}

// stripEndedAt removes the ended_at field from a session's metadata,
// reproducing what a recorder that was killed leaves on disk.
func stripEndedAt(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, metaFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse meta: %v", err)
	}
	delete(raw, "ended_at")
	delete(raw, "exit_code")
	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write meta: %v", err)
	}
}

// newIdentifiedSession writes a session carrying the identity that tells one
// recorded terminal from another.
func newIdentifiedSession(t *testing.T, sessionsDir, id, cwd, title string, endedAgo time.Duration) string {
	t.Helper()
	meta := Meta{
		ID: id, StartedAt: time.Now().Add(-endedAgo - time.Minute), Shell: "pwsh",
		PID: os.Getpid(), MaxBytes: 1 << 20, CookedMaxBytes: 1 << 20, SegmentBytes: 4096,
	}
	sess, err := Create(sessionsDir, meta, 1<<20)
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	sess.WriteCooked([]byte("output\n"))
	sess.SetCWD(cwd)
	sess.SetTitle(title)
	sess.Flush()
	if endedAgo >= 0 {
		sess.Close(nil)
		// Close stamps "now"; back-date it so age-based filtering can be tested.
		backdateEnd(t, sess.Dir(), time.Now().Add(-endedAgo))
	}
	return sess.Dir()
}

func backdateEnd(t *testing.T, dir string, when time.Time) {
	t.Helper()
	path := filepath.Join(dir, metaFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["ended_at"] = when.Format(time.RFC3339Nano)
	out, _ := json.Marshal(raw)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Identity is the whole point: several recorded terminals are otherwise ten
// rows differing only by timestamp.
func TestSessionIdentityIsRecorded(t *testing.T) {
	dir := t.TempDir()
	sdir := newIdentifiedSession(t, dir, "s1", `C:\HUIXIN\cursor_local_project\looklook`, "build window", 0)

	info, err := Describe(sdir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Meta.CWD != `C:\HUIXIN\cursor_local_project\looklook` {
		t.Errorf("cwd not recorded: %q", info.Meta.CWD)
	}
	if info.Meta.Title != "build window" {
		t.Errorf("title not recorded: %q", info.Meta.Title)
	}

	// The change history is kept too, so "which directory did that command run
	// in" is answerable, not just "where is it now".
	idx, err := ReadIndex(sdir)
	if err != nil {
		t.Fatal(err)
	}
	var sawCWD, sawTitle bool
	for _, e := range idx {
		switch e.Kind {
		case IndexCWD:
			sawCWD = true
		case IndexTitle:
			sawTitle = true
		}
	}
	if !sawCWD || !sawTitle {
		t.Errorf("index missing identity history: cwd=%v title=%v", sawCWD, sawTitle)
	}
}

// Shells report the directory at every prompt, so the same value arrives over
// and over. Recording each one would fill the index with noise and rewrite
// metadata on every prompt.
func TestRepeatedIdentityIsNotRecordedTwice(t *testing.T) {
	dir := t.TempDir()
	meta := Meta{
		ID: "s", StartedAt: time.Now(), Shell: "pwsh", PID: os.Getpid(),
		MaxBytes: 1 << 20, CookedMaxBytes: 1 << 20, SegmentBytes: 4096,
	}
	sess, err := Create(dir, meta, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		sess.SetCWD("/home/me/src")
		sess.SetTitle("same title")
	}
	sess.SetCWD("/home/me/other")
	sess.Flush()
	sess.Close(nil)

	idx, err := ReadIndex(sess.Dir())
	if err != nil {
		t.Fatal(err)
	}
	var cwdEntries, titleEntries int
	for _, e := range idx {
		switch e.Kind {
		case IndexCWD:
			cwdEntries++
		case IndexTitle:
			titleEntries++
		}
	}
	if cwdEntries != 2 {
		t.Errorf("got %d cwd entries, want 2 (one per actual change)", cwdEntries)
	}
	if titleEntries != 1 {
		t.Errorf("got %d title entries, want 1", titleEntries)
	}
}

// A directory substring is how a person refers to a terminal.
func TestResolveByCWD(t *testing.T) {
	dir := t.TempDir()
	newIdentifiedSession(t, dir, "s-look", `C:\HUIXIN\cursor_local_project\looklook`, "a", 0)
	newIdentifiedSession(t, dir, "s-mon", `C:\HUIXIN\cursor_local_project\terminal_monitoring`, "b", 0)

	got, err := Resolve(dir, "cwd:looklook")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Meta.ID != "s-look" {
		t.Errorf("resolved to %q, want s-look", got.Meta.ID)
	}

	// Case-insensitive, since nobody types a path's exact case.
	if got, err := Resolve(dir, "cwd:LOOKLOOK"); err != nil || got.Meta.ID != "s-look" {
		t.Errorf("case-insensitive match failed: %v %v", got.Meta.ID, err)
	}

	// A substring matching both must be an error listing them, not a guess:
	// answering from the wrong terminal is worse than asking which was meant.
	if _, err := Resolve(dir, "cwd:cursor_local_project"); err == nil {
		t.Error("an ambiguous cwd resolved silently instead of erroring")
	}
	if _, err := Resolve(dir, "cwd:nowhere"); err == nil {
		t.Error("a cwd matching nothing resolved instead of erroring")
	}
}

// When several sessions match, one still recording is the obvious intent, and
// that rule predates the cwd selector. But two live matches must be an error:
// answering from the wrong terminal is worse than asking which was meant.
func TestResolveAmbiguityRules(t *testing.T) {
	dir := t.TempDir()

	live := func(id, cwd string) *Session {
		meta := Meta{
			ID: id, StartedAt: time.Now(), Shell: "pwsh", PID: os.Getpid(),
			MaxBytes: 1 << 20, CookedMaxBytes: 1 << 20, SegmentBytes: 4096,
		}
		sess, err := Create(dir, meta, 1<<20)
		if err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		sess.WriteCooked([]byte("x\n"))
		sess.SetCWD(cwd)
		sess.Flush()
		t.Cleanup(func() { sess.Close(nil) })
		return sess
	}

	// One live among several ended: the live one wins.
	newIdentifiedSession(t, dir, "done-1", "/work/alpha", "t", time.Minute)
	newIdentifiedSession(t, dir, "done-2", "/work/beta", "t", time.Minute)
	live("open-1", "/work/gamma")

	got, err := Resolve(dir, "cwd:/work")
	if err != nil {
		t.Fatalf("a single live match should resolve: %v", err)
	}
	if got.Meta.ID != "open-1" {
		t.Errorf("resolved to %q, want the session still recording", got.Meta.ID)
	}

	// Two live matches: refuse, and name the candidates so the caller can pick.
	live("open-2", "/work/delta")
	_, err = Resolve(dir, "cwd:/work")
	if err == nil {
		t.Fatal("two live matches resolved silently instead of erroring")
	}
	for _, want := range []string{"open-1", "open-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name candidate %s: %v", want, err)
		}
	}
}

// The default listing hides stale history but must never do so silently.
func TestFilterHidesOldButReportsCount(t *testing.T) {
	dir := t.TempDir()
	newIdentifiedSession(t, dir, "recent", "/a", "t", 30*time.Minute)
	newIdentifiedSession(t, dir, "old-1", "/b", "t", 26*time.Hour)
	newIdentifiedSession(t, dir, "old-2", "/c", "t", 48*time.Hour)

	all, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}

	shown, omitted := DefaultFilter().Apply(all)
	if len(shown) != 1 || shown[0].Meta.ID != "recent" {
		t.Errorf("default filter showed %d sessions, want just the recent one: %+v", len(shown), shown)
	}
	if omitted != 2 {
		t.Errorf("omitted count is %d, want 2 -- a hidden row that is not counted reads as if it were everything", omitted)
	}

	everything, omitted := Filter{All: true}.Apply(all)
	if len(everything) != 3 || omitted != 0 {
		t.Errorf("all=true gave %d shown and %d omitted, want 3 and 0", len(everything), omitted)
	}

	// A negative age limit means no limit.
	noLimit, omitted := Filter{EndedWithin: -1, IncludeEnded: true}.Apply(all)
	if len(noLimit) != 3 || omitted != 0 {
		t.Errorf("negative EndedWithin gave %d shown and %d omitted, want 3 and 0", len(noLimit), omitted)
	}
}

// Retention must bound history by age, and must never touch a terminal that
// is still recording.
func TestPruneOlderThanSparesLiveSessions(t *testing.T) {
	dir := t.TempDir()
	newIdentifiedSession(t, dir, "old", "/old", "t", 30*24*time.Hour)
	newIdentifiedSession(t, dir, "fresh", "/fresh", "t", time.Hour)

	// A live session: created, written, never closed, and its recorder is this
	// very process, so it is genuinely live rather than stale.
	liveMeta := Meta{
		ID: "live", StartedAt: time.Now().Add(-40 * 24 * time.Hour), Shell: "pwsh", PID: os.Getpid(),
		MaxBytes: 1 << 20, CookedMaxBytes: 1 << 20, SegmentBytes: 4096,
	}
	live, err := Create(dir, liveMeta, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	live.WriteCooked([]byte("still going\n"))
	live.Flush()
	t.Cleanup(func() { live.Close(nil) })

	removed, err := PruneOlderThan(dir, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed %d sessions, want 1", removed)
	}

	remaining, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, s := range remaining {
		ids[s.Meta.ID] = true
	}
	if ids["old"] {
		t.Error("the old finished session survived pruning")
	}
	if !ids["fresh"] {
		t.Error("a recent session was pruned")
	}
	if !ids["live"] {
		t.Error("a session still recording was pruned, even though it is 40 days old")
	}
}
