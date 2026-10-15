# Wayglass desktop

A window for the whole cycle: the swarm of intents, the changes policy routed
to a person (approve or reject with a written reason), overlaps, and the audit
log. It uses the login `wayglass login` stored and keeps no secret of its own.

## Run

For Windows downloads and PowerShell build instructions, see
[Windows](#windows).

Needs Go 1.25.8+, Xcode command line tools on macOS (GTK3 and WebKitGTK dev
packages on Linux), and the Wails v2 CLI:

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0
wayglass login --worker https://api.wayglass.dev   # once, if not logged in
make desktop                                        # tests, then wails build
open desktop/build/bin/Wayglass.app                 # macOS
```

Without the Wails CLI (macOS):

```bash
cd desktop
CGO_LDFLAGS="-framework UniformTypeIdentifiers -mmacosx-version-min=10.13" \
  go build -tags desktop,production -ldflags "-w -s" -o build/bin/wayglass-desktop .
```

## Windows

Windows builds are unsigned desktop executables for Windows 10/11, available
for x64 (`amd64`) and ARM64 (`arm64`). They use Microsoft's
[WebView2 Runtime](https://developer.microsoft.com/en-us/microsoft-edge/webview2/).
If it is missing, the app offers to open Microsoft's download page and exits;
install the Evergreen Runtime, then launch Wayglass again.

### Download

Get matching archives from [GitHub Releases](https://github.com/wayglass/wayglass/releases):

- `wayglass-desktop_windows_amd64.zip` or `wayglass-desktop_windows_arm64.zip`
  contains `wayglass-desktop.exe` and the licence.
- `wayglass_windows_amd64.zip` or `wayglass_windows_arm64.zip` contains the
  CLI, `wayglass.exe`, used to sign in.

Extract both archives. Compare each ZIP's `Get-FileHash -Algorithm
SHA256` result with `desktop-checksums.txt` or `checksums.txt` from that release.
In the CLI folder, run:

```powershell
.\wayglass.exe login --human --worker https://api.wayglass.dev
```

Then open the desktop `wayglass-desktop.exe`. Both use the same login under
`$HOME\.config\wayglass` by default. For a self-hosted Worker, use its URL
and the login method its administrator provides. Set `$env:WAYGLASS_PROJECT`
before starting the desktop app, or add your project with `+` in the window.

There is no Windows signing certificate or Store account required to build
these executables. Downloaded unsigned apps can trigger
[Microsoft Defender SmartScreen](https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/smartscreen-reputation).
Smart App Control or an organization's application policy may block them;
building from source does not guarantee an exemption from those policies.

### Build from source

Install Git for Windows and Go 1.25.8 or newer. This project's checked-in
frontend needs no Node build, and its Windows build needs no Visual Studio
or C compiler. In PowerShell, clone the whole repository: the desktop module
builds against the parent directory.

```powershell
git clone https://github.com/wayglass/wayglass.git
Set-Location wayglass
go build -trimpath -o bin/wayglass.exe ./cmd/wayglass
go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0
$env:PATH += ";$(go env GOPATH)\bin"
Set-Location desktop
go vet ./...
go test ./...
wails build -platform windows/amd64 -o wayglass-desktop.exe -webview2 browser
```

Stop if any command fails. On an ARM64 PC, use `-platform windows/arm64`.
The desktop executable is `desktop\build\bin\wayglass-desktop.exe`; the CLI is
`bin\wayglass.exe`. Sign in with the CLI, then launch the desktop app:

```powershell
# Still in the desktop directory
..\bin\wayglass.exe login --human --worker https://api.wayglass.dev
.\build\bin\wayglass-desktop.exe
```

These commands make a development build that does not check for updates.
Release builds embed the version and link to newer GitHub Releases.

Status: the x64 and ARM64 release executables are cross-compiled with
`make desktop-release-windows` (checked on macOS; the release workflow runs
the same script on Linux) and have not been run on Windows.
The Windows CI job builds the CLI, runs the desktop tests on x64 and builds
the app for both architectures; it does not exercise the GUI. Login, window
behavior and notifications still need an interactive smoke test on Windows.

## Notes

- **Identity.** Approvals are recorded as the display name set in the window
  (click the name in the title bar; saved in `desktop.yaml`, never in the
  credentials file). Without one, the app uses the CLI's identity:
  `$WAYGLASS_AGENT`, else the `agent` in `~/.config/wayglass/credentials.yaml`,
  else `user@host`.
  The app refuses to decide a change its identity wrote; the Worker refuses too.
  A `multi_approve` change already approved under this identity is refused as
  well (the Worker counts distinct approvers); a reject still goes through.
- **Login problems.** An expired human session (`wayglass login --human`) and a
  key the Worker no longer accepts are both reported with the command to run;
  a Worker that does not answer shows as retrying, and the banner clears when
  it is back.
- **Agent-written text** (goals, reasons, names, paths) is rendered as text
  only, under a CSP with no inline scripts, and cleaned of escape sequences and
  bidi controls before it reaches the window or a notification.
- **Projects.** The Worker has no project list route, so the picker starts with
  `$WAYGLASS_PROJECT` and the `.wayglass/config.yaml` of the directory the app
  was started from; add others with `+`. The list is kept in
  `~/.config/wayglass/desktop.yaml` (names and the display name only).
- **Polling** matches `wayglass watch`: every 2 s, backing off to 30 s while
  the Worker does not answer. An idle poll reads only the audit log.
- **macOS notifications** (bell icon) need the bundled `.app`; a bare binary has no
  bundle id, so it only updates the window title.
- The frontend is plain HTML, CSS and ES modules in `frontend/dist`; there is
  no Node build. `frontend/wailsjs` is generated by `wails build` and ignored.
- **Updates.** A release build (one with a `v*` version linked in) asks
  GitHub Releases for the latest tag 5 s after start and every 6 h. A newer
  tag shows an "Update vX.Y.Z" pill in the title bar; clicking it opens the
  release page in the browser. Nothing is downloaded or replaced on its own.
  The check is quiet: while the repository is private, offline, or
  rate-limited there is no pill and no error. The only URL the pill can open
  is the one Go fetched and checked against
  `https://github.com/wayglass/wayglass/releases/`; the window passes none.
  `make desktop` builds "dev", which never asks.

## Release builds

`scripts/desktop-release.sh` (`make desktop-release-mac`, `make
desktop-release-linux`, `make desktop-release-windows`) writes `dist/desktop/`: a universal (arm64 + amd64)
`wayglass-desktop_darwin_universal.zip`, `wayglass-desktop_linux_amd64.tar.gz`
(binary, `.desktop` entry, icon, licence), unsigned Windows ZIPs for x64 and
ARM64, and `desktop-checksums.txt`. The
whole path, including what CI does on a tag and how a locally signed macOS
build gets into the release, is in `docs/RELEASING.md`.

### Linux

```bash
tar -xzf wayglass-desktop_linux_amd64.tar.gz
install -Dm755 wayglass-desktop ~/.local/bin/wayglass-desktop
install -Dm644 dev.wayglass.desktop.desktop ~/.local/share/applications/dev.wayglass.desktop.desktop
install -Dm644 wayglass-desktop.png ~/.local/share/icons/hicolor/1024x1024/apps/wayglass-desktop.png
```

Needs GTK 3 and WebKitGTK 4.1 at run time (`libwebkit2gtk-4.1-0` on
Debian and Ubuntu, `webkit2gtk4.1` on Fedora, `webkit2gtk-4.1` on Arch).

### Signing (macOS), and why updates keep their permissions

macOS keys every grant the app is given (Files and Folders for Documents,
Downloads and Desktop; notifications; the Gatekeeper first-open approval)
to the app's **designated requirement**, not to the file on disk. A
Developer ID signature's requirement is the bundle identifier plus the
Team ID, so a new version signed the same way *is the same app* to TCC and
nothing is asked again. An ad-hoc signature's requirement is the cdhash of
that exact build, so every update would look like a new app and ask for
every folder again, and Gatekeeper would refuse it on any other Mac. Hence
the rules in `scripts/desktop-release.sh`:

- The bundle identifier is `dev.wayglass.desktop` (`build/darwin/Info.plist`)
  and never changes.
- Release builds are signed with the one **Developer ID Application**
  identity (Team ID in the certificate), hardened runtime on, timestamped;
  never ad-hoc, never with an Apple Development certificate (those sign, but
  cannot be notarized). The script refuses to continue unless `codesign -d
  -r-` reports a designated requirement that names both the identifier and
  the Team ID.
- Entitlements are empty (`build/darwin/entitlements.plist`). The app is not
  sandboxed: it reads the CLI's login in `~/.config/wayglass`, which a
  sandboxed app could not see, and without the sandbox networking needs no
  entitlement; a Go binary needs neither JIT nor unsigned executable memory
  (checked: the hardened-runtime build runs with this file).
- The app itself touches only `~/.config/wayglass` (reads
  `credentials.yaml`, writes `desktop.yaml`) and, to find the project, reads
  `.wayglass/config.yaml` upward from the directory it was started in. It
  never opens a git repository. Started from Finder that directory is `/`,
  so no folder prompt appears; started from a terminal inside a checkout
  under Documents, Downloads or Desktop, macOS asks once, with the reason in
  `Info.plist` (`NS*FolderUsageDescription`), and the grant survives updates
  for the reason above.
- The app is notarized (`xcrun notarytool`) and the ticket stapled, so it
  opens offline on a Mac that has never seen it.

Local signing check (October 14, 2026): universal builds
`v0.0.0-signtest` and `v0.0.1-signtest`, signed with the same Developer ID,
have identical designated requirements: `dev.wayglass.desktop`, Apple’s
Developer ID certificate checks, and team `ZDZZXMQPNS`, with no cdhash.
This verifies identity continuity across versions. Apple accepted the
`v0.0.0-signtest` build; its stapled ticket validates and Gatekeeper reports
`accepted, source=Notarized Developer ID`. A copy marked with Safari's
quarantine attribute opened under App Translocation without a Gatekeeper
prompt. An actual upgrade with pre-existing folder or notification grants
was not exercised.

To check a shipped build:

```bash
codesign -d -r- /Applications/Wayglass.app       # designated => identifier "dev.wayglass.desktop" ... subject.OU = "<Team ID>"
spctl --assess --type execute -v /Applications/Wayglass.app   # accepted, source=Notarized Developer ID
```
