#!/bin/sh
# tmon installer for Linux and macOS.
#
#   curl -fsSL https://github.com/OWNER/tmon/releases/latest/download/install.sh | sh
#
# Environment:
#   TMON_INSTALL_DIR   where to put the binary. Default: ~/.local/bin, or
#                      /usr/local/bin when running as root.
#   TMON_VERSION       a tag such as v0.1.0. Default: the latest release.
#   TMON_REPO          owner/name, if you forked it.
#
# POSIX sh on purpose: a server may not have bash, and the whole point of this
# script is that it runs on a machine you have not prepared.
set -eu

REPO="${TMON_REPO:-OWNER/tmon}"
VERSION="${TMON_VERSION:-latest}"

say() { printf '%s\n' "$*"; }
die() {
	printf 'tmon install: %s\n' "$*" >&2
	exit 1
}

need() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- platform

os=$(uname -s)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported operating system: $os (tmon builds for Linux, macOS and Windows)" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture: $arch (tmon builds for amd64 and arm64)" ;;
esac

asset="tmon_${os}_${arch}.tar.gz"

if [ "$VERSION" = latest ]; then
	base="https://github.com/$REPO/releases/latest/download"
else
	base="https://github.com/$REPO/releases/download/$VERSION"
fi

# ---------------------------------------------------------------- destination

if [ -n "${TMON_INSTALL_DIR:-}" ]; then
	dest="$TMON_INSTALL_DIR"
elif [ "$(id -u)" = 0 ]; then
	dest=/usr/local/bin
else
	dest="$HOME/.local/bin"
fi

# ---------------------------------------------------------------- download

if need curl; then
	fetch() { curl -fsSL -o "$2" "$1"; }
elif need wget; then
	fetch() { wget -qO "$2" "$1"; }
else
	die "neither curl nor wget is available"
fi

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t tmon)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "tmon: downloading $asset"
fetch "$base/$asset" "$tmp/$asset" ||
	die "could not download $base/$asset
Check that a release exists and that $os/$arch is one of its assets."

# ---------------------------------------------------------------- verify
#
# A silent corrupt download is worse than a loud failure, so the checksum is
# checked whenever the tools to do it exist. It is a warning rather than an
# error when they do not, because a minimal container may have neither.

if fetch "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
	if need sha256sum; then
		sum=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
	elif need shasum; then
		sum=$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)
	else
		sum=
		say "tmon: no sha256 tool found, skipping checksum verification"
	fi
	if [ -n "$sum" ]; then
		want=$(grep " [*]\{0,1\}$asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)
		[ -n "$want" ] || die "checksums.txt has no entry for $asset"
		[ "$sum" = "$want" ] || die "checksum mismatch for $asset
  expected $want
  got      $sum"
		say "tmon: checksum ok"
	fi
else
	say "tmon: checksums.txt unavailable, skipping verification"
fi

# ---------------------------------------------------------------- install

tar -xzf "$tmp/$asset" -C "$tmp" || die "could not extract $asset"
[ -f "$tmp/tmon" ] || die "archive did not contain a tmon binary"

mkdir -p "$dest" || die "could not create $dest"
chmod 0755 "$tmp/tmon"

# mv across filesystems fails on some systems; cp then rm always works.
cp "$tmp/tmon" "$dest/tmon.new" || die "could not write to $dest (try sudo, or set TMON_INSTALL_DIR)"
mv -f "$dest/tmon.new" "$dest/tmon"

# On macOS an unsigned binary carries a quarantine flag and Gatekeeper reports
# it as "damaged", which sends people looking for a corrupt download. Clearing
# it here is safe: this script fetched the file and just verified its checksum.
if [ "$os" = darwin ] && need xattr; then
	xattr -d com.apple.quarantine "$dest/tmon" 2>/dev/null || true
fi

say "tmon: installed to $dest/tmon"
"$dest/tmon" version || true

# ---------------------------------------------------------------- PATH advice

case ":$PATH:" in
*":$dest:"*) ;;
*)
	say ""
	say "$dest is not on your PATH. Add it:"
	say "  echo 'export PATH=\"$dest:\$PATH\"' >> ~/.bashrc && exec \$SHELL"
	;;
esac

say ""
say "Next: run 'tmon start' in a terminal you want recorded."
