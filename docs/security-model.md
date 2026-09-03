# Security model

What "the AI can read but not execute" actually means in tmon, how it is
enforced, and where the boundaries genuinely are.

## The claim

An AI assistant connected to tmon can read terminal history recorded on this
machine and a fixed set of facts about configured remote hosts. It cannot run
a command of its choosing anywhere, cannot write to any file, and cannot change
any host. This holds even if the assistant is fed a malicious instruction, and
even if the machine running tmon is compromised — for remote hosts, at least.

## Why it is not just "we did not give it an execute tool"

Not registering a dangerous tool is a software convention. It survives exactly
as long as nobody changes the code, nobody adds a convenience flag, and no
other process can reach the same credentials.

tmon puts the restriction where it cannot be argued with: in sshd on the target
host.

```
authorized_keys on the target:
  command="/usr/local/libexec/tmon-probe",restrict,no-pty,no-port-forwarding,
  no-agent-forwarding,no-X11-forwarding,no-user-rc  ssh-ed25519 AAAA...
```

When a key carries `command=`, sshd discards whatever the client asked to run
and runs that program instead. The client's request survives only as the
`SSH_ORIGINAL_COMMAND` environment variable, which the probe reads only in
order to ignore it. So the question "what can this key do" has one answer that
does not depend on tmon's code at all.

`restrict` is used rather than a list of individual `no-*` options because it
denies everything and then turns nothing back on: a capability added to a
future OpenSSH is denied by default. The explicit `no-*` options are kept
alongside it for servers older than OpenSSH 7.2, which do not understand
`restrict`.

## The two tiers

**`--tier root`** creates a dedicated `aiview` account with `nologin` as its
shell, installs the forced-command key, *and* adds an `sshd_config` block:

```
Match User aiview
    ForceCommand /usr/local/libexec/tmon-probe
    PermitTTY no
    AllowTcpForwarding no
    AllowAgentForwarding no
    AllowStreamLocalForwarding no
    X11Forwarding no
    PermitTunnel no
    PermitOpen none
```

Two independent mechanisms now say the same thing. Either alone would be
sufficient; both together mean a single editing mistake does not open the door.

Commands that need privilege get a sudoers file listing **exact commands with
no wildcards**:

```
aiview ALL=(root) NOPASSWD: /usr/sbin/nginx -t, /usr/sbin/nginx -T, /usr/bin/ss -tulpnH
```

A wildcard here would be the hole: `nginx *` would permit `nginx -s stop`.

**`--tier user`** needs no administrator. It adds one key to an existing
account's own `~/.ssh/authorized_keys`. This is often misread as the weaker,
"trust us" option — it is not. sshd enforces a forced command on a user's own
key exactly as strictly as on a system account's. The only thing lost is
privileged reads, which report themselves as `unavailable` rather than failing.

**No third tier exists.** A host that cannot take either setup is recorded as
`disabled` and does not appear as queryable. There is no mode where tmon
connects with ordinary credentials and limits itself in its own process,
because that would offer the feeling of a guarantee without one.

## Verification, not declaration

Configuration drifts. A key gets pasted with a broken quote, sshd is edited but
never reloaded, a probe path moves. So a host is not queryable because it is
configured — it is queryable because it has been tested.

`tmon host verify` opens a connection with the read-only key and attempts:

| Attempt | Must result in |
|---|---|
| `echo <nonce>` | the nonce does not come back — no supplied command ran |
| `cat /etc/passwd` | no passwd-shaped content returned |
| `touch /tmp/<nonce> && echo created` | no confirmation — nothing was created |
| request a pty | refused |
| remote port forward (`Listen`) | refused |
| forwarded connection (`Dial`) | refused |
| probe answers a capabilities request | valid JSON reply |

All must pass. A failure sets the host's enforcement to `disabled` and writes
that to the config, so a host cannot be left half-trusted. Agent and X11
forwarding are reported as `n/a`: they are denied by `restrict`, but this
check does not exercise them independently, and saying so is better than
claiming a test that did not happen.

## The probe's own surface

Even with a forced command, the probe is what actually runs, so its input
matters. It is built so that nothing a caller supplies becomes part of a
command:

- The request is one line of JSON containing a list of **verb names**.
- Each verb maps to a literal `argv` written in `internal/probe/probe.go`.
- **No verb takes an argument.** There is no path, no filter, no pattern —
  nothing that a caller could influence.
- Commands run via `exec.Command` with an explicit argv. **No shell is
  involved**, so shell metacharacters have no meaning.
- An unknown verb is rejected outright.
- Every response echoes the exact argv that ran, so what happened is visible
  rather than asserted.
- Output is capped at 256 KiB per section and each command times out after
  10 seconds.

Widening what a probe can do means adding to one readable table in one file.
That is the whole audit surface.

## The local side

Terminal history is read straight from files on this machine. There is no
privilege boundary here — anything the user can read, tmon can read — so the
protections are about exposure rather than authority:

- Everything under `~/.tmon` is created owner-only.
- Redaction runs on the way to disk and again on the way out.
- The HTTP transports bind to loopback by default, require a bearer token
  compared in constant time, and reject any request carrying a non-loopback
  `Origin` header — otherwise any web page the user has open could post to
  localhost and read their terminal history.
- They can be published to a network with `--bind`, because an agent on another
  machine has to reach them somehow. This is gated on an explicit flag rather
  than a config value, so a setting left in a file cannot expose a terminal by
  itself. Publishing changes the exposure materially and honestly:
  - the traffic is plain HTTP, so recorded output **and the token** are
    readable by anything on the path;
  - `--allow` restricts client networks, and loopback is always permitted so
    the host cannot lock itself out;
  - the `Origin` check is dropped there. It defends against DNS rebinding,
    which is a threat specific to a localhost server; on an intentionally
    networked endpoint it is the wrong control and would only break
    browser-based clients on other machines. The token remains the gate, and
    it is one a rebinding page cannot obtain.
  - an SSH tunnel is the better answer where the remote end can arrange one:
    `ssh -N -L 7337:127.0.0.1:7337 user@host`, then point the agent at
    127.0.0.1. No exposed port, and encrypted.
- The token may also be passed as a `?token=` query parameter. This exists
  because a browser `EventSource` cannot set request headers at all, so for
  clients restricted to the SSE transport it is the only way to authenticate.
  A URL is a worse place for a secret than a header, since URLs reach logs and
  history; it is acceptable here only because the endpoint is bound to
  loopback, so the URL never leaves the machine. Prefer the header where the
  client can send one.
- The SSE stream is a response channel, not a push channel. Nothing is ever
  written to it that the client did not request, so holding a long-lived
  connection open does not make tmon push-driven.
- The stdio transport has no port and no token at all, and is preferred where
  the client supports it.

## Prompt injection

Recorded terminal output is untrusted text: it can contain anything a program
printed, including text designed to look like instructions. tmon's answer is
structural rather than filtering — there is no tool that takes a command from
the model:

- No local tool executes anything.
- The only remote path takes a verb from a closed enum, and the enum contains
  no verb that changes state.
- Even a tool call constructed entirely by injected text can therefore do
  nothing but read.

The server's instructions also tell the model to treat buffer contents as data
and to present deployment commands as suggestions for the user to run. That is
guidance, not enforcement, and it is not what the guarantee rests on.

## What this does not protect against

Stated plainly, because a security model that lists only its strengths is
not useful:

- **Anything the user runs themselves.** tmon suggests; if a suggested command
  is wrong or harmful and the user runs it, tmon did not prevent that.
- **Secrets in the buffer.** Redaction is shape-matching. An unusual credential
  format is recorded in the clear, readable by anything running as the user.
- **A compromised local machine reading history.** File permissions stop other
  users, not code running as this user. What a local compromise still cannot do
  is use tmon's host keys for anything but reads.
- **Unpinned host keys.** Until `tmon host verify` records a fingerprint, a
  machine-in-the-middle could impersonate a host and see the probe traffic. It
  still could not use the key for anything else, because the restriction lives
  on the real server.
- **The probe binary on the target.** If someone with root on the target
  replaces it, it is their machine to change. tmon's guarantee is about what
  *tmon* can do, not about a host that is already lost.
