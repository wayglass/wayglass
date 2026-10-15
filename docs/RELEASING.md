# Release builds

Build the CLI archives locally with `make release-dry-run`. Output goes to
`dist/`: macOS and Linux tarballs and unsigned Windows ZIPs, each for arm64
and amd64, plus `checksums.txt`. Packaging needs `tar` and `zip`.
Each archive contains `wayglass` (`wayglass.exe` on Windows) and its MIT licence. CGO is disabled.
`VERSION` and `COMMIT` can override the embedded build information.

Test installation without a published release:

```sh
make release-dry-run
sh scripts/test-install.sh
WAYGLASS_INSTALL_BASE="file://$PWD/dist" sh scripts/install.sh
wayglass version --json
```

`PREFIX` overrides the destination directory (default `~/.local/bin`), for
example `PREFIX="$HOME/bin" sh scripts/install.sh`. The installer needs curl,
tar, and either sha256sum or shasum. It verifies the archive before replacing
an existing binary. Git is required to use Wayglass.

After the repository is public and a release is published, install with:

```sh
curl -fsSL https://raw.githubusercontent.com/wayglass/wayglass/main/scripts/install.sh | sh
```

Then run `wayglass login` and `wayglass doctor`. Public downloads will fail
while the repository is private or has no release; use the local override above.
To select a published version, set `WAYGLASS_INSTALL_BASE` to
`https://github.com/wayglass/wayglass/releases/download/vX.Y.Z`.

The release workflow runs when a maintainer pushes a `v*` tag. It tests the CLI,
builds archives, exercises the installer, and publishes assets with generated
release notes. If macOS signing secrets are missing, it creates a draft until
the signed macOS app and CLI are uploaded; it does not publish an incomplete
set of apps. A dry run creates neither a tag nor a GitHub Release.

Windows users download and extract ZIPs directly; the shell installer is for
macOS and Linux. See [Windows desktop setup and source builds](../desktop/README.md#windows).

### Signed macOS CLI archives

On macOS, with a Developer ID Application identity and the notarization
profile described below, run:

```sh
make release-sign-mac VERSION=vX.Y.Z
sh scripts/test-install.sh
```

This builds the CLI archives for every platform. Both Darwin binaries are signed with
hardened runtime and a secure timestamp, then submitted together to Apple's
notary service. Packaging proceeds only after an `Accepted` result;
`dist/cli-notarization.json` records the submission ID and status. Checksums
are computed after signing. Linux binaries are unchanged by signing.

A plain Mach-O executable cannot carry a stapled ticket, so Gatekeeper needs
network access to retrieve its notarization ticket. This follows Apple's
[notarization workflow](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution).

Verify an extracted signed CLI binary with:

```sh
codesign --verify --strict --check-notarization --verbose=2 ./wayglass
./wayglass version --json
```

Use `spctl --assess --type execute` for the desktop `.app`. On the standalone
CLI it reports “the code is valid but does not seem to be an app”; use the
`codesign` notarization check above instead.

`make release-dry-run` still creates unsigned development archives without
Apple credentials. When the CI signing secrets are present, the release
workflow replaces its cross-compiled Darwin archives with signed, notarized
ones before calculating the published checksums. Without those secrets, it
keeps the release in draft. For a manual signed CLI release, build at the
release tag and upload the CLI archives and their `checksums.txt` together.

Older unsigned releases and dry-run archives downloaded in a browser may
show an unverified-developer warning. After checking the source and checksum,
use `xattr -d com.apple.quarantine ./wayglass` or Open Anyway in System
Settings → Privacy & Security for those builds. Signed, notarized releases
do not need this workaround. The curl installer sets no quarantine mark.

## Desktop

The same tag also builds the desktop app (`desktop/`, Wails v2; the CLI
version is in the Makefile). `.github/workflows/release.yml` uses these jobs (plus a signing-secret check):

- `cli`: the archives above.
- `desktop-windows` (Ubuntu runner, Wails cross-compilation):
  `wayglass-desktop_windows_amd64.zip` and
  `wayglass-desktop_windows_arm64.zip`, each with `wayglass-desktop.exe` and the
  licence. These are unsigned; no Windows signing credentials are needed.
- `desktop-linux` (Ubuntu 24.04, GTK 3 + WebKitGTK 4.1):
  `wayglass-desktop_linux_amd64.tar.gz` with the binary, a `.desktop` entry,
  the icon and the licence. arm64 waits for a Linux arm64 runner the org can
  use; it is one more matrix entry in `scripts/desktop-release.sh linux`.
- `desktop-mac` (macOS runner): signs and notarizes the Darwin CLI archives
  and builds a universal `Wayglass.app`, Developer ID
  signed, hardened runtime, notarized and stapled, as
  `wayglass-desktop_darwin_universal.zip`. It runs only when these
  repository secrets exist, and is skipped (not failed) otherwise:
  `MACOS_CERT_P12` (the Developer ID Application certificate and key,
  base64 of the .p12), `MACOS_CERT_PASSWORD`, `MACOS_NOTARY_KEY` (contents of
  the App Store Connect API key .p8), `MACOS_NOTARY_KEY_ID`,
  `MACOS_NOTARY_ISSUER`. The certificate lives in a keychain created for
  the job and deleted after it.
- `publish`: creates the release with the CLI archives, `checksums.txt`, the
  desktop archives and `desktop-checksums.txt`. Windows and Linux builds must
  succeed. If macOS signing was skipped, the release remains a draft for the
  local signed upload below.

Every desktop package is built with `sh scripts/desktop-release.sh mac|linux|windows`
(`make desktop-release-mac`, `make desktop-release-linux`,
`make desktop-release-windows`); CI and a laptop run the
same script. Output: `dist/desktop/`. `DRY_RUN=1` builds but only prints the
signing, notarization and upload commands. The Windows packaging mode runs
on macOS or Linux with the Wails CLI and `zip`; native PowerShell source-build
instructions are in [desktop/README.md](../desktop/README.md#windows).

### Signed macOS build on the maintainer's Mac

Until the secrets above exist (or whenever CI's mac job is skipped), the mac
build is made locally and uploaded by hand. Once, store the notarization
credentials in the keychain (an App Store Connect API key, or an Apple ID
with an app-specific password; nothing is written to the repo):

```sh
xcrun notarytool store-credentials wayglass-notary
```

Then, per release, check out the release tag and wait for its workflow to
create the draft. Build and upload the signed app and CLI:

```sh
make desktop-release-mac VERSION=vX.Y.Z      # build, sign, notarize, staple, zip
sh scripts/desktop-release.sh upload vX.Y.Z  # gh release upload; merges desktop-checksums.txt
make release-sign-mac VERSION=vX.Y.Z
gh release upload vX.Y.Z dist/wayglass_*.tar.gz dist/wayglass_windows_*.zip dist/checksums.txt --clobber
```

Before publishing the draft, verify its assets include the macOS, Linux and
Windows CLI archives, the universal macOS app, Linux desktop tarball, Windows
desktop ZIPs, and both checksum files. Then publish it:

```sh
gh release edit vX.Y.Z --draft=false
```

The script requires one "Developer ID Application" identity in the
keychain (`security find-identity -v -p codesigning`), or
`WAYGLASS_SIGN_IDENTITY` names it. It refuses an ad-hoc or Apple Development
signature and checks that the designated requirement names
`dev.wayglass.desktop` and the Team ID: that pairing is what lets a new
version keep the folder and notification permissions the old one was given
(`desktop/README.md`, "Signing"). `SKIP_NOTARIZE=1` makes a signed build for
this Mac only.

### Local verification (October 14, 2026)

At code revision `625f722`, Apple accepted the `v0.0.0-signtest` desktop
and CLI submissions. The universal app passed strict signature verification,
`stapler validate`, and `spctl` with `source=Notarized Developer ID`.
A Safari-quarantined copy opened under App Translocation without a Gatekeeper
prompt. Both CLI architectures passed `codesign --check-notarization`; the
quarantined arm64 binary ran and the installer tests passed against the signed
archives. The installed CLI and desktop app passed the same signature checks.

Desktop versions `v0.0.0-signtest` and `v0.0.1-signtest` have identical
designated requirements. That checks signing identity continuity; an upgrade
with existing TCC or notification grants was not exercised. These are local
test builds, not published releases. CI signing secrets remain unset.

### Configure signing in GitHub Actions

A maintainer performs these steps; agents must not export identities or create keys.

1. In Keychain Access → login → My Certificates, select the **Developer ID
   Application** identity with its private key. Choose Export and save it as
   a password-protected `.p12` outside the repository. See Apple's
   [signing identity export guide](https://developer.apple.com/documentation/Xcode/sharing-your-teams-signing-certificates).
2. In App Store Connect → Users and Access → Integrations → App Store
   Connect API, request API access if needed, then generate a **Team** key
   with Developer access. Download the `.p8` once and retain the Key ID and
   Issuer ID. This workflow uses team-key authentication. See Apple's
   [API key instructions](https://developer.apple.com/help/app-store-connect/get-started/app-store-connect-api).
3. In the repository's Settings → Secrets and variables → Actions, add:

   | Secret | Value |
   | --- | --- |
   | `MACOS_CERT_P12` | Base64 of the exported `.p12` |
   | `MACOS_CERT_PASSWORD` | The `.p12` export password |
   | `MACOS_NOTARY_KEY` | The complete `.p8` contents |
   | `MACOS_NOTARY_KEY_ID` | The key's Key ID |
   | `MACOS_NOTARY_ISSUER` | The team's Issuer ID |

   To copy the certificate encoding without printing it in a terminal:
   `base64 -i /path/outside/repo/DeveloperID.p12 | pbcopy`.
   Paste into the secret field, then clear the clipboard. Keep exported
   files in secure storage outside the checkout; do not attach them to an issue.

The workflow imports the identity into a temporary keychain and removes it
and the temporary API-key file after the build, including on failure.
No release tag or secret is created by the local build targets.

### Update check

A release build asks `https://api.github.com/repos/wayglass/wayglass/releases/latest`
and shows an "Update" pill when a newer tag exists; the pill opens the
release page, nothing more. A private repository answers 404 and the app
stays quiet, so releases can be tagged before the repository is public.
