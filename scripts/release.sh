#!/bin/sh
# Shared packaging path for tagged releases and local dry runs.
# SIGN_MAC=1 signs and notarizes the Darwin binaries on macOS before packaging.
# See macos-signing.sh for Keychain profile / App Store Connect API key options.
set -eu
cd "$(dirname "$0")/.."
version=${VERSION:-$(git describe --tags --always --dirty)}
commit=${COMMIT:-$(git rev-parse --short HEAD)}
case "$version:$commit" in *[!A-Za-z0-9._:+-]*) echo 'Invalid version or commit' >&2; exit 1 ;; esac
case "${SIGN_MAC:-0}" in
    0) ;;
    1)
        . ./scripts/macos-signing.sh
        mac_signing_identity
        ;;
    *) echo 'SIGN_MAC must be 0 or 1' >&2; exit 1 ;;
esac
mkdir -p dist
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64 windows/arm64 windows/amd64; do
    os=${target%/*}; arch=${target#*/}
    binary=wayglass
    if [ "$os" = windows ]; then binary=wayglass.exe; fi
    mkdir -p "$stage/${os}_${arch}"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X main.version=$version -X main.commit=$commit" \
        -o "$stage/${os}_${arch}/$binary" ./cmd/wayglass
    cp LICENSE "$stage/${os}_${arch}/LICENSE"
    if [ "${SIGN_MAC:-0}" = 1 ] && [ "$os" = darwin ]; then
        codesign --force --options runtime --timestamp --identifier dev.wayglass.cli \
            --sign "$WAYGLASS_SIGN_IDENTITY" "$stage/${os}_${arch}/wayglass"
        codesign --verify --strict --verbose=2 "$stage/${os}_${arch}/wayglass"
    fi
done
if [ "${SIGN_MAC:-0}" = 1 ]; then
    # One submission for both architectures. Bare Mach-O files cannot be
    # stapled; Gatekeeper retrieves their notarization tickets online.
    mkdir "$stage/notary"
    cp -R "$stage/darwin_arm64" "$stage/darwin_amd64" "$stage/notary/"
    ditto -c -k --keepParent "$stage/notary" "$stage/cli-notary.zip"
    mac_notarize "$stage/cli-notary.zip" "$PWD/dist/cli-notarization.json"
    for arch in arm64 amd64; do
        codesign --verify --strict --check-notarization --verbose=2 "$stage/darwin_$arch/wayglass"
    done
fi
# Publish archives only after every requested signature and notarization passed.
for target in darwin_arm64 darwin_amd64 linux_arm64 linux_amd64; do
    tar -czf "dist/wayglass_${target}.tar.gz" -C "$stage/$target" wayglass LICENSE
done
for target in windows_arm64 windows_amd64; do
    # zip updates existing archives; remove the old one so no stale entries survive.
    archive="$PWD/dist/wayglass_${target}.zip"
    rm -f "$archive"
    (cd "$stage/$target" && zip -q "$archive" wayglass.exe LICENSE)
done
cd dist
if command -v sha256sum >/dev/null 2>&1; then
    sha256sum wayglass_*.tar.gz wayglass_windows_*.zip > checksums.txt
else
    shasum -a 256 wayglass_*.tar.gz wayglass_windows_*.zip > checksums.txt
fi
