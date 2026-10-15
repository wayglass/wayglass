# Web assets

From this directory, run `npm ci && npm run build` after changing web markup,
classes or vendor imports. Commit `web/app.css` and `web/vendor.js` with the
source change. The Worker serves those files directly; Node is used only for
the build. CSS is Tailwind plus daisyUI. YAML parsing and line comparison are
bundled locally; fonts and Lucide icons are also served locally.

The app uses hash routes so static hosting needs no rewrite fallback:
`/#/projects`, `/?project=<name>#/`, `#/intents`, `#/intents/<id>`,
`#/review`, `#/review/<decision>`, `#/changes/<decision>`, `#/code`,
`#/history`, `#/audit`, `#/policy`, `#/sessions/<id>` and `#/signin`.
Keep the project query parameter on project routes. Existing public audit
URLs open the project dashboard; `/console/` redirects to sign-in or review.

A key stays in tab-scoped session storage until sign-out. The client only
calls the same origin and rejects redirects. Public projects expose their
allowlisted metadata. Code, diffs, history, sessions and source policy need
a key. The Worker decides whether a key can approve a particular change.

Repository reads are bounded: 20 directory candidates or history commits per
request, 1,000 entries per tree, 32 path segments, and 128 KiB of displayed
file text. The browser displays at most 4,000 lines. Large or binary diffs require the CLI. History follows first
parents. The web app can show a selected commit's recorded intent; it cannot
attribute an individual line without git blame. It gives the CLI command
for that lookup. Audit exports contain the loaded, filtered rows; subsequent
pages are loaded with the audit refresh control. Decision counts cover the
latest 500 proposals, including superseded ones. Directory listing operations
are not assigned to a project's cost total.

Run `node --test tests/console-model.test.mjs` from the repository root.
Browser tests are in `tests/console-browser.mjs`; its header describes the
optional Playwright setup. Worker read-route tests are in
`worker/test/browse.test.ts`. Tests use synthetic fixtures, not production
approvals.

Runtime dependency licences: daisyUI and Tailwind (MIT), yaml (ISC), diff
(BSD-3-Clause), Lucide (ISC), DM Sans (SIL Open Font License). The font licence
ships in `web/fonts/LICENSE`. Full notices for the bundled dependencies ship in `web/THIRD-PARTY-NOTICES.txt`.
