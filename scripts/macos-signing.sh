#!/bin/sh
# Shared Developer ID and notarization checks. Source from the repo root.

mac_signing_identity() {
    [ "$(uname -s)" = Darwin ] || { echo 'Signing requires macOS' >&2; return 1; }
    if [ -z "${WAYGLASS_SIGN_IDENTITY:-}" ]; then
        WAYGLASS_SIGN_IDENTITY=$(security find-identity -v -p codesigning |
            sed -n 's/.*"\(Developer ID Application: [^"]*\)".*/\1/p')
    fi
    # Require one full certificate name, including its team. Do not guess
    # between teams or accept an Apple Development / ad-hoc identity.
    MAC_SIGN_TEAM=$(printf '%s\n' "$WAYGLASS_SIGN_IDENTITY" |
        sed -n 's/^Developer ID Application: .* (\([A-Z0-9]*\))$/\1/p')
    case "$MAC_SIGN_TEAM" in
        ''|*[!A-Z0-9]*)
            echo 'Set WAYGLASS_SIGN_IDENTITY to one full Developer ID Application identity from security find-identity -v -p codesigning' >&2
            return 1 ;;
    esac
}

mac_notarize() {
    # Keep the result (including submission ID) for diagnosing a rejection.
    # notarytool can exit successfully even when the submission is Invalid.
    printf 'Submitting for notarization; result will be saved to %s\n' "$2"
    if [ -n "${WAYGLASS_NOTARY_KEY:-}" ]; then
        xcrun notarytool submit "$1" --wait --timeout 30m --output-format json \
            --key "$WAYGLASS_NOTARY_KEY" --key-id "${WAYGLASS_NOTARY_KEY_ID:?}" \
            --issuer "${WAYGLASS_NOTARY_ISSUER:?}" > "$2" || return 1
    else
        xcrun notarytool submit "$1" --wait --timeout 30m --output-format json \
            --keychain-profile "${WAYGLASS_NOTARY_PROFILE:-wayglass-notary}" > "$2" || return 1
    fi
    cat "$2"
    if [ "$(plutil -extract status raw -o - "$2")" != Accepted ]; then
        echo "Notarization was not Accepted; see $2" >&2
        return 1
    fi
}
