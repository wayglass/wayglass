#!/bin/sh
# PREFIX is the destination directory itself (default: ~/.local/bin).
set -eu
fail() { printf 'wayglass: %s\n' "$*" >&2; exit 1; }
for tool in curl tar awk mktemp; do
    command -v "$tool" >/dev/null 2>&1 || fail "Required command not found: $tool"
done
case $(uname -s) in Darwin) os=darwin ;; Linux) os=linux ;; *) fail 'Supported systems: macOS and Linux' ;; esac
case $(uname -m) in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) fail 'Supported architectures: arm64 and amd64' ;; esac
if command -v sha256sum >/dev/null 2>&1; then
    checksum() { sha256sum "$1"; }
elif command -v shasum >/dev/null 2>&1; then
    checksum() { shasum -a 256 "$1"; }
else
    fail 'Install sha256sum or shasum to verify the download'
fi
base=${WAYGLASS_INSTALL_BASE:-https://github.com/wayglass/wayglass/releases/latest/download}
# A release download over HTTPS stays on HTTPS through every redirect
# (GitHub redirects to its object store); redirecting to plain HTTP is refused.
redirects=
case "$base" in
    https://*) redirects='--proto-redir =https' ;;
    http://localhost:*|http://127.0.0.1:*|file://*) ;;
    *) fail 'Download base must use HTTPS (or local HTTP/file for testing)' ;;
esac
archive=wayglass_${os}_${arch}.tar.gz
prefix=${PREFIX:-"$HOME/.local/bin"}
stage=$(mktemp -d)
pending=
trap 'rm -rf "$stage"; if [ -n "$pending" ]; then rm -f "$pending"; fi' EXIT HUP INT TERM
# $redirects is unquoted on purpose: it is two words or none.
# shellcheck disable=SC2086
curl -fLsS --retry 3 $redirects "${base%/}/$archive" -o "$stage/$archive"
# shellcheck disable=SC2086
curl -fLsS --retry 3 $redirects "${base%/}/checksums.txt" -o "$stage/checksums.txt"
expected=$(awk -v name="$archive" '$2 == name {print $1}' "$stage/checksums.txt")
[ "${#expected}" -eq 64 ] || fail "Missing or ambiguous checksum for $archive"
case "$expected" in *[!0-9a-f]*) fail 'Invalid checksum' ;; esac
actual=$(checksum "$stage/$archive" | awk '{print $1}')
[ "$actual" = "$expected" ] || fail "Checksum mismatch for $archive"
# Extract only the binary, never paths supplied by the archive.
tar -xOzf "$stage/$archive" wayglass > "$stage/wayglass"
[ -s "$stage/wayglass" ] || fail 'Archive contains an empty binary'
mkdir -p "$prefix"
pending=$(mktemp "$prefix/.wayglass.XXXXXX")
cat "$stage/wayglass" > "$pending"
chmod 755 "$pending"
mv -f "$pending" "$prefix/wayglass"
pending=
printf 'Installed %s/wayglass\n' "$prefix"
case ":$PATH:" in *":$prefix:"*) ;; *) printf 'Add %s to your PATH.\n' "$prefix" ;; esac
printf 'Next: wayglass login, then wayglass doctor\n'
