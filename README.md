**English** · [简体中文](README.zh-CN.md)

# tmon

Records your terminal, and lets an AI assistant read it back — read-only, and
only when you ask.

It records the byte stream of your local shells, serves them to an assistant
over MCP, and is read from only when a question you asked causes a tool call.
Nothing is watched, nothing is pushed, and the assistant cannot execute
anything.

What the assistant sees when it looks:

```
4 session(s), newest first:

- id=20260828T152532.511-63068  live
    cwd=C:\proj\infra
    title="Administrator: pwsh"
    last=terraform plan -> exit 1
    shell=pwsh  buffered=3.2 MiB  rate=2.1MB/min  reaches_back=~1.4h  commands=17
- id=20260828T152532.441-76240  ended
    cwd=C:\proj\webapp
    last=npm run build -> ok
```

Two things it is for:

- **"Why did that just fail?"** tmon captured the output, so the assistant can
  look at the command that failed and its exact output, instead of you copying
  and pasting an error out of a screen that has already scrolled away.
- **"How should I deploy this?"** tmon can read facts about a target host —
  free disk, memory, which ports are taken, what docker and nginx are already
  doing — so the advice is grounded in that machine rather than generic. The
  assistant writes the commands; **you** run them.

## What makes it different

Other MCP servers give an assistant a terminal. They are solving the opposite
problem: they hand the model a shell of its own and let it run commands in it.
tmon lets it read *yours*, and it cannot run anything at all. Five things
follow from that.

**Nothing is sampled, and completeness is checkable.** Capture is the byte
stream through the pty, so output that scrolled past faster than you could
read it is still there in full. More usefully, every read says whether the
ring had already discarded older output (`truncated`) and, separately,
whether a real discontinuity was found (`gap_detected`). "Nothing is missing"
is a claim you can check rather than a promise.

**Read-only is enforced by sshd, not by us.** The restriction is not "we did
not register an exec tool" — it is a forced command on the key, so sshd
discards whatever an SSH client asks to run. It holds even if everything on
this side were compromised, and `tmon host verify` proves it by *trying*: run
a command, read `/etc/passwd`, create a file, allocate a terminal, forward a
port, and pass only if all five are refused.

**Your terminal is untouched.** Capture sits at the pty, below the emulator,
so Windows Terminal, iTerm, PuTTY, tmux and the VS Code panel all work
unchanged. `ssh` sessions come back for free: everything you see on a remote
server inside a recorded shell is recorded *here*, with nothing installed
there and no privileges needed.

**One endpoint, any number of terminals.** No per-terminal configuration
exists to get out of sync. Sessions carry their working directory, title and
last command, so ten open terminals stay tellable apart — you ask about
`cwd:webapp`, not about an id you had to go and look up.

**Nothing runs unless you ask.** No timers, no watchers, no alerts, no
background jobs. Between your questions tmon is a recorder writing to disk and
nothing else, which is also why there is nothing to turn off.

And it is one static binary. No runtime, no daemon to install, no agent on the
servers you query.

## The two rules it is built around

**It is pull-only.** Nothing is monitored, nothing is watched, nothing is
pushed. There are no timers, no alerts, no background jobs. A tool runs when
you ask a question and the assistant calls it, and at no other time.

**The AI can read and cannot execute — enforced by the operating system, not
by trust.** For remote hosts, the SSH key tmon holds is installed on the target
with a *forced command*: sshd throws away whatever an SSH client asks to run
and runs a fixed read-only probe instead. The probe takes a verb from a closed
list and no free-form arguments, so there is no string anywhere for an
instruction to travel through. This holds even if everything on this side were
compromised. A host that cannot be set up that way is marked `disabled` and is
not queryable at all — there is deliberately no weaker mode where tmon merely
promises to behave.

## How the capture works, and why not screenshots

tmon runs your shell inside a pseudo-terminal and records the byte stream
passing through it. Every byte the shell writes goes through that pipe exactly
once, in order. Nothing is sampled, so output that scrolls past faster than you
could read it is still recorded in full, and asking a question ten minutes
later returns a complete picture rather than whatever happened to be on screen
at two sampling instants.

Because capture happens at the pty on *this* machine, it does not care which
terminal emulator you use — Windows Terminal, PuTTY, MobaXterm, iTerm, the
VS Code panel are all on the far side of it. It also captures `ssh` sessions:
if you ssh into a server inside a recorded shell, everything you see there is
recorded here, with no agent on the server and no privileges needed there.

Each session keeps two streams. The **raw** stream is byte-exact. The **cooked**
stream has carriage returns, backspaces and erase sequences applied and colour
codes removed, so a progress bar that redrew 4000 times reads as the one line
it ended up being. The assistant reads cooked by default.

**The one real limitation:** recording starts when the shell starts. A terminal
window that is already open cannot be recorded retroactively — no lossless
method can do that. So `tmon start` covers both halves at once: it records the
terminal you run it in, and installs a startup hook so every terminal opened
afterwards records itself without being asked. Nothing is per-terminal after
that first command.

## Installation

tmon is a single binary with no runtime dependencies. Download it, put it on
your `PATH`, done.

### The quick way

Linux and macOS — including a server you have just sshed into:

```sh
curl -fsSL https://github.com/Mengzhex/tmon/releases/latest/download/install.sh | sh
```

Windows, in PowerShell:

```powershell
irm https://github.com/Mengzhex/tmon/releases/latest/download/install.ps1 | iex
```

Both detect your platform, verify the download against `checksums.txt`,
install to `~/.local/bin` (or `/usr/local/bin` when run as root, `~in` on
Windows), and tell you if that directory is not on your `PATH`. Override with
`TMON_INSTALL_DIR` and pin a release with `TMON_VERSION=v0.1.0`.

Piping a script from the internet into a shell is a real decision, not a
formality. The script is short and does nothing clever —
[read it first](scripts/install.sh) if you would rather, or follow the manual
steps below, which are what it automates.

### Which file to download

Every release attaches one archive per platform. The download links below
always resolve to the newest release, so they do not go stale.

| Platform | Archive |
|---|---|
| Windows (Intel/AMD) | `tmon_windows_amd64.zip` |
| Windows (ARM) | `tmon_windows_arm64.zip` |
| macOS (Apple silicon) | `tmon_darwin_arm64.tar.gz` |
| macOS (Intel) | `tmon_darwin_amd64.tar.gz` |
| Linux (x86-64) | `tmon_linux_amd64.tar.gz` |
| Linux (ARM64) | `tmon_linux_arm64.tar.gz` |

If you are unsure of the architecture, `uname -m` answers it: `x86_64` means
amd64, `aarch64` or `arm64` means arm64.

### Windows

```powershell
$dest = "$HOME\bin"
New-Item -ItemType Directory -Force $dest | Out-Null
$url = 'https://github.com/Mengzhex/tmon/releases/latest/download/tmon_windows_amd64.zip'
Invoke-WebRequest -Uri $url -OutFile "$env:TEMP\tmon.zip"
Expand-Archive -Force "$env:TEMP\tmon.zip" -DestinationPath $dest
```

Then put `~\bin` on your `PATH` if it is not already there, and **open a new
terminal** so the change takes effect:

```powershell
$user = [Environment]::GetEnvironmentVariable('PATH', 'User')
if ($user -notlike "*$dest*") {
    [Environment]::SetEnvironmentVariable('PATH', "$user;$dest", 'User')
}
```

### Linux

```sh
curl -fsSL https://github.com/Mengzhex/tmon/releases/latest/download/tmon_linux_amd64.tar.gz | tar xz
install -Dm755 tmon ~/.local/bin/tmon
```

`~/.local/bin` is already on `PATH` on most distributions. If `command -v tmon`
comes up empty, add it:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
```

Use `sudo install -Dm755 tmon /usr/local/bin/tmon` instead to install it for
everyone on the machine.

**A name clash worth knowing about.** The Linux kernel's thermal monitor is
also called `tmon` and ships as `/usr/bin/tmon` in `linux-tools` /
`linux-misc-tools`. If `tmon start` answers *"TMON needs to be run as root"*,
you reached that program, not this one — `~/.local/bin` is either absent from
your `PATH` or comes after `/usr/bin`. `type -a tmon` shows every match in
order. Put your own directory first, or call it by path. This one never needs
root.

### macOS

```sh
curl -fsSL https://github.com/Mengzhex/tmon/releases/latest/download/tmon_darwin_arm64.tar.gz | tar xz
mkdir -p /usr/local/bin && install -m755 tmon /usr/local/bin/tmon
```

**Gatekeeper will block it on the first run**, because these binaries are not
signed with an Apple developer certificate. macOS reports this as *"tmon is
damaged and cannot be opened"*, which is misleading — the download is fine, it
just carries a quarantine flag. Clear the flag:

```sh
xattr -d com.apple.quarantine /usr/local/bin/tmon
```

Signing and notarising would remove the need for that step, and requires a paid
Apple developer account. There isn't one, so this is stated plainly rather than
left for you to discover.

### Verifying the download

Each release includes `checksums.txt`. On Linux:

```sh
curl -fsSLO https://github.com/Mengzhex/tmon/releases/latest/download/checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

On macOS use `shasum -a 256 --ignore-missing -c checksums.txt`. On Windows,
compare the hash yourself:

```powershell
(Get-FileHash "$env:TEMP\tmon.zip" -Algorithm SHA256).Hash.ToLower()
```

### Building from source

Go 1.25 or newer:

```sh
git clone https://github.com/Mengzhex/tmon.git
cd tmon
go build -o tmon ./cmd/tmon
```

See [docs/BUILD.md](docs/BUILD.md) for cross-compilation and the release
targets.

### Requirements

| | |
|---|---|
| Windows | 10 version 1809 / Server 2019 or newer — earlier versions have no ConPTY, which is what tmon captures through |
| macOS | 11 or newer |
| Linux | any distribution; the released binaries are static (CGO off), so musl-based systems such as Alpine work too |
| Shell | any shell is captured in full. bash, zsh and PowerShell additionally get per-command structure — `last=`, the working directory, and which command failed |

**Platform status.** Development and the test suite run on Windows, where the
pseudoconsole path is exercised continuously. The POSIX pty path is compiled
and cross-checked for every release and is covered by the platform-independent
tests, but it has had far less real-world use. If it misbehaves on Linux or
macOS, that is worth reporting.

## Quick start

Two commands. Open a terminal, and:

```sh
tmon start                 # monitoring on
tmon start --port 7337     # ...and serve MCP at a URL, if your client needs one
tmon end                   # monitoring off
```

`tmon start` does the three things that have to happen, so none of them has to
be discovered separately:

1. makes **every new terminal** record itself from now on (one edit to your
   shell startup file, backed up first, undone with `tmon hook uninstall`)
2. prints the configuration to paste into your AI client — **once**, not per
   terminal
3. records **this** terminal, since the window you typed it in would otherwise
   be the one blind spot

Then work normally and ask your AI: *"why did that just fail?"*

`tmon end` is the exact inverse: it stops covering new terminals and stops the
recordings that are running. Everything already captured stays readable — it
turns monitoring off, it does not delete history.

One thing to know about `end`: recording and the shell are the same object,
because tmon records by wrapping the shell in a pty. So stopping a recording
necessarily ends the shell it was recording; there is no arrangement where the
shell survives but recording stops. It asks each recorder to finish cleanly
rather than killing it, so the last output is flushed rather than lost. Use
`--keep-shells` to stop only future terminals and leave running ones alone.

The printed configuration offers three ways to connect. Use whichever your
platform supports:

- **stdio** — the client launches `tmon mcp` itself. No port, no token, nothing
  left running. This is the default and it is enough for most people.
- **Streamable HTTP** (`/mcp`) — the current URL-based transport.
- **SSE** (`/sse`) — the older HTTP+SSE transport, which is still the only one
  many hosted platforms offer. When a platform asks for an "MCP SSE URL", this
  is what it wants.

`tmon start --port 7337` already runs that endpoint. To run one *without*
recording anything — on a machine that only serves what other terminals
recorded, say — use `serve` directly:

```sh
tmon serve --port 7337   # backgrounds itself, prints the URLs, returns
```

It backgrounds itself because the endpoint has nothing more to say once it has
printed its URL, so it should not hold your terminal. Pass `--foreground` to
keep it in the window and watch its log live, ending it with Ctrl+C.

```sh
tmon serve --status    # is one running, and where
tmon serve --stop      # stop it from any terminal
```

A backgrounded endpoint survives closing the terminal that started it, and
writes anything it has to say to `~/.tmon/serve.log`.

**Never stop tmon by image name.** `taskkill /F /IM tmon.exe` or `pkill tmon`
also kills the recorder behind every recorded terminal, and since a recorder
owns that shell's pty, those shells die with it — unrelated windows break. Use
`tmon serve --stop` or `tmon end`, which ask the target to finish cleanly.

All HTTP forms bind to loopback by default, require a bearer token, and reject
requests carrying a browser `Origin`.

### An agent on another machine

```sh
tmon serve --bind 0.0.0.0 --port 7337 --allow 192.168.1.0/24
```

It prints a URL per network interface, for both transports, with the token
already in the SSE one. Two things are worth knowing before using it:

- The traffic is plain HTTP. Recorded terminal output and the token are
  readable by anything that can observe the network, and redaction catches
  credential shapes it recognises rather than all of them. Treat the reach of
  that port as the reach of your scrollback.
- `--bind` must be typed. A value in the config file alone will not publish the
  endpoint, so a stray setting cannot expose a terminal by accident.

Where the agent's machine can open an SSH tunnel, prefer it — no exposed port,
and encrypted:

```sh
ssh -N -L 7337:127.0.0.1:7337 user@this-machine   # on the agent's machine
```

then point the agent at `http://127.0.0.1:7337/sse`.

`tmon shell` records only the current terminal and changes nothing else, if you
would rather not have a startup hook at all.

## Many terminals, one endpoint

There is nothing per-terminal to configure. Recorders are independent processes
that write to `~/.tmon/sessions/`; one endpoint reads that directory, so every
terminal you record shows up through it. Open ten terminals and you still have
one MCP configuration and nothing to clean up.

What makes that usable is that sessions carry identity, so several terminals
are tellable apart — that is the listing at the top of this page.

The working directory is the reliable discriminator, and tools accept it
directly: ask about `cwd:webapp` rather than hunting for a session id. Where a
substring matches several sessions and more than one is still recording, tmon
returns an error listing them instead of guessing.

Where identity comes from:

| | source | availability |
|---|---|---|
| title | `OSC 0`/`OSC 2` | free; a pseudoconsole emits it without the shell's help |
| working directory | tmon's own marker, or `OSC 7` | needs bash, zsh or PowerShell; Windows never emits `OSC 7` |
| last command and its exit | the command index | needs bash, zsh or PowerShell |
| what is running *now* | an unfinished command block | bash and zsh only — PowerShell reports a command only once it has finished |

By default a listing shows every terminal still recording plus anything that
finished in the last two hours, and says how many older ones it left out.
Finished sessions are deleted after `buffer.retain_days` (7), which never
touches a terminal that is still recording.

## start and serve are not alternatives

`tmon start --port N` covers the usual case, so most people never type
`tmon serve` at all. The two exist because they sit on opposite sides of the
data:

| | `tmon start` | `tmon serve` |
|---|---|---|
| what it does | **records** a terminal | lets an AI **read** what was recorded, at a URL |
| direction | produces data | produces nothing |
| where | in each terminal you want captured | once, anywhere |
| needed? | always | only if you did not use `start --port` |

Without `start` there is nothing to read, and an AI asking about your terminal
correctly reports that it sees nothing. Without `serve` recording carries on
exactly as before; only URL-based clients cannot reach it. If your client took
the pasted `command` configuration, it launches `tmon mcp` itself and `serve`
is not needed at all.

## Commands

| | |
|---|---|
| `tmon start [--label NAME] [--port N]` | monitoring on: hook + config + record this terminal |
| `tmon end [--keep-hook] [--keep-shells]` | monitoring off; recorded history is kept |
| `tmon shell [--label NAME] [--quiet]` | record only this terminal, touching nothing else |
| `tmon run -- ./deploy.sh` | record a single command |
| `tmon hook install \| uninstall \| status` | record every new terminal automatically |
| `tmon sessions [--all] [--live] [--hours N]` | what has been recorded, with cwd and what each ran |
| `tmon tail [SESSION] [--lines N] [--raw]` | read a session yourself, no AI involved |
| `tmon last-error [SESSION]` | the most recent failed command and its output |
| `tmon mcp` | speak MCP on stdin/stdout (clients run this themselves) |
| `tmon serve [--foreground]` | serve MCP on an HTTP port, without recording anything |
| `tmon serve --status \| --stop` | check on, or stop, that endpoint |
| `tmon mcp-config [--format …]` | print the client configuration again |
| `tmon token rotate` | replace the HTTP bearer token |
| `tmon host add \| list \| verify \| query` | read-only access to target hosts |
| `tmon config path \| show \| init` | configuration |
| `tmon version` | which build this is |

`--home DIR` works on every command and moves the whole state directory, which
defaults to `~/.tmon` or `$TMON_HOME`. `tmon help --all` prints every option.

## Sessions

Label a session to ask about it by name later:

```sh
tmon shell --label deploy
```

The assistant selects a session with `latest` (the default, preferring one that
is still open), `cwd:webapp`, `label:deploy`, `host:web-prod-1`, or a session
id. `cwd:` is usually the one to reach for with several terminals open, since
the working directory is what actually distinguishes them. A session is
attributed to a host through the `sessions:` rules in the config file, not on
the command line.

When a selector matches several sessions and more than one is live, tmon
returns an error listing them rather than guessing — answering from the wrong
terminal is worse than asking which one you meant.

When the shell is bash, zsh or PowerShell, tmon also records where each command
started and ended and what it exited with. That is what turns *"why did that
fail"* into a lookup — the last non-zero exit and its own output — instead of
"here are the last 500 lines". Other shells are still captured in full; only
that precision is missing, and tmon says so in its answers rather than pretending.

## Tips

**Start it before the long thing, not after.** Recording begins when the shell
starts, so a build that has already failed in an unrecorded window is gone.
`tmon start` installs the hook once and every terminal you open afterwards is
covered, which is the version of this you do not have to remember.

**Ask by directory.** With several terminals open, "why did the build in
webapp fail?" works because `cwd:webapp` selects it. Copying session ids
around is the slow path, and tmon refuses to guess between two live matches
rather than answering from the wrong terminal.

**`last-error` beats `tail`.** In bash, zsh and PowerShell tmon knows where
each command started and ended, so "the last failed command and its own
output" is a lookup. Reaching for the last 500 lines instead makes the
assistant do the parsing, and it will sometimes get it wrong.

**Label things you will come back to.** `tmon start --label deploy` lets you
ask about `label:deploy` next week, when the working directory has stopped
being memorable.

**ssh sessions are recorded on this side.** You do not need tmon on the
server. Run `ssh` inside a recorded shell and the remote output is captured
locally — useful precisely on machines where you cannot install anything.

**Size the buffer from measurements, not guesses.** `tmon sessions` prints
each session's real write rate and how far back its buffer currently reaches.
If that is shorter than the gap between something breaking and you asking
about it, raise `buffer.max_bytes`. See
[docs/buffer-sizing.md](docs/buffer-sizing.md).

**Stop it with its own commands.** `tmon end`, or `tmon serve --stop` for just
the endpoint. `taskkill /IM tmon.exe` and `pkill tmon` kill the recorder behind
every recorded terminal, and a recorder owns its shell's pty, so unrelated
windows die with it.

**Know the edge of redaction.** It catches credential *shapes* it recognises,
in both directions — on write and again on read. It is not a guarantee.
Real password prompts turn echo off so those characters never reach the pty at
all; what redaction is for is the token you typed on a command line or a
script printed.

## Remote hosts

```sh
tmon host add web-prod-1 --address 10.0.0.11 --user deploy --tier user
# copy the probe and script it prints, run the script on the target
tmon host verify web-prod-1
```

Two tiers, both enforced by sshd:

- `--tier root` — a dedicated account plus an `sshd_config` `Match` block *and*
  a forced command on the key. Two independent layers, so one mistake is not
  fatal. Privileged read-only commands (`nginx -T`, process names behind
  listening ports) work, via a sudoers allowlist of exact commands with no
  wildcards.
- `--tier user` — one extra key in your own `~/.ssh/authorized_keys` carrying
  `command="..."` and `restrict`. **No root needed**, and sshd still does the
  enforcing. Commands needing privilege report themselves as unavailable
  instead of failing.

`tmon host verify` is the part that matters. It does not read the config and
declare success: it tries to run a command, read `/etc/passwd`, create a file,
allocate a terminal, and forward a port, and passes only if every one is
refused. **A host is not queryable until it passes**, and a host that fails is
set back to `disabled` automatically.

tmon never writes to a target host. Installing the probe needs privileges the
read-only key does not have and should not have, so `tmon host add` generates
the key and an install script for you to run with your own credentials.

## Buffer sizing

Each session gets a ring buffer of segment files. When it fills, the oldest
whole segment is dropped, so what remains is always one unbroken run — there
is never a hole in the middle. Every read reports whether the ring had already
discarded older output (`truncated`) and, separately, whether a genuine
discontinuity was detected (`gap_detected`), so "nothing is missing" stays a
checkable claim rather than an assumption.

Defaults are 256 MiB of raw plus 64 MiB of cooked per session. Heavy log spam
runs 2–5 MB/min, so that reaches back roughly 1–2 hours. `tmon sessions` shows
each session's *measured* rate and how far back its buffer actually reaches, so
you can size it against your real load. See [docs/buffer-sizing.md](docs/buffer-sizing.md).

## Sensitive output

Anything that appeared on your screen is in the buffer. Redaction runs twice —
once before writing to disk, and again before anything is returned to the AI —
covering assignments to secret-looking names, `Authorization` headers, AWS and
GitHub and Slack and JWT shapes, private key blocks, `mysql -p`, `curl -u`, and
credentials in URLs. Everything tmon writes is owner-only.

Be clear-eyed about the limits: this is a filter that catches shapes it knows,
not a guarantee. Real password prompts turn echo off, so those characters never
reach the pty at all — what redaction is actually for is the credential you
typed on a command line or a script printed.

## Requirements traceability

| # | Requirement | Where |
|---|---|---|
| 1 | Pull-only; no monitoring, alerting or pushing | `internal/mcpsrv` — no timers, no subscriptions; `capabilities` advertises tools only |
| 2 | AI read-only, enforced at the system layer | `internal/probe` (closed verb list, no shell, fixed argv) + sshd forced command; proven by `tmon host verify` |
| 3 | Scenario A: recent output and error analysis | `get_last_error`, `get_recent_output`, `search_output`, `get_command_history` |
| 4 | Scenario B: host facts for deployment advice | `get_host_env`, `get_deploy_context` |
| 5 | Lossless capture, not screenshots | `internal/record` pty capture; `internal/store` contiguity checks |
| 6 | Configurable buffer size | `buffer.max_bytes` global, per-label override, global disk cap |
| 7 | Select which session to query | labels, hosts, working directories, ids; `store.Resolve` refuses ambiguous matches |
| 8 | Multiple hosts | `hosts:` list, each with its own key, tier and verification state |
| — | Works with most terminals | capture at the pty makes the terminal emulator irrelevant |

## Documentation

- [docs/security-model.md](docs/security-model.md) — what "read-only" means here, and what it does not cover
- [docs/buffer-sizing.md](docs/buffer-sizing.md) — choosing buffer sizes
- [docs/BUILD.md](docs/BUILD.md) — building, and the toolchain problem on this machine
