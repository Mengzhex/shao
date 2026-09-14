# Building tmon

## Status

Builds, vets and tests clean on `windows/amd64` with Go 1.26.7, and
cross-compiles for `windows/arm64`, `linux/{amd64,arm64}` and
`darwin/{amd64,arm64}`.

`.github/workflows/ci.yml` runs the suite on windows, ubuntu and macos
runners. That matters more than it looks: capture has two implementations
behind build tags -- ConPTY and the POSIX pty -- so passing on one platform
proves nothing about the other. Until that workflow has actually run, the
POSIX path has only ever been compiled here, never executed, because this
machine has no Linux or macOS to run it on.

Verified end to end on Windows against the real binary:

- recording through ConPTY, including UTF-8 output
- 30,000 lines flooded through a 256 KiB ring: the surviving sequence is
  contiguous, `first=1 last=30000 gaps=0`
- `get_last_error` returning the failed command, its exit code and its own
  output, with credentials redacted in both the stream and the command text
- MCP over stdio: handshake, `tools/list` (8 tools, all `readOnlyHint`),
  tool calls, and notifications correctly left unanswered
- MCP over HTTP: 401 without a token, 401 with a wrong one, 403 for a browser
  `Origin`, 405 for `GET`, 200 for a valid call
- the probe's verb whitelist rejecting an unknown verb without executing
  anything, including the valid verb sent alongside it
- `host add` generating an ed25519 key and an install script that passes
  `sh -n`, with the forced-command path resolving to an absolute path
- **the PowerShell command index**, by piping CR-terminated keystrokes into the
  recorded pty: a successful cmdlet records 0, a failing cmdlet 1, a native
  `cmd /c exit 7` records 7, and the command after a failure records 0 rather
  than inheriting the previous exit code

Not yet exercised:

- **A human at a keyboard.** Keystrokes piped into the pty cover the same code
  path, but raw-mode echo, window resize and interactive full-screen programs
  have not been watched by a person.

  This gap is not theoretical. A test harness runs tmon with pipes on stdout,
  so the real console is never attached, and one whole class of bug lives only
  on that path: what tmon forwards *to* the terminal. A pseudoconsole asks its
  host for win32-input-mode and focus reporting by writing `ESC[?9001h` and
  `ESC[?1004h` into its output. Forwarded to a real terminal, those make it
  switch input encoding, after which its keystrokes arrive as `ESC[...;32;1_`
  and get rendered as literal text: an unusable terminal. Every automated test
  passed throughout. See internal/record/modefilter.go, and prefer a real
  console when testing anything on the output path.
- **bash and zsh integration.** Only the PowerShell snippet has been run; the
  other two need a POSIX machine.
- **Scenario B against a live host.** `tmon host verify` and `get_host_env`
  need a real Linux target over SSH. The probe dispatcher and the install
  scripts are verified locally; the SSH round trip is not.

## Never stop tmon by image name

`taskkill /F /IM tmon.exe` and `pkill tmon` are the wrong instrument, always.

Every recorded terminal runs its own `tmon` process: the recorder that owns
that shell's pseudo-terminal. Killing by image name therefore kills the
recorders too, and because a recorder owns the pty, the shells it wrapped die
with it. Unrelated terminal windows break, and their sessions are left marked
live with their last output unflushed.

This happened during development, to real windows the user had open.

Use instead:

```sh
tmon serve --stop      # stops the HTTP endpoint, touches no recorder
tmon end               # stops recording, and the endpoint, cleanly
```

Both work by asking the target to shut down and confirming that it did, so
nothing is lost. If a single process really is wedged, kill that one **pid** --
never the image.

## Why the toolchain was broken on this machine

This machine runs endpoint DLP software that transparently encrypts files by
extension, and `.h` is one of the extensions it covers. Processes on its trust
list see plaintext; everything else sees ciphertext.

Go's assembler is not on that list. So when it tries to read Go's own public
headers in `$GOROOT/pkg/include/` — `textflag.h`, `funcdata.h`, `asm_amd64.h`
and friends — it gets encrypted bytes:

```
# sync/atomic
textflag.h:1:1: invalid UTF-8 encoding
textflag.h:1: expected identifier, found "\x88"
asm: assembly of .../sync/atomic/asm.s failed
```

Every Go build assembles parts of the standard library, so **every** build
fails, including a hello-world in an empty directory.

How to confirm it is this and not a corrupt download: read one of those headers
from two different processes.

```sh
# Git Bash is on the trust list and sees the real 1501-byte file
md5sum "$GOROOT/pkg/include/textflag.h"
```
```powershell
# PowerShell is not, and sees 5597 bytes starting 88 7d 1c ...
Get-FileHash "$env:GOROOT\pkg\include\textflag.h" -Algorithm MD5
```

Different hashes for the same path is the signature. Reinstalling does not help
— a second download produced *different* ciphertext with the same structure,
which is what in-place encryption looks like and what a corrupt transfer does
not.

Note that `.go` files are **not** covered by the policy, so tmon's own source is
unaffected, and tmon has no `.h` files of its own. Exactly five public Go
toolchain headers are the entire blocker.

### How it was resolved

The official Go distribution was downloaded as a zip and extracted with
PowerShell into the conda environment, replacing the conda-forge build. The
DLP encrypts files written by *trusted* processes; conda is on that list and
PowerShell is not, so the same headers land as plaintext. Nothing about the
policy or the security software was changed, and the broken copy is parked
next to it as `go.conda-broken` if it is ever wanted back.

Confirm the toolchain is healthy with:

```sh
. scripts/goenv.sh && "$GO" build ./...
```

`$GOROOT/pkg/include/textflag.h` should be 1501 bytes of plaintext. If a future
`conda install` re-encrypts it, this is the symptom to look for.

### Other ways forward

1. **Ask IT to add the Go toolchain to the DLP trust list** — `go.exe`,
   `asm.exe`, `compile.exe`, `link.exe`, the same way Git Bash is already on
   it. This is the clean fix and it fixes every future Go project too.
2. **Build somewhere else** — WSL (`wsl --install -d Ubuntu`; its ext4
   filesystem is not behind the Windows filter driver), a container, or CI.
   Cross-compile to `windows/amd64` and copy the binary back.

## Normal build

The toolchain lives in the conda environment `gotmon`, holding the official Go
distribution rather than the conda-forge package (see above). `scripts/goenv.sh`
exports `GOROOT` and `GO` so no shell needs to be activated:

```sh
. scripts/goenv.sh     # exports GOROOT and GO
"$GO" version          # go1.26.7 windows/amd64
```

Then:

```sh
"$GO" build -o tmon.exe ./cmd/tmon    # this platform
"$GO" test ./...
"$GO" vet ./...
"$GOFMT" -l ./cmd ./internal          # prints nothing when clean
bash scripts/release.sh               # every release artifact, into dist/
```

The `Makefile` wraps the same commands (`make build`, `make test`, `make
dist`, `make crosscheck`), but **`make` is not installed on this machine**, so
the commands above are the ones that actually work here. On a machine that has
it, override the toolchain location or point at a normal Go install:

```sh
make build CONDA_ENV_PREFIX=/path/to/env
make build GO=go GOROOT=
```

`scripts/release.sh` finds the toolchain the same way: it sources
`scripts/goenv.sh`, checks whether the resulting `$GO` actually runs, and falls
back to `go` on `PATH`. That is what lets one script serve both this machine
and a CI runner.

`goenv.sh` exports `GOROOT` **only when the conda prefix actually exists**.
Exporting one that does not is worse than exporting none: it breaks the
toolchain that *is* installed, with `go: cannot find GOROOT directory` and exit
2. The first CI run found this the hard way, failing with "no usable Go
toolchain" while Go sat on the runner's `PATH`.

## The probe binary

The probe is not a separate program. `cmd/tmon` switches into probe mode when
it is invoked under a name starting with `tmon-probe`, which is how sshd runs
it as a forced command. So the probe is just a Linux cross-compile of the same
source, and there is one binary to keep in step rather than two.
`scripts/release.sh` builds both architectures as part of a normal release.

## Release layout

`bash scripts/release.sh [VERSION]` produces:

```
dist/tmon_windows_amd64.zip     dist/tmon_linux_amd64.tar.gz
dist/tmon_windows_arm64.zip     dist/tmon_linux_arm64.tar.gz
dist/tmon_darwin_amd64.tar.gz   dist/tmon-probe-linux-amd64
dist/tmon_darwin_arm64.tar.gz   dist/tmon-probe-linux-arm64
dist/install.sh                 dist/checksums.txt
dist/install.ps1
```

Three decisions are worth knowing, because each one is load-bearing somewhere
else:

**Asset names carry no version.** That is what makes
`https://github.com/Mengzhex/tmon/releases/latest/download/tmon_linux_amd64.tar.gz`
always resolve to the newest release, so the install commands in the README do
not have to be edited on every release and a Scoop or Homebrew manifest only
has to change its hash. The tag is still stamped into the binary through
`-ldflags`, and `tmon version` reports it.

**Archives contain the binary and nothing else.** The README's install
commands extract straight into a directory on `PATH`, so a bundled licence or
README would land in the user's `~/bin` next to the executable.

**The probe ships as a bare binary, not an archive.** It gets copied to a
target host with `scp`, where an archive would only add a step. `tmon host add`
looks for it next to the tmon executable or in `~/.tmon/probe/`, so shipping it
alongside tmon makes host setup a copy-and-paste rather than a build step.

**The installers are stamped, not hand-edited.** `scripts/install.sh` and
`scripts/install.ps1` name this repository directly, so they run straight from
a clone. `scripts/release.sh` rewrites that slug when it copies them into
`dist/` and the real one differs, taking it from `$GITHUB_REPOSITORY` under
Actions and from the `origin` remote otherwise. A fork therefore publishes an
installer pointing at the fork, with nothing to remember at release time.

Packaging tools differ by machine, so the script checks rather than assumes:
`zip` when present, otherwise bsdtar, which libarchive lets write zip files.
This machine has neither `make` nor `zip` but does ship bsdtar in
`C:\Windows\System32`.

## Cutting a release

Pushing a tag is the entire procedure. There is no upload step and no registry
account anywhere: Go's module proxy pulls from the repository, and every
package manifest (Scoop, Homebrew, winget) holds only a URL pointing at the
assets below.

```sh
git tag v0.1.0
git push origin main --tags
```

`.github/workflows/release.yml` then runs the test suite on all three
platforms, and only if that passes does it build the artifacts and create the
release. A build that cannot pass its own tests on macOS should not be
downloadable as a macOS binary.

To do it by hand instead, with the `gh` CLI:

```sh
bash scripts/release.sh v0.1.0
gh release create v0.1.0 dist/* --generate-notes
```

`gh` is not installed on this machine; `winget install GitHub.cli` adds it.

## Getting a build without cutting a release

`ci.yml` runs `scripts/release.sh` on every push to `main` and uploads `dist/`
as a workflow artifact named `tmon-dist`, kept for 14 days. It is reachable
from the run's summary page in the Actions tab, or with the CLI:

```sh
gh run download --name tmon-dist
```

That is the path to use when a server needs a binary before any version has
been tagged. The artifact is a zip of the same files a release would attach,
so the manual install steps in the README apply unchanged.
