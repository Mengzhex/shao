package mcpsrv

import (
	"fmt"
	"strings"
	"time"
)

// Formatting helpers. Tool output is read by a model, so sizes and times are
// rendered the way a person would write them rather than as raw numbers: a
// model reasons better about "2.1 MiB" and "4 minutes ago" than about a byte
// count and an RFC 3339 timestamp.

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "unknown"
	case d > 48*time.Hour:
		return "more than 48h"
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	default:
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
}

func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	if d < 0 {
		return "just now"
	}
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%.0f seconds ago", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%.0f minutes ago", d.Minutes())
	case d < 24*time.Hour:
		return fmt.Sprintf("%.1f hours ago", d.Hours())
	default:
		return fmt.Sprintf("%.1f days ago", d.Hours()/24)
	}
}

// quoteLabel wraps arbitrary terminal-supplied text in quotes for display.
//
// %q would be the obvious choice but it escapes backslashes, and a terminal
// title is very often a Windows path: "C:\Program Files\..." is harder to
// read than the thing it describes. Control characters are stripped instead,
// since those are the only part that could actually corrupt the output.
func quoteLabel(text string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range text {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r < 0x20 || r == 0x7f:
			// Drop it: a newline or an escape in a title would otherwise
			// break the shape of the listing.
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
