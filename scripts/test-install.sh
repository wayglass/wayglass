#!/bin/sh
# Uses real release archives; never contacts GitHub or modifies the user's bin.
set -eu
cd "$(dirname "$0")/.."
repo=$(pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
mkdir "$stage/assets"
cp dist/*.tar.gz dist/checksums.txt "$stage/assets/"
export WAYGLASS_INSTALL_BASE="file://$stage/assets"
export PREFIX="$stage/bin with spaces"
sh scripts/install.sh
"$PREFIX/wayglass" version --json
cp "$PREFIX/wayglass" "$stage/original"
# A corrupted archive must fail without replacing a working installation.
for archive in "$stage"/assets/*.tar.gz; do printf 'corrupt' >> "$archive"; done
if sh scripts/install.sh; then echo 'Accepted corrupt archive' >&2; exit 1; fi
cmp "$PREFIX/wayglass" "$stage/original"
cp "$repo"/dist/*.tar.gz "$stage/assets/"
printf '' > "$stage/assets/checksums.txt"
if sh scripts/install.sh; then echo 'Accepted missing checksum' >&2; exit 1; fi
cmp "$PREFIX/wayglass" "$stage/original"
# A checksums file that names the archive twice is ambiguous, not a match.
cp "$repo/dist/checksums.txt" "$stage/assets/checksums.txt"
cat "$repo/dist/checksums.txt" >> "$stage/assets/checksums.txt"
if sh scripts/install.sh; then echo 'Accepted an ambiguous checksum' >&2; exit 1; fi
cmp "$PREFIX/wayglass" "$stage/original"
# A plain-HTTP base is refused before anything is downloaded.
if WAYGLASS_INSTALL_BASE=http://example.invalid/releases sh scripts/install.sh; then echo 'Accepted an HTTP base' >&2; exit 1; fi
# Every refused run cleans up after itself: only the working binary stays in PREFIX.
if [ "$(ls -A "$PREFIX")" != wayglass ]; then echo "Leftovers in PREFIX: $(ls -A "$PREFIX")" >&2; exit 1; fi
echo 'Installer checks passed'
