# Building tmon

## Status

Builds, vets and tests clean on `windows/amd64` with Go 1.26.7, and
cross-compiles for `linux/{amd64,arm64}` and `darwin/{amd64,arm64}`.

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
make build        # tmon for this platform
make probe        # Linux probe binaries to upload to target hosts
make dist         # release binaries for every platform
make crosscheck   # compile all platforms, produce nothing
make test
```

To build elsewhere, override the prefix or just point at a normal Go install:

```sh
make build CONDA_ENV_PREFIX=/path/to/env
make build GO=go GOROOT=
```

## The probe binary

The probe is not a separate program. `cmd/tmon` switches into probe mode when
it is invoked under a name starting with `tmon-probe`, which is how sshd runs
it as a forced command. So `make probe` is just a Linux cross-compile of the
same source, and there is one binary to keep in step rather than two.

## Release layout

`make dist` produces:

```
dist/tmon-windows-amd64.exe     dist/tmon-linux-amd64
dist/tmon-windows-arm64.exe     dist/tmon-linux-arm64
dist/tmon-darwin-amd64          dist/tmon-probe-linux-amd64
dist/tmon-darwin-arm64          dist/tmon-probe-linux-arm64
```

`tmon host add` looks for a probe binary next to the tmon executable or in
`~/.tmon/probe/`, so shipping `tmon-probe-linux-amd64` alongside tmon makes
host setup a copy-and-paste rather than a build step.
