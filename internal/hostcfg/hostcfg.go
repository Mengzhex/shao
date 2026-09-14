// Package hostcfg sets up and checks read-only access to target hosts.
//
// The security model in one paragraph: shao never holds credentials that can
// change a target host. For each host it generates a dedicated SSH key whose
// only accepted use is a forced command running the read-only probe. sshd
// enforces that, not shao, so the restriction survives anything going wrong
// on this side, including the AI being talked into asking for something else.
// There is deliberately no fallback tier where shao merely promises to send
// only safe commands: a host that cannot be set up this way is marked
// disabled and does not appear as queryable at all.
//
// shao also never writes to a target host. Installing the probe needs
// privileges that the read-only key does not have and should not have, so
// `shao host add` generates the key and an install script for a human to run
// with their own credentials. `shao host verify` then proves the result by
// trying to escape the restriction and reporting whether it could.
package hostcfg

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/Mengzhex/shao/internal/config"
)

// Tier names the enforcement level a host is being set up for.
type Tier string

const (
	// TierRoot uses a dedicated account plus an sshd_config Match block, so
	// the forced command is declared in two independent places.
	TierRoot Tier = "root"
	// TierUser adds a forced-command key to an existing account's own
	// authorized_keys. This needs no root, and sshd enforces it just the
	// same; only commands that require privilege become unavailable.
	TierUser Tier = "user"
)

// Enforcement maps a setup tier to the value stored in the config.
func (t Tier) Enforcement() config.Enforcement {
	if t == TierRoot {
		return config.EnforceForcedCommandRoot
	}
	return config.EnforceForcedCommandUser
}

// KeyPaths returns where a host's dedicated key lives.
func KeyPaths(cfg *config.Config, host string) (private, public string) {
	dir := cfg.KeysDir()
	return filepath.Join(dir, host), filepath.Join(dir, host+".pub")
}

// GenerateKey creates a dedicated ed25519 key for one host.
//
// A separate key per host means revoking access to one host is one line
// removed on that host, and it keeps shao's key out of the user's everyday
// agent where it could be picked up by something else.
func GenerateKey(cfg *config.Config, host string) (privatePath, publicKey string, err error) {
	priv, pub, err := newEd25519()
	if err != nil {
		return "", "", err
	}

	privPath, pubPath := KeyPaths(cfg, host)
	if err := config.EnsurePrivateDir(cfg.KeysDir()); err != nil {
		return "", "", err
	}
	if err := config.WritePrivateFile(privPath, priv); err != nil {
		return "", "", err
	}
	if err := config.WritePrivateFile(pubPath, pub); err != nil {
		return "", "", err
	}
	return privPath, strings.TrimSpace(string(pub)), nil
}

func newEd25519() (privatePEM, authorizedKey []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	comment := "shao-readonly-probe"

	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal private key: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment
	return pem.EncodeToMemory(block), []byte(line + "\n"), nil
}

// authorizedKeysOptions are the restrictions attached to the key itself.
//
// `restrict` turns everything off and then nothing is turned back on, which
// is the safe direction: a future OpenSSH feature is denied by default rather
// than silently permitted. The explicit no-* options are redundant on modern
// OpenSSH and are kept for servers older than 7.2 where `restrict` is not
// understood.
const authorizedKeysOptions = `restrict,no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-user-rc`

// AuthorizedKeysLine renders the line to install on the target host.
func AuthorizedKeysLine(probePath, publicKey string) string {
	return fmt.Sprintf(`command="%s",%s %s`, probePath, authorizedKeysOptions, publicKey)
}

// InstallPlan is everything a human needs to finish setting up a host.
type InstallPlan struct {
	Host       config.HostConfig
	Tier       Tier
	PublicKey  string
	ProbePath  string
	Script     string
	ScriptPath string
	// LocalProbe is the binary that has to reach the host before the script
	// can install it.
	LocalProbe string
}

// BuildInstallPlan renders the setup script for a host.
func BuildInstallPlan(host config.HostConfig, tier Tier, publicKey, localProbe string) InstallPlan {
	probePath := host.ProbePath
	if probePath == "" {
		if tier == TierRoot {
			probePath = "/usr/local/libexec/shao-probe"
		} else {
			probePath = "$HOME/.shao/shao-probe"
		}
	}

	plan := InstallPlan{
		Host:       host,
		Tier:       tier,
		PublicKey:  publicKey,
		ProbePath:  probePath,
		LocalProbe: localProbe,
	}
	if tier == TierRoot {
		plan.Script = rootScript(host, probePath, publicKey)
	} else {
		plan.Script = userScript(probePath, publicKey)
	}
	return plan
}

// Save writes the install script next to the host's key.
func (p *InstallPlan) Save(cfg *config.Config) error {
	dir := filepath.Join(cfg.Root(), "hosts", p.Host.Name)
	if err := config.EnsurePrivateDir(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, "install.sh")
	if err := config.WritePrivateFile(path, []byte(p.Script)); err != nil {
		return err
	}
	// The script is meant to be run by a human on the target host.
	_ = os.Chmod(path, 0o700)
	p.ScriptPath = path
	return nil
}

func rootScript(host config.HostConfig, probePath, publicKey string) string {
	account := host.User
	if account == "" {
		account = "aiview"
	}
	return fmt.Sprintf(`#!/bin/sh
# shao read-only access, root tier. Run this on %s as root.
#
# It creates a dedicated unprivileged account whose only possible SSH action
# is running the read-only probe. Two independent mechanisms enforce that:
# a forced command on the key, and a ForceCommand in sshd_config. Either
# alone would be enough; both together mean one mistake is not fatal.
#
# Nothing here grants write access, and the account gets no shell.
set -eu

ACCOUNT=%s
PROBE=%s

if ! id "$ACCOUNT" >/dev/null 2>&1; then
  useradd --system --create-home --shell /usr/sbin/nologin "$ACCOUNT"
fi

# The probe binary must already have been copied to /tmp/shao-probe.
install -o root -g root -m 0755 /tmp/shao-probe "$PROBE"

install -d -o "$ACCOUNT" -g "$ACCOUNT" -m 0700 "$(getent passwd "$ACCOUNT" | cut -d: -f6)/.ssh"
AUTH="$(getent passwd "$ACCOUNT" | cut -d: -f6)/.ssh/authorized_keys"
cat > "$AUTH" <<'KEY'
%s
KEY
chown "$ACCOUNT":"$ACCOUNT" "$AUTH"
chmod 0600 "$AUTH"

# Second layer. Appended once; re-running the script will not duplicate it.
if ! grep -q "shao read-only probe" /etc/ssh/sshd_config; then
  cat >> /etc/ssh/sshd_config <<SSHD

# shao read-only probe: this account can do nothing but run the probe.
Match User $ACCOUNT
    ForceCommand $PROBE
    PermitTTY no
    AllowTcpForwarding no
    AllowAgentForwarding no
    AllowStreamLocalForwarding no
    X11Forwarding no
    PermitTunnel no
    PermitOpen none
SSHD
fi

# Read-only commands that need privilege. Each entry is an exact command with
# no wildcard: a wildcard here would be a way to run something else.
cat > /etc/sudoers.d/shao-probe <<'SUDO'
%s ALL=(root) NOPASSWD: /usr/sbin/nginx -t, /usr/sbin/nginx -T, /usr/bin/ss -tulpnH
SUDO
chmod 0440 /etc/sudoers.d/shao-probe
visudo -cf /etc/sudoers.d/shao-probe

sshd -t && systemctl reload sshd
echo "shao: read-only access installed for $ACCOUNT"
echo "shao: now run  shao host verify %s  to prove it holds"
`, host.Address, account, probePath, AuthorizedKeysLine(probePath, publicKey), account, host.Name)
}

// userScript composes the authorized_keys line inside the script rather than
// embedding it ready-made.
//
// sshd does not expand environment variables in the command= option, so a
// literal "$HOME/.shao/shao-probe" there would be taken as a path with a
// dollar sign in it and never execute. The script runs on the target and knows
// the real home directory, so it is the right place to resolve it.
func userScript(probePath, publicKey string) string {
	return fmt.Sprintf(`#!/bin/sh
# shao read-only access, no-root tier. Run this on the target host as
# yourself. No administrator privileges are needed.
#
# This adds one extra key to your own authorized_keys, carrying a forced
# command. sshd is what enforces it: whatever an SSH client asks to run is
# discarded and the probe runs instead. The key shao holds therefore cannot
# do anything but read host facts, even though it authenticates as you.
#
# Commands that need root (nginx -T, process names behind listening ports)
# report themselves as unavailable rather than failing.
set -eu

PROBE="%s"

mkdir -p "$(dirname "$PROBE")"
# The probe binary must already have been copied to /tmp/shao-probe.
install -m 0755 /tmp/shao-probe "$PROBE"

mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
touch "$HOME/.ssh/authorized_keys"
chmod 600 "$HOME/.ssh/authorized_keys"

# command= is resolved by sshd literally, with no variable expansion, so the
# path has to be absolute by the time it is written. $PROBE is expanded here.
PUBKEY='%s'
OPTIONS='%s'
LINE="command=\"$PROBE\",$OPTIONS $PUBKEY"

case "$PROBE" in
  /*) ;;
  *) echo "shao: refusing to install: probe path '$PROBE' is not absolute" >&2; exit 1 ;;
esac

if ! grep -qF "$LINE" "$HOME/.ssh/authorized_keys"; then
  printf '%%s\n' "$LINE" >> "$HOME/.ssh/authorized_keys"
fi

echo "shao: read-only key installed, forced command: $PROBE"
echo "shao: now run  shao host verify  to prove it holds"
`, probePath, publicKey, authorizedKeysOptions)
}
