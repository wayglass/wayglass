#!/bin/sh
# Desktop release builds: a signed and notarized universal Wayglass.app on
# macOS, a tar.gz on Linux, unsigned Windows ZIPs, and manual upload of a locally built macOS
# zip to a GitHub Release. Output goes to dist/desktop/.
#
#   sh scripts/desktop-release.sh mac            build, sign, notarize, staple, zip
#   sh scripts/desktop-release.sh linux          build and tar (run on Linux)
#   sh scripts/desktop-release.sh windows        cross-build unsigned x64/ARM64 ZIPs
#   sh scripts/desktop-release.sh upload vX.Y.Z  gh release upload of the mac zip
#
# Environment (never written to the repo or printed):
#   VERSION                  release version; default: git describe
#   DRY_RUN=1                print the signing, notarization and upload
#                            commands instead of running them
#   WAYGLASS_SIGN_IDENTITY   codesign identity; default: the one
#                            "Developer ID Application" in the keychain
#   WAYGLASS_NOTARY_PROFILE  notarytool keychain profile (default
#                            wayglass-notary, from 'xcrun notarytool
#                            store-credentials wayglass-notary')
#   WAYGLASS_NOTARY_KEY, WAYGLASS_NOTARY_KEY_ID, WAYGLASS_NOTARY_ISSUER
#                            an App Store Connect API key instead of the
#                            profile (CI): path to the .p8, its id, issuer id
#   SKIP_NOTARIZE=1          sign only (a build to run here, not to ship)
#   WAILS                    the Wails CLI; default: wails on PATH or in GOPATH
set -eu
cd "$(dirname "$0")/.."
root=$PWD
out=$root/dist/desktop
mode=${1:-}
version=${VERSION:-$(git describe --tags --always --dirty)}
case "$version" in *[!A-Za-z0-9._+-]*|'') echo 'Invalid version' >&2; exit 1 ;; esac
# CFBundleShortVersionString wants digits and dots: v0.2.0 -> 0.2.0, and an
# untagged v0.1.0-3-gabc123 -> 0.1.0.
short=$(printf '%s' "${version#v}" | sed -n 's/^\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p')
short=${short:-0.0.0}
wails=${WAILS:-$(command -v wails 2>/dev/null || echo "$(go env GOPATH)/bin/wails")}
dry=${DRY_RUN:-}

fail() { printf 'desktop-release: %s\n' "$*" >&2; exit 1; }
# run prints the command and, unless DRY_RUN is set, runs it. Only commands
# with side effects beyond dist/ go through it.
run() {
    printf '+'; printf ' %s' "$@"; printf '\n'
    if [ -z "$dry" ]; then "$@"; fi
}

build() {
    platform=$1; shift
    [ -x "$wails" ] || fail "Wails CLI not found; go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0"
    # Vet and test on the host. A Windows cross-build vets for Windows instead:
    # its tests cannot run here, and the CI Windows job runs them natively.
    case "$platform" in
        windows/*) (cd "$root/desktop" && CGO_ENABLED=0 GOOS=windows GOARCH="${platform#*/}" go vet ./...) ;;
        *) (cd "$root/desktop" && go vet ./... && go test ./...) ;;
    esac
    (cd "$root/desktop" && "$wails" build -clean -platform "$platform" -trimpath \
        -ldflags "-s -w -X main.version=$version" "$@")
}

checksums() {
    cd "$out"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$@" > desktop-checksums.txt
    else
        shasum -a 256 "$@" > desktop-checksums.txt
    fi
    cat desktop-checksums.txt
}

mac() {
    [ "$(uname -s)" = Darwin ] || fail 'the macOS build runs on macOS'
    . ./scripts/macos-signing.sh
    if [ -n "$dry" ] && [ -z "${WAYGLASS_SIGN_IDENTITY:-}" ]; then
        WAYGLASS_SIGN_IDENTITY='Developer ID Application: Dry Run (DRYRUN)'
    fi
    mac_signing_identity
    identity=$WAYGLASS_SIGN_IDENTITY
    team=$MAC_SIGN_TEAM
    echo "desktop-release: signing as $identity"

    build darwin/universal
    app=$root/desktop/build/bin/Wayglass.app
    [ -d "$app" ] || fail "no $app"
    plutil -replace CFBundleShortVersionString -string "$short" "$app/Contents/Info.plist"
    plutil -replace CFBundleVersion -string "$short" "$app/Contents/Info.plist"
    lipo -info "$app/Contents/MacOS/Wayglass" | grep -q 'x86_64 arm64' || fail 'not a universal binary'

    # One signature for the bundle: the only code inside is the main
    # executable (no frameworks, helpers or plug-ins), so no --deep.
    run codesign --force --options runtime --timestamp \
        --entitlements "$root/desktop/build/darwin/entitlements.plist" \
        --sign "$identity" "$app"
    if [ -z "$dry" ]; then
        codesign --verify --deep --strict --verbose=2 "$app"
        # The designated requirement is what TCC grants and Gatekeeper
        # approvals are keyed to. It must name the bundle id and the Team
        # ID, never a cdhash (which changes with every build).
        req=$(codesign -d -r- "$app" 2>&1 | sed -n 's/^designated => //p')
        echo "designated requirement: $req"
        case "$req" in *cdhash*) fail 'designated requirement pins this build instead of the app identity' ;; esac
        case "$req" in
            *'identifier "dev.wayglass.desktop"'*"subject.OU] = \"$team\""*|*'identifier "dev.wayglass.desktop"'*"subject.OU] = $team") ;;
            *) fail 'designated requirement does not pin the bundle id and Team ID; an update would need new permissions' ;;
        esac
    fi

    mkdir -p "$out"
    zip=$out/wayglass-desktop_darwin_universal.zip
    rm -f "$zip"
    # ditto keeps the bundle's resource forks and symlinks, which notarytool
    # and Gatekeeper need; zip(1) does not.
    ditto -c -k --keepParent "$app" "$zip"

    if [ -z "${SKIP_NOTARIZE:-}" ]; then
        if [ -n "$dry" ]; then
            echo "+ notarize $zip (must return Accepted)"
        else
            mac_notarize "$zip" "$out/notarization.json"
        fi
        # The ticket goes into the bundle, so the app opens offline; re-zip.
        run xcrun stapler staple "$app"
        if [ -z "$dry" ]; then
            rm -f "$zip"
            ditto -c -k --keepParent "$app" "$zip"
            xcrun stapler validate "$app"
            spctl --assess --type execute --verbose=2 "$app"
        fi
    else
        echo 'desktop-release: SKIP_NOTARIZE set; this build will not open on other Macs'
    fi
    checksums wayglass-desktop_darwin_universal.zip
    echo "desktop-release: $zip"
}

linux() {
    [ "$(uname -s)" = Linux ] || fail 'the Linux build runs on Linux'
    case $(uname -m) in aarch64|arm64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) fail 'Supported architectures: arm64 and amd64' ;; esac
    # Ubuntu 24.04 ships WebKitGTK 4.1 only; the tag picks it over 4.0.
    build "linux/$arch" -tags webkit2_41 -o wayglass-desktop
    bin=$root/desktop/build/bin/wayglass-desktop
    [ -x "$bin" ] || fail "no $bin"
    mkdir -p "$out"
    stage=$(mktemp -d)
    trap 'rm -rf "$stage"' EXIT HUP INT TERM
    cp "$bin" "$stage/wayglass-desktop"
    cp "$root/desktop/build/linux/dev.wayglass.desktop.desktop" "$stage/"
    cp "$root/desktop/build/appicon.png" "$stage/wayglass-desktop.png"
    cp "$root/LICENSE" "$stage/LICENSE"
    tar -czf "$out/wayglass-desktop_linux_$arch.tar.gz" -C "$stage" \
        wayglass-desktop dev.wayglass.desktop.desktop wayglass-desktop.png LICENSE
    checksums "wayglass-desktop_linux_$arch.tar.gz"
    echo "desktop-release: $out/wayglass-desktop_linux_$arch.tar.gz"
}

windows() {
    command -v zip >/dev/null 2>&1 || fail 'Windows packaging needs zip'
    mkdir -p "$out"
    stage=$(mktemp -d)
    trap 'rm -rf "$stage"' EXIT HUP INT TERM
    cp "$root/LICENSE" "$stage/LICENSE"
    for arch in amd64 arm64; do
        build "windows/$arch" -o wayglass-desktop.exe -webview2 browser
        cp "$root/desktop/build/bin/wayglass-desktop.exe" "$stage/wayglass-desktop.exe"
        archive="$out/wayglass-desktop_windows_$arch.zip"
        rm -f "$archive"
        (cd "$stage" && zip -q "$archive" wayglass-desktop.exe LICENSE)
    done
    checksums wayglass-desktop_windows_amd64.zip wayglass-desktop_windows_arm64.zip
}

# upload adds the locally built mac zip to an existing release and merges
# its checksum into the release's desktop-checksums.txt (the CI wrote the
# Linux and Windows lines).
upload() {
    tag=${1:-}
    case "$tag" in v[0-9]*) ;; *) fail 'usage: desktop-release.sh upload vX.Y.Z' ;; esac
    zip=$out/wayglass-desktop_darwin_universal.zip
    [ -f "$zip" ] || fail "no $zip; run 'sh scripts/desktop-release.sh mac' first"
    [ -f "$out/desktop-checksums.txt" ] || fail "no $out/desktop-checksums.txt"
    stage=$(mktemp -d)
    trap 'rm -rf "$stage"' EXIT HUP INT TERM
    if [ -z "$dry" ]; then
        gh release download "$tag" --pattern desktop-checksums.txt --dir "$stage" ||
            fail 'cannot read existing desktop checksums; refusing to replace them'
    fi
    { if [ -f "$stage/desktop-checksums.txt" ]; then grep -v ' wayglass-desktop_darwin_universal\.zip$' "$stage/desktop-checksums.txt"; fi
      grep ' wayglass-desktop_darwin_universal\.zip$' "$out/desktop-checksums.txt"; } > "$stage/merged.txt"
    cp "$stage/merged.txt" "$stage/desktop-checksums.txt"
    run gh release upload "$tag" "$zip" "$stage/desktop-checksums.txt" --clobber
}

case "$mode" in
    mac) mac ;;
    linux) linux ;;
    windows) windows ;;
    upload) upload "${2:-}" ;;
    *) echo 'usage: desktop-release.sh mac | linux | windows | upload vX.Y.Z' >&2; exit 2 ;;
esac
