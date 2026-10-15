#!/bin/sh
# Failure-path checks with fake Apple tools; no credentials or network.
set -eu
cd "$(dirname "$0")/.."
. ./scripts/macos-signing.sh
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
uname() { echo Darwin; }
security() { printf '%s\n' "$TEST_IDENTITIES"; }
xcrun() { printf '{"status":"%s"}\n' "$TEST_STATUS"; }
plutil() { printf '%s\n' "$TEST_STATUS"; }
TEST_IDENTITIES='1) EXAMPLE "Apple Development: Example (TESTTEAM)"'
if (unset WAYGLASS_SIGN_IDENTITY; mac_signing_identity); then
    echo 'Accepted an Apple Development identity' >&2; exit 1
fi
TEST_IDENTITIES='1) EXAMPLE "Developer ID Application: Example (TESTTEAM)"'
unset WAYGLASS_SIGN_IDENTITY
mac_signing_identity
[ "$MAC_SIGN_TEAM" = TESTTEAM ]
TEST_IDENTITIES="$TEST_IDENTITIES
2) EXAMPLE \"Developer ID Application: Another (OTHERTEAM)\""
if (unset WAYGLASS_SIGN_IDENTITY; mac_signing_identity); then
    echo 'Chose an ambiguous team' >&2; exit 1
fi
for identity in '-' 'Apple Distribution: Example (TESTTEAM)' 'Apple Development: Example (TESTTEAM)'; do
    if (WAYGLASS_SIGN_IDENTITY=$identity; mac_signing_identity); then
        echo 'Accepted an unsupported identity override' >&2; exit 1
    fi
done
unset WAYGLASS_NOTARY_KEY
for TEST_STATUS in Invalid Rejected 'In Progress' ''; do
    if mac_notarize "$stage/example.zip" "$stage/result.json"; then
        echo 'Accepted unsuccessful notarization' >&2; exit 1
    fi
done
TEST_STATUS=Accepted
mac_notarize "$stage/example.zip" "$stage/result.json"
xcrun() { return 69; }
if mac_notarize "$stage/example.zip" "$stage/result.json"; then
    echo 'Accepted failed notarization command' >&2; exit 1
fi
# Exercise the packaging boundary with fake compiler and Apple tools, but
# real tar/checksums. A rejected submission must not replace existing assets.
mkdir -p "$stage/repo/scripts" "$stage/repo/dist" "$stage/tools"
cp scripts/release.sh scripts/macos-signing.sh "$stage/repo/scripts/"
cp LICENSE "$stage/repo/"
cat > "$stage/tools/go" <<'STUB'
#!/bin/sh
while [ "$#" -gt 0 ]; do
    if [ "$1" = -o ]; then printf 'unsigned\n' > "$2"; exit 0; fi
    shift
done
exit 1
STUB
cat > "$stage/tools/codesign" <<'STUB'
#!/bin/sh
case "$1" in
    --force) for target do :; done; printf 'signed\n' >> "$target" ;;
    --verify) exit 0 ;;
    *) exit 1 ;;
esac
STUB
cat > "$stage/tools/uname" <<'STUB'
#!/bin/sh
echo Darwin
STUB
cat > "$stage/tools/security" <<'STUB'
#!/bin/sh
echo '1) EXAMPLE "Developer ID Application: Example (TESTTEAM)"'
STUB
cat > "$stage/tools/ditto" <<'STUB'
#!/bin/sh
for target do :; done
: > "$target"
STUB
cat > "$stage/tools/xcrun" <<'STUB'
#!/bin/sh
printf '{"status":"%s"}\n' "$TEST_STATUS"
STUB
cat > "$stage/tools/plutil" <<'STUB'
#!/bin/sh
printf '%s\n' "$TEST_STATUS"
STUB
chmod +x "$stage"/tools/*
unset WAYGLASS_SIGN_IDENTITY
printf 'previous archive\n' > "$stage/repo/dist/wayglass_darwin_arm64.tar.gz"
printf 'previous checksums\n' > "$stage/repo/dist/checksums.txt"
cp "$stage/repo/dist/wayglass_darwin_arm64.tar.gz" "$stage/previous-archive"
cp "$stage/repo/dist/checksums.txt" "$stage/previous-checksums"
if PATH="$stage/tools:$PATH" TEST_STATUS=Invalid SIGN_MAC=1 VERSION=v0.0.0 COMMIT=test \
    sh "$stage/repo/scripts/release.sh"; then
    echo 'Packaged a rejected submission' >&2; exit 1
fi
cmp "$stage/previous-archive" "$stage/repo/dist/wayglass_darwin_arm64.tar.gz"
cmp "$stage/previous-checksums" "$stage/repo/dist/checksums.txt"
PATH="$stage/tools:$PATH" TEST_STATUS=Accepted SIGN_MAC=1 VERSION=v0.0.0 COMMIT=test \
    sh "$stage/repo/scripts/release.sh"
for arch in arm64 amd64; do
    tar -xOf "$stage/repo/dist/wayglass_darwin_$arch.tar.gz" wayglass | grep -qx signed
    if tar -xOf "$stage/repo/dist/wayglass_linux_$arch.tar.gz" wayglass | grep -qx signed; then
        echo 'Signed a Linux binary' >&2; exit 1
    fi
done
(cd "$stage/repo/dist" && shasum -a 256 -c checksums.txt)
echo 'macOS signing and packaging checks passed (stubbed tools)' 
