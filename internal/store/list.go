package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Mengzhex/shao/internal/config"
)

// Info is a session as seen by a reader: its metadata plus what its buffers
// currently hold.
type Info struct {
	Meta Meta   `json:"meta"`
	Dir  string `json:"-"`

	RawSpan    Span `json:"raw_span"`
	CookedSpan Span `json:"cooked_span"`

	// Commands is how many command blocks the index holds. Zero on a shell
	// with no integration hook, which means only byte and line reads work.
	Commands int `json:"commands"`
	// LastActivity is when output was last written.
	LastActivity time.Time `json:"last_activity"`

	// BytesPerMinute is the measured raw write rate over the session's life.
	// It is reported so the ring size can be tuned against the load that
	// actually occurs rather than a guess.
	BytesPerMinute float64 `json:"bytes_per_minute"`
	// RetentionEstimate is roughly how far back the ring reaches at that
	// rate, which is the number that answers "will the buffer still hold
	// what I want to ask about".
	RetentionEstimate time.Duration `json:"retention_estimate_ns"`

	// Running is the command in flight, if any: an index entry that was opened
	// and never closed. It answers "what is that terminal doing right now",
	// which is usually the reason for looking at a list of terminals at all.
	Running string `json:"running,omitempty"`
	// RunningSince is when that command started.
	RunningSince time.Time `json:"running_since,omitempty"`
	// LastCommand is the most recent finished command, with its exit status,
	// used when nothing is running.
	LastCommand string `json:"last_command,omitempty"`
	LastExit    *int   `json:"last_exit,omitempty"`

	// Stale means the session was never marked ended but its recorder process
	// is gone, which happens when a recorder is killed rather than allowed to
	// exit. Metadata alone cannot tell the difference, and treating such a
	// session as live is actively harmful: it would let a dead terminal
	// shadow a real one when resolving "latest", and would report an open
	// window to the AI that the user closed long ago.
	Stale bool `json:"stale"`
}

// Live reports whether the session is still recording. A session whose
// recorder has died is not live, whatever its metadata says.
func (i Info) Live() bool { return i.Meta.Live() && !i.Stale }

// Describe renders one session for `shao sessions`.
//
// Two lines rather than one: with several terminals recorded, the working
// directory and what is running are the only things that tell them apart, and
// they do not fit alongside the counters on a single line.
func (i Info) Describe() string {
	status := "ended"
	switch {
	case i.Live():
		status = "live"
	case i.Stale:
		status = "stale"
	}
	label := i.Meta.Label
	if label == "" {
		label = "-"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%-28s  %-6s  %-10s  %8s buffered  %6.1f MB/min  ~%s back  %d cmds",
		i.Meta.ID, status, label,
		humanBytes(i.RawSpan.Bytes()),
		i.BytesPerMinute/(1<<20),
		humanDuration(i.RetentionEstimate),
		i.Commands,
	)
	var detail []string
	if i.Meta.CWD != "" {
		detail = append(detail, "cwd "+i.Meta.CWD)
	}
	if i.Meta.Host != "" {
		detail = append(detail, "host "+i.Meta.Host)
	}
	switch {
	case i.Running != "":
		detail = append(detail, "running "+i.Running)
	case i.LastCommand != "":
		last := "last " + i.LastCommand
		if i.LastExit != nil && *i.LastExit != 0 {
			last += fmt.Sprintf(" (exit %d)", *i.LastExit)
		}
		detail = append(detail, last)
	}
	if len(detail) > 0 {
		fmt.Fprintf(&b, "\n    %s", strings.Join(detail, "  |  "))
	}
	return b.String()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "n/a"
	case d > 48*time.Hour:
		return ">48h"
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	default:
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
}

// List returns every readable session, newest first.
//
// A directory that cannot be parsed is skipped rather than failing the whole
// listing: one damaged session must not make the others unreachable.
func List(sessionsDir string) ([]Info, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := Describe(filepath.Join(sessionsDir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(a, b int) bool {
		return out[a].Meta.StartedAt.After(out[b].Meta.StartedAt)
	})
	return out, nil
}

// DefaultEndedWithin is how far back a finished session is still listed by
// default. Long enough to cover the terminal you just closed, which is a
// common thing to ask about; short enough that yesterday's sessions do not
// bury today's.
const DefaultEndedWithin = 2 * time.Hour

// Filter selects which sessions a listing should show.
type Filter struct {
	// EndedWithin hides finished sessions older than this. Zero means use
	// DefaultEndedWithin; a negative value means no age limit.
	EndedWithin time.Duration
	// IncludeEnded false shows only sessions still recording.
	IncludeEnded bool
	// All overrides everything and shows the lot.
	All bool
}

// DefaultFilter is what a caller that has no opinion should use.
func DefaultFilter() Filter {
	return Filter{EndedWithin: DefaultEndedWithin, IncludeEnded: true}
}

// Apply splits sessions into those to show and the number omitted.
//
// The count is returned rather than discarded on purpose: a listing that
// quietly drops rows reads as "this is everything", and a caller deciding
// which terminal to look at needs to know there are others. The same reason
// Span reports Truncated instead of just returning less.
func (f Filter) Apply(sessions []Info) (shown []Info, omitted int) {
	if f.All {
		return sessions, 0
	}
	within := f.EndedWithin
	if within == 0 {
		within = DefaultEndedWithin
	}
	now := time.Now()
	for _, s := range sessions {
		if s.Live() {
			shown = append(shown, s)
			continue
		}
		if !f.IncludeEnded {
			omitted++
			continue
		}
		if within > 0 && now.Sub(s.LastActivity) > within {
			omitted++
			continue
		}
		shown = append(shown, s)
	}
	return shown, omitted
}

// Describe loads one session directory.
func Describe(dir string) (Info, error) {
	meta, err := ReadMeta(dir)
	if err != nil {
		return Info{}, err
	}
	info := Info{Meta: meta, Dir: dir}
	// A recorder that was killed never got to mark its session ended, so the
	// process itself is the authority on whether recording is still going on.
	if meta.Live() && !ProcessAlive(meta.PID) {
		info.Stale = true
	}

	if info.RawSpan, err = ReadSpan(StreamDir(dir, StreamRaw)); err != nil {
		return Info{}, err
	}
	info.CookedSpan, _ = ReadSpan(StreamDir(dir, StreamCooked))

	if idx, err := ReadIndex(dir); err == nil {
		for _, e := range idx {
			if e.Kind != IndexCmd {
				continue
			}
			info.Commands++
			// A command block with no end is one still running. Only the last
			// such block matters; earlier ones are commands whose exit marker
			// was missed, not commands still in flight.
			if e.EndedAt == nil {
				info.Running, info.RunningSince = e.Cmd, e.StartedAt
				continue
			}
			info.Running, info.RunningSince = "", time.Time{}
			info.LastCommand, info.LastExit = e.Cmd, e.ExitCode
		}
	}
	// A session that is not recording cannot have anything in flight, whatever
	// its index last said.
	if !info.Meta.Live() || info.Stale {
		info.Running, info.RunningSince = "", time.Time{}
	}

	info.LastActivity = lastActivity(dir, meta)

	// NewestOffset is the total number of bytes ever written, so dividing by
	// elapsed time gives the real load rather than what is still retained.
	elapsed := info.LastActivity.Sub(meta.StartedAt)
	if elapsed > 0 {
		info.BytesPerMinute = float64(info.RawSpan.NewestOffset) / elapsed.Minutes()
		if info.BytesPerMinute > 0 {
			minutes := float64(meta.MaxBytes) / info.BytesPerMinute
			info.RetentionEstimate = time.Duration(minutes * float64(time.Minute))
		}
	}
	return info, nil
}

func lastActivity(dir string, meta Meta) time.Time {
	if meta.EndedAt != nil {
		return *meta.EndedAt
	}
	// The newest raw segment's mtime is the cheapest true signal of when
	// output last landed.
	rawDir := StreamDir(dir, StreamRaw)
	if segs, err := listSegments(rawDir); err == nil && len(segs) > 0 {
		newest := segs[len(segs)-1]
		if st, err := os.Stat(filepath.Join(rawDir, segName(newest.Seq))); err == nil {
			return st.ModTime()
		}
	}
	return meta.StartedAt
}

// Resolve turns a session selector into one session.
//
// Accepted forms, in the order they are tried:
//
//	""  or "latest"   the most recently active session, preferring a live one
//	"label:deploy"    by label
//	"host:web-prod-1" the most recent session attributed to that host
//	"cwd:looklook"    by a substring of the working directory
//	"id:<id>"         by exact id
//	"<prefix>"        an id prefix, or failing that a label
//
// Ambiguity is an error rather than a silent pick, because answering a
// question from the wrong terminal is worse than asking which one was meant.
func Resolve(sessionsDir, selector string) (Info, error) {
	sessions, err := List(sessionsDir)
	if err != nil {
		return Info{}, err
	}
	if len(sessions) == 0 {
		return Info{}, fmt.Errorf("no recorded sessions yet: start one with `shao shell`")
	}

	sel := strings.TrimSpace(selector)
	if sel == "" || strings.EqualFold(sel, "latest") {
		return mostRecent(sessions), nil
	}

	if rest, ok := cutPrefix(sel, "label:"); ok {
		return uniqueMatch(sessions, sel, func(i Info) bool {
			return strings.EqualFold(i.Meta.Label, rest)
		})
	}
	if rest, ok := cutPrefix(sel, "host:"); ok {
		return uniqueMatch(sessions, sel, func(i Info) bool {
			return strings.EqualFold(i.Meta.Host, rest)
		})
	}
	if rest, ok := cutPrefix(sel, "id:"); ok {
		return uniqueMatch(sessions, sel, func(i Info) bool {
			return i.Meta.ID == rest
		})
	}
	if rest, ok := cutPrefix(sel, "cwd:"); ok {
		// Substring, because a directory is how a person refers to a terminal
		// ("the looklook one") and they will not type the full path.
		needle := strings.ToLower(rest)
		return uniqueMatch(sessions, sel, func(i Info) bool {
			return strings.Contains(strings.ToLower(i.Meta.CWD), needle)
		})
	}

	// Bare selector: try an exact id, then an id prefix, then a label.
	for _, s := range sessions {
		if s.Meta.ID == sel {
			return s, nil
		}
	}
	if info, err := uniqueMatch(sessions, sel, func(i Info) bool {
		return strings.HasPrefix(i.Meta.ID, sel)
	}); err == nil {
		return info, nil
	}
	return uniqueMatch(sessions, sel, func(i Info) bool {
		return strings.EqualFold(i.Meta.Label, sel)
	})
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return "", false
}

// mostRecent prefers a live session, so a question asked while a terminal is
// open is answered from that terminal rather than from yesterday's.
func mostRecent(sessions []Info) Info {
	var best Info
	var found bool
	for _, s := range sessions {
		if !s.Live() {
			continue
		}
		if !found || s.LastActivity.After(best.LastActivity) {
			best, found = s, true
		}
	}
	if found {
		return best
	}
	// List is already sorted newest first.
	return sessions[0]
}

func uniqueMatch(sessions []Info, selector string, pred func(Info) bool) (Info, error) {
	var matches []Info
	for _, s := range sessions {
		if pred(s) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return Info{}, fmt.Errorf("no session matches %q (see `shao sessions`)", selector)
	case 1:
		return matches[0], nil
	}
	// Several sessions can legitimately share a label, one per window. Take
	// the live one when exactly one is live; otherwise make the caller choose.
	var live []Info
	for _, m := range matches {
		if m.Live() {
			live = append(live, m)
		}
	}
	if len(live) == 1 {
		return live[0], nil
	}
	candidates := make([]string, 0, len(matches))
	for _, m := range matches {
		candidates = append(candidates, m.Meta.ID)
	}
	return Info{}, fmt.Errorf("%q matches %d sessions (%s); use an id to disambiguate",
		selector, len(matches), strings.Join(candidates, ", "))
}

// Finalize marks a session ended, for use on a stale one whose recorder died
// without doing it.
//
// The end time is the last observed activity rather than now, because that is
// when recording actually stopped; stamping the current time would claim the
// session was capturing output during a period when nothing was running.
func Finalize(dir string, endedAt time.Time) error {
	meta, err := ReadMeta(dir)
	if err != nil {
		return err
	}
	if meta.EndedAt != nil {
		return nil
	}
	meta.EndedAt = &endedAt
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return config.WritePrivateFile(filepath.Join(dir, metaFile), append(data, '\n'))
}

// PruneOlderThan deletes finished sessions whose last activity is older than
// the given age, and reports how many went.
//
// Only finished sessions are considered, and Info.Live() already excludes a
// session whose recorder was killed, so a terminal that is still recording can
// never be deleted from under itself.
func PruneOlderThan(sessionsDir string, age time.Duration) (int, error) {
	if age <= 0 {
		return 0, nil
	}
	sessions, err := List(sessionsDir)
	if err != nil || len(sessions) == 0 {
		return 0, err
	}
	cutoff := time.Now().Add(-age)
	var removed int
	for _, s := range sessions {
		if s.Live() {
			continue
		}
		if s.LastActivity.After(cutoff) {
			continue
		}
		if err := os.RemoveAll(s.Dir); err != nil {
			// A session directory can be held open by a reader in another
			// process on Windows. Skipping it costs one more retained session
			// until the next start, which is cheaper than failing the caller.
			continue
		}
		removed++
	}
	return removed, nil
}

// EnforceGlobalCap deletes whole ended sessions, oldest first, until the tree
// fits within capBytes bytes.
//
// Live sessions are never touched: their recorders hold files open, and the
// point of the cap is to bound accumulated history, not to interfere with a
// terminal someone is using.
func EnforceGlobalCap(sessionsDir string, capBytes int64) error {
	if capBytes <= 0 {
		return nil
	}
	sessions, err := List(sessionsDir)
	if err != nil || len(sessions) == 0 {
		return err
	}

	type sized struct {
		info Info
		size int64
	}
	var total int64
	var ended []sized
	for _, s := range sessions {
		size := dirSize(s.Dir)
		total += size
		if !s.Live() {
			ended = append(ended, sized{s, size})
		}
	}
	if total <= capBytes {
		return nil
	}
	// Oldest first.
	sort.Slice(ended, func(a, b int) bool {
		return ended[a].info.Meta.StartedAt.Before(ended[b].info.Meta.StartedAt)
	})
	for _, e := range ended {
		if total <= capBytes {
			break
		}
		if err := os.RemoveAll(e.info.Dir); err != nil {
			continue
		}
		total -= e.size
	}
	return nil
}

func dirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
