package shellint

import (
	"encoding/base64"
	"testing"
)

func cmdMarker(cmd string) string {
	return "\x1b]7331;cmd;" + base64.StdEncoding.EncodeToString([]byte(cmd)) + "\x07"
}

func TestParsesFullCommandCycle(t *testing.T) {
	p := NewParser()
	stream := "\x1b]133;A\x07" + // prompt
		cmdMarker("go build ./...") +
		"\x1b]133;C\x07" + // output begins
		"some output\n" +
		"\x1b]133;D;2\x07" // finished, exit 2

	events := p.Feed([]byte(stream))
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	if events[0].Kind != EvPromptStart {
		t.Errorf("event 0 is %v, want EvPromptStart", events[0].Kind)
	}
	if events[1].Kind != EvCommandText || events[1].Cmd != "go build ./..." {
		t.Errorf("event 1 is %+v, want the command text", events[1])
	}
	if events[2].Kind != EvOutputStart {
		t.Errorf("event 2 is %v, want EvOutputStart", events[2].Kind)
	}
	if events[3].Kind != EvCommandEnd || !events[3].ExitKnown || events[3].ExitCode != 2 {
		t.Errorf("event 3 is %+v, want exit 2", events[3])
	}
}

// The position reported for a marker is what the recorder turns into a byte
// offset, so it has to point just past the terminator.
func TestEventPositionIsAfterTerminator(t *testing.T) {
	p := NewParser()
	prefix := "hello"
	marker := "\x1b]133;C\x07"
	events := p.Feed([]byte(prefix + marker + "world"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if want := len(prefix) + len(marker); events[0].Pos != want {
		t.Errorf("Pos = %d, want %d", events[0].Pos, want)
	}
}

func TestMarkerSplitAcrossChunks(t *testing.T) {
	p := NewParser()
	if got := p.Feed([]byte("\x1b]133;")); len(got) != 0 {
		t.Fatalf("emitted %d events from an incomplete marker", len(got))
	}
	events := p.Feed([]byte("D;0\x07"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if !events[0].ExitKnown || events[0].ExitCode != 0 {
		t.Errorf("got %+v, want a known exit of 0", events[0])
	}
}

func TestStringTerminatorForm(t *testing.T) {
	p := NewParser()
	events := p.Feed([]byte("\x1b]133;A\x1b\\"))
	if len(events) != 1 || events[0].Kind != EvPromptStart {
		t.Fatalf("ESC-backslash terminated marker not recognised: %+v", events)
	}
}

// Unrelated OSC traffic passes through constantly and must be ignored rather
// than misread. Hyperlinks (OSC 8) are the common case; note that OSC 0 is
// *not* in this list any more, because the title is now collected on purpose.
func TestUnrelatedOSCIgnored(t *testing.T) {
	p := NewParser()
	events := p.Feed([]byte("\x1b]8;;http://x/\x1b\\\x1b]10;fg\x07\x1b]11;bg\x07\x1b]52;c;Zm9v\x07"))
	if len(events) != 0 {
		t.Errorf("unrelated OSC produced events: %+v", events)
	}
}

// The window title is free identity: a pseudoconsole emits it without the
// shell being involved, and it is one of the few things that tells several
// recorded terminals apart.
func TestTitleParsed(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"\x1b]0;C:\\Windows\\System32\\cmd.exe\x07", `C:\Windows\System32\cmd.exe`},
		{"\x1b]2;just the title\x07", "just the title"},
		{"\x1b]0;title with ; semicolons ; in it\x07", "title with ; semicolons ; in it"},
		{"\x1b]0;ended with ST\x1b\\", "ended with ST"},
	}
	for _, tc := range cases {
		p := NewParser()
		events := p.Feed([]byte(tc.in))
		if len(events) != 1 {
			t.Errorf("%q produced %d events, want 1", tc.in, len(events))
			continue
		}
		if events[0].Kind != EvTitle {
			t.Errorf("%q produced kind %v, want EvTitle", tc.in, events[0].Kind)
		}
		if events[0].Title != tc.want {
			t.Errorf("%q gave title %q, want %q", tc.in, events[0].Title, tc.want)
		}
	}

	// An empty title carries nothing and would only overwrite a useful one.
	p := NewParser()
	if events := p.Feed([]byte("\x1b]0;\x07")); len(events) != 0 {
		t.Errorf("empty title produced events: %+v", events)
	}
}

// The working directory is the single most useful discriminator between
// terminals. It arrives two ways: OSC 7 where a terminal emits it, and shao's
// own marker, which is the only source on Windows.
func TestCWDParsed(t *testing.T) {
	cwdMarker := func(dir string) string {
		return "\x1b]7331;cwd;" + base64.StdEncoding.EncodeToString([]byte(dir)) + "\x07"
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"our marker, windows path", cwdMarker(`C:\HUIXIN\cursor_local_project\looklook`), `C:\HUIXIN\cursor_local_project\looklook`},
		{"our marker, posix path", cwdMarker("/home/me/src/app"), "/home/me/src/app"},
		{"our raw fallback", "\x1b]7331;cwdraw;/home/me/src\x07", "/home/me/src"},
		{"osc 7 posix", "\x1b]7;file://myhost/home/me/src\x07", "/home/me/src"},
		{"osc 7 windows", "\x1b]7;file://myhost/C:/Users/me\x07", "C:/Users/me"},
		{"osc 7 percent-encoded", "\x1b]7;file://h/home/me/a%20dir\x07", "/home/me/a dir"},
		{"osc 7 empty host", "\x1b]7;file:///home/me\x07", "/home/me"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewParser()
			events := p.Feed([]byte(tc.in))
			if len(events) != 1 {
				t.Fatalf("produced %d events, want 1: %+v", len(events), events)
			}
			if events[0].Kind != EvCWD {
				t.Fatalf("kind %v, want EvCWD", events[0].Kind)
			}
			if events[0].CWD != tc.want {
				t.Errorf("cwd %q, want %q", events[0].CWD, tc.want)
			}
		})
	}

	// A malformed OSC 7 must be dropped, not turned into a bogus directory.
	for _, bad := range []string{"\x1b]7;\x07", "\x1b]7;not-a-url\x07", "\x1b]7;file://onlyhost\x07"} {
		p := NewParser()
		if events := p.Feed([]byte(bad)); len(events) != 0 {
			t.Errorf("%q produced events: %+v", bad, events)
		}
	}
}

// Identity markers arrive split across pty reads like everything else.
func TestIdentityMarkersSplitAcrossChunks(t *testing.T) {
	p := NewParser()
	if got := p.Feed([]byte("\x1b]0;my ti")); len(got) != 0 {
		t.Fatalf("emitted %d events from an incomplete title", len(got))
	}
	events := p.Feed([]byte("tle\x07"))
	if len(events) != 1 || events[0].Title != "my title" {
		t.Fatalf("split title not reassembled: %+v", events)
	}
}

func TestCommandWithSemicolonsAndNewlines(t *testing.T) {
	// Base64 is used precisely so framing characters inside a command cannot
	// break the marker.
	cmd := "for f in *; do echo \"$f\"; done"
	p := NewParser()
	events := p.Feed([]byte(cmdMarker(cmd)))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].Cmd != cmd {
		t.Errorf("got %q, want %q", events[0].Cmd, cmd)
	}
}

func TestRawCommandForm(t *testing.T) {
	p := NewParser()
	events := p.Feed([]byte("\x1b]7331;cmdraw;ls -la\x07"))
	if len(events) != 1 || events[0].Cmd != "ls -la" {
		t.Fatalf("cmdraw not handled: %+v", events)
	}
}

func TestExitWithoutStatus(t *testing.T) {
	p := NewParser()
	events := p.Feed([]byte("\x1b]133;D\x07"))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if events[0].ExitKnown {
		t.Error("a D marker with no status should report ExitKnown=false, not exit 0")
	}
}

func TestDetectRemoteHost(t *testing.T) {
	cases := map[string]string{
		"ssh web-prod-1":                      "web-prod-1",
		"ssh deploy@10.0.0.11":                "10.0.0.11",
		"ssh -p 2222 -i ~/.ssh/id deploy@box": "box",
		"docker exec -it api sh":              "api",
		"kubectl exec -it pod-1 -- sh":        "pod-1",
		"ls -la":                              "",
		"echo ssh is a program":               "",
		"":                                    "",
	}
	for cmd, want := range cases {
		if got := DetectRemoteHost(cmd); got != want {
			t.Errorf("DetectRemoteHost(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestDetectShellKind(t *testing.T) {
	// Paths are kept separator-neutral: filepath.Base means a Windows path
	// would not split the same way when these tests run on Linux.
	cases := map[string]Kind{
		"/bin/bash":      KindBash,
		"/usr/bin/zsh":   KindZsh,
		"pwsh.exe":       KindPwsh,
		"powershell.exe": KindPwsh,
		"/usr/bin/fish":  KindNone,
		"/bin/sh":        KindBash,
	}
	for path, want := range cases {
		if got := Detect(path); got != want {
			t.Errorf("Detect(%q) = %q, want %q", path, got, want)
		}
	}
}
