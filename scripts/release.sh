#!/usr/bin/env bash
# Builds every release artifact into dist/, plus checksums.txt.
#
# Asset names carry no version. That is deliberate: it makes
#   https://github.com/Mengzhex/tmon/releases/latest/download/<name>
# always resolve to the newest release, so the install commands in the README
# never go stale and a Scoop or Homebrew manifest only has to change its hash.
# The tag is still stamped into the binary through -ldflags, and `tmon version`
# reports it.
#
# Usage: scripts/release.sh [VERSION]
# Without an argument the version comes from `git describe`.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"

# Go lives in a conda env on the development machine and on PATH everywhere
# else; goenv.sh resolves that and exports nothing machine-specific when the
# conda prefix is absent.
if [ -f scripts/goenv.sh ]; then
	# shellcheck source=/dev/null
	. scripts/goenv.sh
fi
if ! "${GO:-}" version >/dev/null 2>&1; then
	# Belt and braces for an inherited environment: a GOROOT naming a
	# toolchain that is not on this machine breaks the one that is, with
	# "cannot find GOROOT directory" and exit 2, so drop it before falling
	# back to PATH.
	unset GOROOT
	GO=go
fi
"$GO" version >/dev/null 2>&1 || {
	echo "release: no usable Go toolchain" >&2
	exit 1
}

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X main.buildVersion=$VERSION"

# Static binaries on every target, regardless of the build host.
#
# Cross-compiling implies CGO_ENABLED=0, so building on Windows produced static
# Linux binaries and the README's "runs on Alpine too" was true. Building the
# same target natively on a Linux runner does not: a C toolchain is present, so
# cgo is on by default and net/os-user link against glibc. v0.1.0 shipped a
# Linux binary with PT_INTERP=/lib64/ld-linux-x86-64.so.2, which dies on musl
# with the famously unhelpful "no such file or directory" -- naming the missing
# interpreter, not the binary. Nothing here needs cgo.
export CGO_ENABLED=0

DIST="$ROOT/dist"
STAGE="$DIST/.stage"
rm -rf "$DIST"
mkdir -p "$STAGE"

# archive_zip and archive_tgz exist because the tooling differs by machine:
# a CI runner has zip(1), this Windows box does not but ships bsdtar, which
# libarchive lets write zip files. Both are checked rather than assumed.
archive_zip() { # $1=out  $2=dir  $3=file
	if command -v zip >/dev/null 2>&1; then
		(cd "$2" && zip -q -X "$1" "$3")
	elif tar --version 2>&1 | grep -qi bsdtar; then
		tar -a -c -f "$1" -C "$2" "$3"
	elif [ -x /c/Windows/System32/tar.exe ]; then
		/c/Windows/System32/tar.exe -a -c -f "$1" -C "$2" "$3"
	else
		echo "release: no tool available to write $1" >&2
		exit 1
	fi
}

archive_tgz() { # $1=out  $2=dir  $3=file
	# The executable bit has to be written into the archive rather than set on
	# the file: chmod is a no-op on NTFS through MSYS, so building on Windows
	# otherwise produces a tarball that extracts 0644 and gives "permission
	# denied" on the server. GNU tar can override the stored mode; bsdtar
	# cannot, but bsdtar here means macOS, where the chmod above did work.
	if tar --version 2>&1 | grep -qi 'GNU tar'; then
		tar --mode=0755 -c -z -f "$1" -C "$2" "$3"
	else
		tar -c -z -f "$1" -C "$2" "$3"
	fi
}

echo "tmon $VERSION"

for target in windows/amd64 windows/arm64 darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
	os="${target%/*}"
	arch="${target#*/}"

	bin=tmon
	[ "$os" = windows ] && bin=tmon.exe

	rm -f "$STAGE/$bin"
	GOOS="$os" GOARCH="$arch" "$GO" build -trimpath -ldflags "$LDFLAGS" \
		-o "$STAGE/$bin" ./cmd/tmon

	# Force the executable bit into the archive. Go writes 0755 when building
	# on Linux, but building on Windows produces a file that tars as 0644, and
	# extracting that on a server gives "permission denied" from a release that
	# looked fine to whoever cut it.
	chmod 0755 "$STAGE/$bin"

	# Binary-only archives. Anything else would land in the user's ~/bin
	# alongside it, because the README's install commands extract straight
	# into a directory on PATH.
	if [ "$os" = windows ]; then
		out="tmon_${os}_${arch}.zip"
		archive_zip "$DIST/$out" "$STAGE" "$bin"
	else
		out="tmon_${os}_${arch}.tar.gz"
		archive_tgz "$DIST/$out" "$STAGE" "$bin"
	fi
	printf '  %-28s %s\n' "$out" "$(du -h "$DIST/$out" | cut -f1)"
	rm -f "$STAGE/$bin"
done

# The probe ships as a bare binary rather than an archive: it gets copied to a
# target host with scp, so an archive would only add a step. sshd invokes it
# under the name tmon-probe and the binary switches to probe mode based on the
# name it was called as, which is why it is the same program.
for arch in amd64 arm64; do
	out="tmon-probe-linux-$arch"
	GOOS=linux GOARCH="$arch" "$GO" build -trimpath -ldflags "$LDFLAGS" \
		-o "$DIST/$out" ./cmd/tmon
	printf '  %-28s %s\n' "$out" "$(du -h "$DIST/$out" | cut -f1)"
done

rmdir "$STAGE"

# The installers ship as release assets so that the one-liner in the README is
# a single URL. They name this repository directly, which keeps them runnable
# straight from a clone; the slug is rewritten here when it differs, taken from
# $GITHUB_REPOSITORY under Actions and from the origin remote otherwise, so a
# fork publishes an installer pointing at the fork with nothing to remember.
DEFAULT_SLUG="Mengzhex/tmon"

slug="${GITHUB_REPOSITORY:-}"
if [ -z "$slug" ]; then
	origin=$(git remote get-url origin 2>/dev/null || true)
	case "$origin" in
	*github.com[:/]*)
		slug=${origin#*github.com}
		slug=${slug#[:/]}
		slug=${slug%.git}
		;;
	esac
fi
for f in install.sh install.ps1; do
	if [ -n "$slug" ] && [ "$slug" != "$DEFAULT_SLUG" ]; then
		sed "s|$DEFAULT_SLUG|$slug|g" "scripts/$f" >"$DIST/$f"
		echo "  note: $f retargeted to $slug" >&2
	else
		cp "scripts/$f" "$DIST/$f"
	fi
	chmod 0755 "$DIST/$f"
	printf '  %-28s
' "$f"
done

# checksums.txt is what `sha256sum -c` and the package manifests both read.
(
	cd "$DIST"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum -- * >checksums.txt
	else
		shasum -a 256 -- * >checksums.txt
	fi
	# The file lists itself as of the moment before it existed; drop that line.
	grep -v ' checksums.txt$' checksums.txt >checksums.tmp || true
	mv checksums.tmp checksums.txt
)

printf '  %-28s\n' checksums.txt
echo
echo "dist/ is ready. Publish with:"
echo "  gh release create $VERSION dist/* --generate-notes"
