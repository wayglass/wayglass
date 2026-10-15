# Terminal recordings

Run from the repository root. Install VHS (tested with 0.12.1), its ttyd and
ffmpeg dependencies, Go, Git, jq and Node.js. The tapes use Menlo, Catppuccin
Mocha and a 35 ms typing delay. Install Menlo or change `Set FontFamily` to an
installed monospace font before recording.

```sh
scripts/demo/tapes.sh validate
scripts/demo/tapes.sh render offline 720
scripts/demo/tapes.sh render live 720
scripts/demo/tapes.sh render all 720
scripts/demo/tapes.sh render all 1080
# Render a single step, preparing its prerequisites while hidden:
scripts/demo/tapes.sh render review 720
```

| Tape | What it records |
|---|---|
| `init.tape` | Creates a public demo trunk and writes the example policy. |
| `declare.tape` | Docs and auth agents declare concurrently in separate clones. |
| `watch.tape` | Live overlaps, suggested re-plan, classified changes and intent detail. |
| `propose.tape` | Docs auto-merge; auth needs a person; EU PII is quarantined. |
| `review.tape` | Live review and approval by `codex-tape-tester`, separate from the author. |
| `why.tape` | The docs line's intent, tags, decision and reason from its git note. |
| `session.tape` | Forks the auth session into a new checkout with its saved context. |
| `audit.tape` | The website URL and its authenticated audit API, in the terminal. |
| `watch-demo.tape` | Offline watch, labelled sample data by the application. |
| `review-demo.tape` | Offline review with AI advice and a sample approval. |
| `ai-review.tape` | Offline AI approval, escalation and advice; explicitly labelled sample data. |

The suite writes GIF, MP4 and terminal text to `docs/tapes/out/` (ignored).
Default recordings are 1280×720; the 1080 variant is 1920×1080 with font size
and padding scaled to match. Its output filenames end in `-1080`. A fresh
`run.*` directory retains the project name, local checkouts, redacted responses
and generated tapes. Copy recordings elsewhere before removing the worktree.
Nothing is uploaded by VHS; the runner clears `VHS_PUBLISH`. It also clears
`NO_COLOR` and `CLICOLOR` for VHS and sets `TERM=xterm-256color` and
`COLORTERM=truecolor`, so recordings keep the TUI palette even when the
calling shell disables colour.

Live runs require an existing `wayglass login` with an admin key. The harness
loads that key into the environment without printing or copying it. It uses a
fresh `eu-demo-test-tape-*` project in the existing EU namespace, opted into
the public audit view. A full suite creates a trunk and forks for docs, auth, PII and the
continued session. On exit, the runner revokes its agent keys and deletes its
own disposable project and repos, including after a failed capture. Local
redacted capture files remain. Offline recordings contact neither the Worker
nor Artifacts.

Setup is enclosed in `Hide`/`Show`. The `tape_*` helpers run the real CLI;
`safe` captures JSON, removes token and remote fields, redacts token/account
patterns, and selects fields for display. It also redacts failures. The saved
JSON has the same redactions. No stored response contains a repo token. The
TUI, overlaps and why output come directly from `wayglass`.

The review helper tests empty, whitespace and non-empty usernames before
opening the TUI. The approval reason describes those checks. Approval happens
only on this run's auth intent, as `codex-tape-tester`; the PII intent remains
quarantined. Never point these tapes at an existing presentation project.

Overlaps display a **suggested** re-plan; the harness proposes each change
once, records watch and review, then lands docs before recording why. Session
continuation uses a second local checkout, not a second machine. The audit tape records the authenticated endpoint
used by the website; record the browser separately for a view of the actual
page. These distinctions matter when choosing clips for a video.

Before sharing, inspect the recordings and their terminal text for credentials,
repo tokens and account IDs, including error frames. Check that waits succeeded
and that the policy outcomes match the tape. Treat fixture text and approval
reasons as recording data, not proposed narration or submission copy.

## Website recordings

The landing page is in `../site`, separate from the in-product audit view in
`web/`. Its videos and poster images live in `../site/public/recordings/`.
The older suite is retained locally in `docs/tapes/out/task-003/`.

- `watch.mp4` and `watch.webm` show live overlaps and intent details. The
  gallery includes the other live golden-path commands and the sample-data
  `watch-demo`, `review-demo` and `ai-review` clips. Each has a WebP poster.
- `desktop-app.mp4` is a native Desktop App walkthrough captured on October
  14, 2026, after `make desktop`. It reads the existing
  `demo-1791938487` project: overview, all intents, pending auth details,
  landed docs details and audit history. No approval, rejection or AI request
  is submitted. It is a recording of the live demo project, not offline data.

To refresh the terminal gallery, render the full suite above. Copy each MP4
from `docs/tapes/out/` to `../site/public/recordings/`, convert a WebM alternate,
and extract a readable poster frame. For example:

```sh
ffmpeg -y -i docs/tapes/out/watch.mp4 -c copy -movflags +faststart \
  ../site/public/recordings/watch.mp4
ffmpeg -y -i docs/tapes/out/watch.mp4 -c:v libvpx-vp9 -crf 32 -b:v 0 -an \
  ../site/public/recordings/watch.webm
ffmpeg -y -ss 12 -i docs/tapes/out/watch.mp4 -frames:v 1 -lossless 1 \
  ../site/public/recordings/watch.webp
```

The Desktop App capture frames and timestamps are retained locally under
`docs/tapes/out/desktop-frames/`; `desktop-frames.txt` beside that directory
is the ffmpeg concat list. To encode this capture again:

```sh
ffmpeg -y -f concat -safe 0 -i docs/tapes/out/desktop-frames.txt \
  -vf 'scale=1600:-2,fps=15' -c:v libx264 -crf 23 -pix_fmt yuv420p \
  -movflags +faststart ../site/public/recordings/desktop-app.mp4
ffmpeg -y -ss 1 -i ../site/public/recordings/desktop-app.mp4 -frames:v 1 \
  ../site/public/recordings/desktop-app.webp
```

For a new desktop capture, rebuild the app and repeat those read-only views.
Keep the site's caption accurate about the source of the data. Inspect each
clip before replacing the website assets, then run `npm run build` in
`../site`. The page uses native playback controls and loads video only when
requested, with no autoplay. The TUI / Desktop App selector shows one video
at a time and pauses the hidden video.
