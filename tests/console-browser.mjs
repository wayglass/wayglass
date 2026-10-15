// Browser regression checks use synthetic API responses. Live checks are separate.
// PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs node tests/console-browser.mjs
// playwright-core works too, with an installed Chrome:
//   CHROME_BIN="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
import { createServer } from "node:http";
import { readFile, mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import assert from "node:assert/strict";
const { chromium } = await import(
  process.env.PLAYWRIGHT_MODULE || "playwright"
);
const root = new URL("../web/", import.meta.url);
const headers = await readFile(new URL("_headers", root), "utf8");
const csp = headers.match(/Content-Security-Policy: (.+)/)[1];
const server = createServer(async (req, res) => {
  try {
    const path = new URL(req.url, "http://localhost").pathname;
    const file = new URL(
      "." + (path.endsWith("/") ? path + "index.html" : path),
      root,
    );
    if (!file.href.startsWith(root.href)) {
      res.writeHead(403).end();
      return;
    }
    const body = await readFile(fileURLToPath(file));
    const ext = file.pathname.split(".").at(-1);
    res.setHeader(
      "Content-Type",
      {
        js: "text/javascript",
        css: "text/css",
        png: "image/png",
        svg: "image/svg+xml",
        woff2: "font/woff2",
      }[ext] || "text/html",
    );
    res.setHeader("Content-Security-Policy", csp);
    res.end(body);
  } catch {
    res.writeHead(404).end();
  }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
const browser = await chromium.launch({
  ...(process.env.CHROME_BIN ? { executablePath: process.env.CHROME_BIN } : {}),
  headless: true,
});
const out = process.env.CONSOLE_SCREENSHOTS || "/tmp/wayglass-app-screenshots";
await mkdir(out, { recursive: true });
const i = {
  id: "i1234567890",
  agent: "writer",
  status: "proposed",
  goal: "Require a signed session before opening account settings",
  reason:
    "Account settings expose personal details. Verify the session before reading them.",
  declared_paths: ["src/auth/**"],
  actual_paths: ["src/auth/session.ts"],
  created_at: "2026-10-13T22:01:00Z",
  updated_at: "2026-10-13T22:02:00Z",
  session_id: "s1234567890",
};
const d = {
  id: "d1234567890",
  intent_id: i.id,
  action: "human_approve",
  status: "pending_human",
  created_at: i.updated_at,
  commit_sha: "a".repeat(40),
  matched_rule: "rules[2]",
  reason: "Auth is a security boundary.",
  paths: ["src/auth/session.ts"],
  tags: {
    areas: ["auth"],
    data_classes: ["auth"],
    risk: "low",
    jurisdiction: "us",
  },
  detail: {
    files: [{ path: "src/auth/session.ts", added: 12, deleted: 2 }],
    // The AI reviewer could not approve this alone (worker/src/aireview.ts).
    escalation: {
      by: "workers-ai:@cf/meta/llama-4-scout-17b-16e-instruct",
      reason: "The change reads the session before the signature is checked.",
      concerns: ["signature checked after use"],
      from_action: "agent_review",
      identity_source: "ai",
      ai: { model: "@cf/meta/llama-4-scout-17b-16e-instruct" },
    },
    shadow_matches: [
      {
        rule: "rules[4]",
        action: "multi_approve",
        reason: "Try a second reviewer for session changes.",
        preempts: false,
      },
    ],
    would_differ: false,
  },
};
const cost = {
  total: { ops: 240, est_usd: 0.036 },
  ai: { calls: 3, est_usd: 0.0021, today: { calls: 1, est_usd: 0.0007 } },
  scope: "Worker-side binding calls only. Client git traffic is not counted.",
};
// What POST /v1/intents/:id/ai-advice records: advisory, decides nothing.
const advice = {
  summary: "Reads the session before its signature is verified; the rest matches the goal.",
  risks: ["A forged session reaches the settings read"],
  concerns: [],
  questions: ["Should the signature check move above the read?"],
  by: "workers-ai:@cf/meta/llama-4-scout-17b-16e-instruct",
  model: "@cf/meta/llama-4-scout-17b-16e-instruct",
};
let shots = 0;
// Cloudflare Access on the Worker: a person is signed in through the login
// page without a key form, sign-out ends both layers, and a stale or refused
// session waits for a click instead of looping through the identity provider.
async function accessScenarios() {
  const context = await browser.newContext({
    viewport: { width: 1280, height: 900 },
  });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  const n = { probes: 0, logins: 0, whoamis: 0, revokes: 0 };
  let probe = "redirect", // redirect | on | fail: what GET /v1/human/session answers
    whoami = 200;
  const session = {
    key: "art_human_key",
    agent: "person@example.com",
    expires_at: "2099-01-01T00:00:00Z",
  };
  // What the Worker's /v1/human/login?to=console page does once the session
  // route minted a key (worker/src/human.ts): store it, then open the app.
  const loginPage = (project) =>
    `<!doctype html><title>Wayglass sign-in</title><p id="msg">Signing in...</p><script>
      const s = ${JSON.stringify(session)}, cfg = ${JSON.stringify({ project })};
      sessionStorage.setItem("wayglass.console.session", JSON.stringify({ key: s.key, name: s.agent, source: "access", expires_at: s.expires_at }));
      sessionStorage.removeItem("wayglass.console.signed-out");
      location.replace("/" + (cfg.project ? "?project=" + encodeURIComponent(cfg.project) : "") + (cfg.project ? "#/review" : "#/projects"));
    </script>`;
  await page.route("**/cdn-cgi/access/logout", (route) =>
    route.fulfill({
      contentType: "text/html",
      body: "<!doctype html><title>Access</title><p>Signed out of Cloudflare Access.</p>",
    }),
  );
  await page.route("**/v1/**", async (route) => {
    const request = route.request(),
      u = new URL(request.url()),
      fail = (status, code) =>
        route.fulfill({
          status,
          json: { v: 1, ok: false, error: { code, message: code } },
        });
    assert.equal(u.origin, origin);
    if (u.pathname === "/v1/human/session") {
      n.probes++;
      assert.equal(request.method(), "GET");
      if (probe === "fail") return route.abort("connectionfailed");
      if (probe === "redirect")
        return route.fulfill({
          status: 302,
          headers: {
            location: "https://team.cloudflareaccess.com/cdn-cgi/access/login/x",
          },
        });
      return fail(405, "method_not_allowed");
    }
    if (u.pathname === "/v1/human/login") {
      n.logins++;
      assert.equal(u.searchParams.get("to"), "console");
      assert.equal(request.headers().authorization, undefined);
      return route.fulfill({
        contentType: "text/html",
        body: loginPage(u.searchParams.get("project") || ""),
      });
    }
    if (u.pathname.startsWith("/v1/public/")) {
      // Polled while signed out; nothing is public on this Worker.
      assert.equal(request.headers().authorization, undefined);
      return fail(404, "not_found");
    }
    assert.equal(request.headers().authorization, `Bearer ${session.key}`);
    let data;
    if (u.pathname === "/v1/whoami") {
      n.whoamis++;
      if (whoami !== 200) return fail(whoami, "unauthorized");
      assert.equal(request.headers()["x-wayglass-agent"], session.agent);
      data = {
        agent: session.agent,
        kind: "human",
        role: "agent",
        bound: true,
        key_id: "k0987654321",
        source: "access",
        expires_at: session.expires_at,
      };
    } else if (u.pathname === "/v1/keys/self/revoke") {
      n.revokes++;
      data = { revoked: true };
    } else if (u.pathname === "/v1/projects")
      data = { projects: [], next: null };
    else if (u.pathname === "/v1/intents") data = { intents: [i] };
    else if (u.pathname === "/v1/decisions") data = { decisions: [d] };
    else if (u.pathname === "/v1/audit") data = { events: [], next: 0 };
    else if (u.pathname === "/v1/cost") data = cost;
    else if (u.pathname.includes("/changes/"))
      data = {
        decision_id: d.id,
        path: d.paths[0],
        base: "b".repeat(40),
        commit: d.commit_sha,
        before: { text: "" },
        after: { text: "export const valid = Boolean(session);\n" },
      };
    else throw new Error("Unexpected route: " + u.pathname);
    await route.fulfill({ json: { v: 1, ok: true, data } });
  });
  const stored = () => page.evaluate(() => Object.entries(sessionStorage));
  const accessLink = page.getByRole("link", {
    name: "Sign in with Cloudflare Access",
    exact: true,
  });
  // A first visit, behind Access (the probe is redirected to the login page):
  // signed in through the login page with no key form, straight to the queue.
  await page.goto(origin + "/?project=demo-test-console");
  await page.getByRole("button", { name: "Approve", exact: true }).waitFor();
  assert.equal(new URL(page.url()).hash, "#/review");
  await page.getByText(`${session.agent} · Person`, { exact: true }).waitFor();
  assert.deepEqual(
    { logins: n.logins, whoamis: n.whoamis },
    { logins: 1, whoamis: 1 },
  );
  // Sign-out revokes the key, forgets it here and ends the Access session too,
  // without the signed-out marker: the next visit signs in by itself again.
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await page.waitForURL("**/cdn-cgi/access/logout");
  assert.equal(n.revokes, 1);
  assert.deepEqual(await stored(), []);
  probe = "on"; // the self-hosted shape: the Worker itself answers the probe
  await page.goto(origin + "/");
  await page.getByRole("heading", { name: "Projects", exact: true }).waitFor();
  await page.getByText(`${session.agent} · Person`, { exact: true }).waitFor();
  assert.equal(n.logins, 2);
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await page.waitForURL("**/cdn-cgi/access/logout");
  assert.equal(n.revokes, 2);
  // A session past its expiry is dropped unsent, and the sign-in page waits
  // for a click: no login redirect, no whoami, key form hidden.
  await page.evaluate((s) => {
    sessionStorage.setItem(
      "wayglass.console.session",
      JSON.stringify({ ...s, expires_at: "2020-01-01T00:00:00Z" }),
    );
  }, { key: session.key, name: session.agent, source: "access" });
  await page.goto(origin + "/");
  await page.getByText("Your session has expired", { exact: false }).waitFor();
  await accessLink.waitFor();
  assert.equal(await page.locator("#key").isVisible(), false);
  assert.deepEqual(
    { logins: n.logins, whoamis: n.whoamis },
    { logins: 2, whoamis: 2 },
  );
  assert.deepEqual(await stored(), [["wayglass.console.signed-out", "1"]]);
  await page.screenshot({
    path: `${out}/light-1280-access-signed-out.png`,
    fullPage: true,
  });
  shots++;
  await page
    .getByRole("link", { name: "Use a Wayglass key instead", exact: true })
    .click();
  await page.locator("#key").waitFor({ state: "visible" });
  await accessLink.waitFor();
  // A stored key the Worker refuses (revoked, or expired on its side) is
  // dropped and the page waits for a click: no automatic login loop.
  whoami = 401;
  await page.evaluate((s) => {
    sessionStorage.setItem("wayglass.console.session", JSON.stringify(s));
    sessionStorage.removeItem("wayglass.console.signed-out");
  }, { key: session.key, name: session.agent, source: "access", expires_at: session.expires_at });
  await page.goto(origin + "/");
  await page.getByText("Your key was refused", { exact: false }).waitFor();
  await accessLink.waitFor();
  assert.deepEqual(
    { logins: n.logins, whoamis: n.whoamis },
    { logins: 2, whoamis: 3 },
  );
  assert.deepEqual(await stored(), [["wayglass.console.signed-out", "1"]]);
  // A probe that fails (network, timeout) offers no Access sign-in and sends
  // nobody to a login page that may not exist.
  probe = "fail";
  whoami = 200;
  await page.evaluate(() => sessionStorage.clear());
  await page.goto(origin + "/console/"); // a fresh document, redirected to sign-in
  await page.locator("#key").waitFor({ state: "visible" });
  await page.waitForTimeout(300);
  assert.equal(await accessLink.count(), 0);
  assert.equal(n.logins, 2);
  assert.equal(new URL(page.url()).pathname, "/");
  assert.deepEqual(errors, []);
  await context.close();
}
try {
  for (const colorScheme of ["light", "dark"])
    for (const width of [1280, 390]) {
      const context = await browser.newContext({
        viewport: { width, height: 900 },
        colorScheme,
      });
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      page.on("console", (m) => {
        if (
          m.type() === "error" &&
          m.text().includes("Content Security Policy")
        )
          errors.push(m.text());
      });
      let state = "items",
        posts = 0,
        asks = 0,
        revokes = 0,
        identityKind = "human",
        delay = false,
        release;
      await page.route("**/v1/**", async (route) => {
        const request = route.request(),
          u = new URL(request.url());
        assert.equal(u.origin, origin);
        let data;
        if (u.pathname === "/v1/human/session")
          // No Cloudflare Access on this Worker: the key form is the way in.
          return route.fulfill({
            status: 404,
            json: {
              v: 1,
              ok: false,
              error: { code: "access_not_configured", message: "not here" },
            },
          });
        if (u.pathname === "/v1/whoami")
          data = {
            agent: "reviewer",
            kind: identityKind,
            role: "agent",
            bound: true,
            key_id: "k1234567890",
            source: "mint",
          };
        else if (u.pathname === "/v1/keys/self/revoke") {
          assert.equal(request.method(), "POST");
          revokes++;
          data = { revoked: true };
        }
        else if (
          u.pathname === "/v1/projects" ||
          u.pathname === "/v1/public/projects"
        )
          data = {
            projects: [
              {
                name: "demo-test-console",
                jurisdiction: "us",
                counts: { open: 2, agents: 2 },
              },
            ],
            next: null,
          };
        else if (u.pathname.endsWith("/audit"))
          data = {
            events: [
              {
                seq: 1,
                at: i.created_at,
                kind: "intent.declared",
                actor: "writer",
                intent_id: i.id,
                data: { goal: i.goal },
              },
            ],
            next: 1,
          };
        else if (u.pathname.endsWith("/intents")) {
          if (delay)
            await new Promise((resolve) => {
              release = resolve;
            });
          data = {
            intents:
              state === "empty"
                ? []
                : [
                    i,
                    {
                      ...i,
                      id: "i0987654321",
                      agent: "second-writer",
                      goal: "Check the session expiry path",
                      status: "open",
                      actual_paths: [],
                    },
                  ],
          };
        } else if (
          u.pathname === "/v1/decisions" ||
          u.pathname.endsWith("/decisions")
        )
          data = {
            decisions:
              state === "empty"
                ? []
                : [asks ? { ...d, detail: { ...d.detail, ai_advice: advice } } : d],
          };
        else if (u.pathname === `/v1/intents/${i.id}/ai-advice`) {
          asks++;
          assert.equal(request.method(), "POST");
          assert.equal(request.postDataJSON().project, "demo-test-console");
          data = {
            intent_id: i.id,
            decision: { ...d, detail: { ...d.detail, ai_advice: advice } },
            advice,
          };
        }
        else if (u.pathname.includes("/changes/"))
          data = {
            decision_id: d.id,
            path: d.paths[0],
            base: "b".repeat(40),
            commit: d.commit_sha,
            before: { text: "export const valid = true;\n" },
            after: { text: "export const valid = Boolean(session);\n" },
          };
        else if (u.pathname.endsWith("/browse")) {
          const path = u.searchParams.get("path");
          data =
            path === ".wayglass/policy.yaml"
              ? {
                  ref: d.commit_sha,
                  text: "version: 1\nrules:\n  - when: { area: auth }\n    action: human_approve\n    reason: Auth is a security boundary.\ndefault: { action: human_approve, reason: Review unmatched changes. }\n",
                }
              : path
                ? {
                    ref: d.commit_sha,
                    path,
                    type: "blob",
                    text: "export const valid = Boolean(session);\n",
                  }
                : {
                    ref: d.commit_sha,
                    type: "tree",
                    entries: [{ name: "session.ts", type: "blob" }],
                  };
        } else if (u.pathname.endsWith("/history"))
          data = {
            commits: [
              {
                hash: d.commit_sha,
                message: "Validate sessions before opening settings",
                author: "writer",
              },
            ],
            next: null,
            scope: "First-parent history of the trunk.",
          };
        else if (u.pathname === "/v1/why") data = { intent: i, decision: d };
        else if (u.pathname.includes("/sessions/"))
          data = {
            session: {
              notes: "Check session expiry next.",
              snapshot: { intent: i },
            },
            url: origin + "/v1/projects/demo-test-console/sessions/s1234567890",
          };
        else if (u.pathname === "/v1/cost") data = cost;
        else if (u.pathname === "/v1/public/projects/demo-test-console")
          data = { project: { jurisdiction: "us" }, cost };
        else if (u.pathname === `/v1/decisions/${d.id}`) {
          posts++;
          assert.equal(request.method(), "POST");
          assert.ok(
            ["approve", "reject"].includes(request.postDataJSON().verdict),
          );
          assert.equal(request.postDataJSON().reason, "<what you checked>");
          return route.fulfill({
            status: 400,
            json: {
              v: 1,
              ok: false,
              error: {
                code: "bad_request",
                message:
                  "A placeholder reason is not enough. Describe what you checked.",
              },
            },
          });
        } else throw new Error("Unexpected route: " + u.pathname);
        if (!u.pathname.startsWith("/v1/public/"))
          assert.equal(
            request.headers()["x-wayglass-agent"],
            u.pathname === "/v1/whoami" ? "chosen-name" : "reviewer",
          );
        await route.fulfill({ json: { v: 1, ok: true, data } });
      });
      const shot = async (name) => {
        await page.evaluate(() => scrollTo(0, 0));
        await page.screenshot({
          path: `${out}/${colorScheme}-${width}-${name}.png`,
          fullPage: true,
          animations: "disabled",
        });
        assert.ok(
          await page.evaluate(
            () => document.documentElement.scrollWidth <= innerWidth,
          ),
          name + " horizontal overflow",
        );
        shots++;
      };
      const openNav = async () => {
        if (width < 1024 && !(await page.locator("#drawer").isChecked()))
          await page.getByLabel("Open navigation", { exact: true }).click();
      };
      const nav = async (label) => {
        await openNav();
        await page
          .locator("aside")
          .getByRole("link", { name: label, exact: true })
          .click();
      };
      await page.goto(origin + "/console/?api=https://untrusted.invalid");
      await page.locator("#key").waitFor();
      assert.equal(new URL(page.url()).pathname, "/");
      assert.equal(new URL(page.url()).search, "");
      await shot("signed-out");
      await page.locator("#key").fill("test-key");
      await page.locator("#name").fill("chosen-name");
      await page.getByRole("button", { name: "Sign in", exact: true }).click();
      await page
        .getByRole("heading", { name: "demo-test-console", exact: true })
        .waitFor();
      await shot("projects");
      await page.locator('main a[href*="?project="]').first().click();
      await page
        .getByRole("heading", { name: "Project dashboard", exact: true })
        .waitFor();
      await shot("dashboard");
      await nav("Review queue");
      await page
        .getByRole("button", { name: "Approve", exact: true })
        .waitFor({ timeout: 5000 })
        .catch(async (e) => {
          console.log(errors, await page.locator("body").innerText());
          await page.screenshot({ path: "/tmp/web-review-failure.png" });
          throw e;
        });
      await page
        .getByText("export const valid = Boolean(session);", { exact: false })
        .waitFor();
      await shot("queue");
      await page.getByRole("button", { name: "Approve", exact: true }).click();
      assert.equal(posts, 0);
      await page.locator("textarea").fill("<what you checked>");
      await page.getByRole("button", { name: "Approve", exact: true }).click();
      await page.getByText("bad_request:", { exact: false }).waitFor();
      await shot("refusal");
      assert.equal(posts, 1);
      // The AI reviewer's escalation reads as the AI's; "Ask AI for advice"
      // records advice that decides nothing, labelled as advice.
      await page
        .getByText("Escalated by AI reviewer (@cf/meta/llama-4-scout-17b-16e-instruct):", { exact: false })
        .first()
        .waitFor();
      await page.getByText("• signature checked after use", { exact: true }).first().waitFor();
      await page.getByRole("button", { name: "Ask AI for advice", exact: true }).click();
      await page.getByText("advised; advice only, the decision is yours.", { exact: false }).waitFor();
      assert.equal(asks, 1);
      await page.getByText("AI advice · advisory", { exact: false }).first().waitFor();
      await page.getByText("Should the signature check move above the read?", { exact: false }).first().waitFor();
      await page.getByRole("button", { name: "Approve", exact: true }).waitFor();
      await shot("ai-advice");
      await page.getByRole("button", { name: "Reject", exact: true }).click();
      await page.waitForFunction(
        () => !document.querySelector('button[value="reject"]').disabled,
      );
      assert.equal(posts, 2);
      await page.waitForTimeout(2200);
      assert.equal(
        await page.locator("textarea").inputValue(),
        "<what you checked>",
      );
      await nav("Intents");
      await page
        .getByText("Land i1234567890 before i0987654321;", { exact: false })
        .waitFor();
      await shot("intents-overlaps");
      await page.getByRole("link", { name: i.goal, exact: true }).click();
      await page
        .getByRole("heading", { name: "Why this change", exact: true })
        .waitFor();
      await shot("intent-detail");
      await page
        .getByRole("link", { name: "Read change", exact: true })
        .click();
      await page
        .getByRole("heading", { name: "Change", exact: true })
        .waitFor();
      await shot("change");
      await nav("Code");
      await page
        .getByRole("link", { name: "session.ts", exact: true })
        .waitFor();
      await shot("code-tree");
      await page.getByRole("link", { name: "session.ts", exact: true }).click();
      await page
        .getByRole("button", { name: "Why line 1", exact: true })
        .click();
      await page
        .getByText("Line 1 · commit provenance", { exact: true })
        .waitFor();
      await shot("code-file-why");
      await nav("History");
      await page
        .getByText("Validate sessions before opening settings", { exact: true })
        .waitFor();
      await shot("history");
      await nav("Policy");
      await page
        .getByRole("heading", { name: "Source YAML", exact: true })
        .waitFor();
      await shot("policy");
      await nav("Audit log");
      await page.locator("#audit-filter").fill("writer");
      const downloadPromise = page.waitForEvent("download");
      await page
        .getByRole("button", { name: "Export JSON", exact: true })
        .click();
      const download = await downloadPromise;
      assert.equal(
        download.suggestedFilename(),
        "demo-test-console-audit.json",
      );
      await shot("audit");
      await page.evaluate(() => (location.hash = "/sessions/s1234567890"));
      await page
        .getByText("Check session expiry next.", { exact: true })
        .waitFor();
      await shot("session");
      state = "empty";
      await openNav();
      await page.locator("#project").fill("demo-test-empty");
      await page.getByRole("button", { name: "Open", exact: true }).click();
      await page
        .getByRole("heading", { name: "Project dashboard", exact: true })
        .waitFor();
      await nav("Review queue");
      await page.getByText("Nothing waiting on you", { exact: true }).waitFor();
      await shot("empty");
      assert.equal(
        await page.evaluate(() =>
          localStorage
            .getItem("wayglass.console.projects")
            .includes("test-key"),
        ),
        false,
      );
      // A late authenticated poll must not repaint signed-out state.
      delay = true;
      await page.waitForTimeout(2200);
      await openNav();
      await page.getByRole("button", { name: "Sign out", exact: true }).click();
      release?.();
      await page.locator("#key").waitFor();
      await page.waitForTimeout(100);
      // Sign-out revoked the key on the Worker and left only the signed-out
      // marker in this tab, never the key.
      assert.equal(revokes, 1);
      assert.deepEqual(
        await page.evaluate(() => Object.entries(sessionStorage)),
        [["wayglass.console.signed-out", "1"]],
      );
      assert.equal(await page.locator("#key").inputValue(), "");
      assert.deepEqual(errors, []);
      await context.close();
    }
  await accessScenarios();
  console.log(
    `${shots} screenshots; every route, exact-decision review, refusals, the AI reviewer's escalation and advice, draft retention, identity, exports, cancellation, Access sign-in and sign-out, CSP and responsive checks passed.`,
  );
} finally {
  await browser.close();
  server.close();
}
