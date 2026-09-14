#!/bin/sh
# shao installer for Linux and macOS.
#
#   curl -fsSL https://github.com/Mengzhex/shao/releases/latest/download/install.sh | sh
#
# Environment:
#   SHAO_INSTALL_DIR   where to put the binary. Default: ~/.local/bin, or
#                      /usr/local/bin when running as root.
#   SHAO_VERSION       a tag such as v0.1.0. Default: the latest release.
#   SHAO_REPO          owner/name, if you forked it.
#
# POSIX sh on purpose: a server may not have bash, and the whole point of this
# script is that it runs on a machine you have not prepared.
set -eu

REPO="${SHAO_REPO:-Mengzhex/shao}"
VERSION="${SHAO_VERSION:-latest}"

say() { printf '%s\n' "$*"; }
die() {
	printf 'shao install: %s\n' "$*" >&2
	exit 1
}

need() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- platform

os=$(uname -s)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported operating system: $os (shao builds for Linux, macOS and Windows)" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture: $arch (shao builds for amd64 and arm64)" ;;
esac

asset="shao_${os}_${arch}.tar.gz"

if [ "$VERSION" = latest ]; then
	base="https://github.com/$REPO/releases/latest/download"
else
	base="https://github.com/$REPO/releases/download/$VERSION"
fi

# ---------------------------------------------------------------- destination

if [ -n "${SHAO_INSTALL_DIR:-}" ]; then
	dest="$SHAO_INSTALL_DIR"
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

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t shao)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "shao: downloading $asset"
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
		say "shao: no sha256 tool found, skipping checksum verification"
	fi
	if [ -n "$sum" ]; then
		want=$(grep " [*]\{0,1\}$asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)
		[ -n "$want" ] || die "checksums.txt has no entry for $asset"
		[ "$sum" = "$want" ] || die "checksum mismatch for $asset
  expected $want
  got      $sum"
		say "shao: checksum ok"
	fi
else
	say "shao: checksums.txt unavailable, skipping verification"
fi

# ---------------------------------------------------------------- install

tar -xzf "$tmp/$asset" -C "$tmp" || die "could not extract $asset"
[ -f "$tmp/shao" ] || die "archive did not contain a shao binary"

mkdir -p "$dest" || die "could not create $dest"
chmod 0755 "$tmp/shao"

# mv across filesystems fails on some systems; cp then rm always works.
cp "$tmp/shao" "$dest/shao.new" || die "could not write to $dest (try sudo, or set SHAO_INSTALL_DIR)"
mv -f "$dest/shao.new" "$dest/shao"

# On macOS an unsigned binary carries a quarantine flag and Gatekeeper reports
# it as "damaged", which sends people looking for a corrupt download. Clearing
# it here is safe: this script fetched the file and just verified its checksum.
if [ "$os" = darwin ] && need xattr; then
	xattr -d com.apple.quarantine "$dest/shao" 2>/dev/null || true
fi

say "shao: installed to $dest/shao"
"$dest/shao" version || true

# ------------------------------------------------------ PATH, and shadowing
#
# Two different problems look identical from the prompt: the directory is not
# on PATH, or it is but another program of the same name comes first. The
# second used to be routine for this tool under its old name, which collided
# with a program shipped by the distribution, and the symptom was an error
# message from that other program being read as this one refusing to start.
# The name no longer collides, but the check is cheap and the failure is
# otherwise very hard to recognise.

found=$(command -v shao 2>/dev/null || true)
case ":$PATH:" in
*":$dest:"*)
	if [ -n "$found" ] && [ "$found" != "$dest/shao" ]; then
		say ""
		say "Warning: a different program named shao is already on your PATH:"
		say "  $found"
		say "It comes before $dest, so typing 'shao' runs that one. Either call"
		say "this one by path:"
		say "  $dest/shao start"
		say "or put $dest first:"
		say "  echo 'export PATH=\"$dest:\$PATH\"' >> ~/.bashrc && exec \$SHELL"
	fi
	;;
*)
	say ""
	say "$dest is not on your PATH. Add it:"
	say "  echo 'export PATH=\"$dest:\$PATH\"' >> ~/.bashrc && exec \$SHELL"
	if [ -n "$found" ]; then
		say ""
		say "Note: '$found' is a different program that happens to share the name."
	fi
	;;
esac

say ""
say "Next: run 'shao start' in a terminal you want recorded."
