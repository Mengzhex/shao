#!/usr/bin/env bash
# Builds every release artifact into dist/, plus checksums.txt.
#
# Asset names carry no version. That is deliberate: it makes
#   https://github.com/OWNER/tmon/releases/latest/download/<name>
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
# else. Sourcing goenv.sh is harmless when that path does not exist, since the
# result is checked before use.
if [ -f scripts/goenv.sh ]; then
	# shellcheck source=/dev/null
	. scripts/goenv.sh
fi
if ! "${GO:-}" version >/dev/null 2>&1; then
	GO=go
fi
"$GO" version >/dev/null 2>&1 || {
	echo "release: no usable Go toolchain" >&2
	exit 1
}

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X main.buildVersion=$VERSION"

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
	tar -c -z -f "$1" -C "$2" "$3"
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
# a single URL. Both carry OWNER/tmon as a placeholder in the repository; the
# real slug is stamped in here, from $GITHUB_REPOSITORY when a workflow is
# running and from the origin remote otherwise. That way nothing has to be
# hand-edited at release time and a fork's installer points at the fork.
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
	if [ -n "$slug" ]; then
		sed "s|OWNER/tmon|$slug|g" "scripts/$f" >"$DIST/$f"
	else
		cp "scripts/$f" "$DIST/$f"
		echo "  note: no GitHub slug found, $f keeps the OWNER placeholder" >&2
	fi
	chmod 0755 "$DIST/$f"
	printf '  %-28s\n' "$f"
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
