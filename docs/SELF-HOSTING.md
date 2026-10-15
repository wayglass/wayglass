# Self-hosting Wayglass

Wayglass is one Go binary plus one Cloudflare Worker. The Worker holds the
intents, decisions and audit log, and it is the only thing that talks to
Artifacts. This page puts that Worker in **your** Cloudflare account, so
nothing depends on wayglass.dev.

It takes about five minutes. `scripts/selfhost.sh` does every step and asks
before it creates anything.

## What you need

| | |
|---|---|
| Cloudflare account | **Workers Paid** plan (Artifacts needs it), with a workers.dev subdomain or a zone for a custom domain |
| API token | Account permissions **Artifacts › Edit**, **Workers Scripts › Edit**, **Queues › Edit**. With `--domain`, also **Zone › Workers Routes › Edit** on that zone. With `--access`, also **Access: Apps and Policies › Edit** and **Access: Organizations, Identity Providers, and Groups › Read** (see [What Access needs](#what-access-needs)). Only the script uses it; the deployed Worker never sees it. |
| Tools | git 2.40+, Go 1.25.8+ (to build the CLI), Node.js 22.12+ with npm (for the pinned `npx wrangler` in `worker/`), Bash, curl, openssl |

## Deploy

```bash
git clone https://github.com/wayglass/wayglass.git && cd wayglass
go build -o bin/wayglass ./cmd/wayglass
export PATH="$PWD/bin:$PATH"

export CLOUDFLARE_ACCOUNT_ID="<your account id>"

scripts/selfhost.sh --dry-run        # writes the config and bundles the Worker; creates nothing
read -r -s CLOUDFLARE_API_TOKEN      # Bash: paste the token without saving it in history
export CLOUDFLARE_API_TOKEN
scripts/selfhost.sh                  # shows the plan, asks, then creates and deploys
```

Without a terminal (an agent, CI), the script refuses to create anything
unless you authorize it up front with `--yes`; the credentials still come
only from the environment:

```bash
# CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN already exported by whoever runs the agent
scripts/selfhost.sh --dry-run        # read the plan first
scripts/selfhost.sh --yes            # same plan, no prompt
```

`--yes` skips only the prompt. A missing namespace or queue is created; any
other failure (a refused token, an unreachable API, a namespace with the
wrong jurisdiction) stops the script with the error Cloudflare returned. The
names you pass are checked before the first API call, so a bad `--name` or
`--namespace` never reaches Cloudflare.

The script:

1. Creates the Artifacts namespace `wayglass` (jurisdiction `us`), or checks
   that an existing one has the jurisdiction you asked for. **A namespace's
   jurisdiction is fixed when it is created.**
2. Creates the queue `artifacts-events` for push events, or reuses it.
3. Writes `worker/wrangler.selfhost.jsonc` from the committed
   `worker/wrangler.jsonc`. Your names, domain and queue id go in that file,
   which git ignores, so you never edit the committed config and pulls never
   conflict.
4. Generates an admin key with `openssl`, and deploys the Worker with
   `npx wrangler deploy --secrets-file`. The secrets are `WAYGLASS_ADMIN_KEY`,
   `CF_ACCOUNT_ID` and, if set, `CF_EVENTS_API_TOKEN`. The key is never
   printed or passed on a command line.
5. Logs this machine in (`wayglass login`) and runs `wayglass doctor`.

### Options

| Option | Default | |
|---|---|---|
| `--name NAME` | `wayglass` | Worker name |
| `--domain HOST` | workers.dev | Serve on a custom domain in one of your zones |
| `--namespace NS` | `wayglass` | Main Artifacts namespace |
| `--jurisdiction J` | `us` | `us`, `eu` or `unrestricted`, for the main namespace |
| `--eu-namespace NS` | none | Also create an EU namespace, so `wayglass init --jurisdiction eu` works and EU-only policy rules (such as quarantining PII) apply |
| `--queue NAME` | `artifacts-events` | Queue for Artifacts events |
| `--public PATTERNS` | none | Projects allowed in the public, read-only audit view, such as `'demo-*,eu-demo-*'` (comma-separated; an EU project named `eu-demo-...` needs its own pattern). Each project must still opt in with `wayglass init --public`. |
| `--ai-review MODE` | `off` | The Workers AI reviewer for `agent_review` changes: `manual` (on request, `wayglass ai-review <intent-id>`) or `auto` (right after every `propose` that lands in `agent_review`). Workers AI bills your account per token; see [Costs](#costs) and [docs/POLICY.md](POLICY.md#agent_review-and-the-ai-reviewer) |
| `--ai-gateway ID` | none | With `--ai-review`: an [AI Gateway](https://developers.cloudflare.com/ai-gateway/) in your account that the model calls go through, for logs, caching and rate limits |
| `--ai-model NAME` | `@cf/meta/llama-4-scout-17b-16e-instruct` | With `--ai-review`: the Workers AI model |
| `--access` | off | Set up Cloudflare Access sign-in for people from your API token: three Access applications, the Worker's Access settings, and a redirect check. Needs `--allow`; works on workers.dev or `--domain`. See [People sign in through Cloudflare Access](#people-sign-in-through-cloudflare-access) |
| `--allow LIST` | | With `--access`: who may sign in. `you@example.com`, `@example.com` or `group:<name>`, comma-separated or repeated |
| `--idp NAME` | all | With `--access`: a login method to offer, by name, id or type (repeatable) |
| `--access-team DOMAIN` | off | Manual Access setup instead of `--access`: your Zero Trust team domain, such as `acme.cloudflareaccess.com`. See [Set it up by hand](#set-it-up-by-hand) |
| `--access-aud TAG` | off | The Access application's audience (AUD) tag; goes with `--access-team` |
| `--access-web-aud TAGS` | `--access-aud` | Audience tag(s), comma-separated, of the Access application covering the web app (`/`, `/console/`, `/v1/public/*`). Needs `--access-team`. See [Set it up by hand](#set-it-up-by-hand) |
| `--keep-key` | | Redeploy without rotating the admin key; the Worker must already exist |
| `--dry-run` | | Write the config and bundle the Worker locally; create and deploy nothing. Needs no API token (with `--access`, a token lets it read the plan) |
| `--yes` | | Do not ask (needed when stdin is not a terminal) |
| `--uninstall` | | Delete this deployment's queue, Worker and the Access applications `--access` created; see [Remove it](#remove-it) |

To redeploy after `git pull`, run `scripts/selfhost.sh --keep-key`. Rerunning
without `--keep-key` rotates the admin key and logs this machine in with the
new one.

### Push events (optional)

When a trunk is pushed to without `wayglass land`, the Worker can flag it in
the audit log. For that, each new trunk needs an event subscription, and the
Worker creates one if it has a token that can only edit Queues:

```bash
read -r -s CF_EVENTS_API_TOKEN       # Bash: token with only Account > Queues > Edit
export CF_EVENTS_API_TOKEN
scripts/selfhost.sh --keep-key
```

Without it everything else works; `wayglass init` warns that trunk pushes are
not watched.

## Use it

```bash
wayglass doctor                      # git, login, Worker, key, project, trunk
cd ~/code/some-repo
wayglass init                        # creates the trunk in your namespace
```

Then follow the loop in the [README](../README.md#4-run-the-loop).

## Give people and agents their own keys

The admin key can do everything, and whoever holds it names themselves. For
anyone else, mint a key bound to a name:

```bash
wayglass keys create --agent claude-ci --note "nightly runs"
```

The key is shown once; the Worker keeps only its SHA-256. Everything done with
it is recorded as `claude-ci`, whatever `WAYGLASS_AGENT` or `X-Wayglass-Agent`
say, so it cannot approve its own change under another name. On the agent's
machine:

```bash
printf %s "$KEY" | wayglass login --worker https://wayglass.<you>.workers.dev --key-stdin
```

`wayglass keys list` shows keys and when each was last used. `wayglass keys
revoke <id>` takes effect on the key's next request.

## People sign in through Cloudflare Access

Wayglass applies Zero Trust to people the same way it applies it to changes:
nobody is trusted because of where they connect from, and every session is
short and tied to one identity. Agents keep their keys. People sign in with
your organisation's login through [Cloudflare
Access](https://developers.cloudflare.com/cloudflare-one/access-controls/),
and the Worker mints each of them a human key bound to their email that
expires after 12 hours (24 at most). Their approvals are recorded as
`you@example.com`, and `wayglass why` shows `Approved: you@example.com
(Access)`.

Wayglass never talks to an identity provider. Access runs the login with
whatever login methods your Zero Trust organisation has, and the Worker
verifies the signed token Access attaches to each request. So Okta, Microsoft
Entra ID, Google Workspace, GitHub, any OIDC or SAML provider, and the
built-in one-time PIN all work, and switching providers later needs no change
to Wayglass.

With Access on, the Worker also refuses the web app (`/`, `/console/`, every
static file) and `/v1/public/*` to anyone without a valid Access token. The key
API (`/v1/*`) is not gated: agents reach it with their keys, and the Worker
refuses it without one.

### What Access needs

| | |
|---|---|
| Hostname | None to add. Without `--domain`, the apps protect the Worker's own `<name>.<subdomain>.workers.dev`, read from the account (`GET /accounts/{id}/workers/subdomain`). With `--domain`, they protect that hostname in one of your zones. Access protects paths of either kind of hostname the same way. |
| Zero Trust organisation | Created once in the dashboard (below). The free plan is enough for up to 50 users. |
| API token | The permissions from [What you need](#what-you-need), plus **Access: Apps and Policies › Edit** and **Access: Organizations, Identity Providers, and Groups › Read**. If the organisation has no login method yet, the script adds one-time PIN, which needs **Edit** on that second permission instead of Read. |

**Create the Zero Trust organisation (once).** The script cannot do this for
you. Open [one.dash.cloudflare.com](https://one.dash.cloudflare.com/), pick a
team name and the Free plan. Your team domain is
`<team>.cloudflareaccess.com`; it is shown under **Settings** and can be
changed later. If the account has no organisation, the script stops and says
so before it creates anything. Cloudflare may ask for a payment method even on
the Free plan.

**workers.dev needs no zone.** A fresh account works as long as it has a
workers.dev subdomain, which the dashboard offers the first time you open
**Workers & Pages** (or `npx wrangler subdomain <you>` from `worker/`). If it
has none, the script stops before creating anything and says so. Do not use
the Worker's one-click **Enable Cloudflare Access** (or "Protect this Worker")
setting for Wayglass: it puts sign-in in front of every path, including the
`/v1` key API, so agents could no longer reach it. The script uses three
hostname-based applications instead. Preview and version URLs
(`<version>-<name>.<subdomain>.workers.dev`) are not covered by them; there the
Worker refuses the web app and sign-in for lack of an Access token, and `/v1`
still needs a key.

### The one command

```bash
# on workers.dev (fresh account, no zone)
scripts/selfhost.sh --access --allow 'you@example.com,@example.com'

# or on a custom domain in one of your zones
scripts/selfhost.sh --domain wayglass.example.com \
  --access --allow 'you@example.com,@example.com'
```

Add `--dry-run` first to see the plan: it reads your organisation, login
methods and existing applications, and creates nothing.

`--allow` decides who may sign in. Give it once with a comma-separated list,
or repeat it:

| Entry | Matches |
|---|---|
| `you@example.com` | That email address |
| `@example.com` | Every email address at that domain |
| `group:<name>` | Members of a group in your identity provider; see the table below for what `<name>` is for each provider |

`--idp <name or id>` (repeatable) picks which login methods the sign-in page
offers, by name (`--idp Okta`), id or type (`--idp onetimepin`). Without it,
the script uses every login method the organisation has. If it has none, the
script adds one-time PIN. With exactly one login method, Access skips its
chooser and goes straight to it.

The script then:

1. Picks the hostname (`--domain`, or `<name>.<subdomain>.workers.dev`) and
   reads the organisation's team domain (`GET /accounts/{id}/access/organizations`).
2. Lists its login methods (`GET .../access/identity_providers`) and picks
   them as above, adding one-time PIN if there are none.
3. Creates three self-hosted Access applications, or updates them in place if
   they exist. They are matched by name, so a rerun never makes duplicates:

   | Application | Covers | Policy |
   |---|---|---|
   | `<name> web` | the whole host, plus `/v1/public` | Allow, the people in `--allow`; 24 h session |
   | `<name> human sign-in` | `/v1/human` | Allow, the same people; 12 h session |
   | `<name> key API` | `/v1` | Bypass, everyone |

   `<name>` is the Worker name (`--name`, default `wayglass`). Access applies
   the most specific path, so `/v1/human` and `/v1/public` stay behind
   sign-in, and the rest of `/v1` reaches the Worker. The bypass is safe
   because the Worker checks a key on every `/v1` route itself; Access in
   front of it would only turn agents away, since they carry keys, not
   browser sessions.
4. Writes the team domain and the two applications' audience (AUD) tags into
   `worker/wrangler.selfhost.jsonc` as `ACCESS_TEAM_DOMAIN`, `ACCESS_AUD`
   (sign-in) and `ACCESS_WEB_AUD` (web app), and deploys. None of these is
   secret.
5. Checks the result with `curl`: `/` and `/v1/human/login` must answer `302`
   to `https://<team>.cloudflareaccess.com/...`, and `/v1/whoami` must answer
   `401` without a key. New applications and a new custom domain can take a few minutes; if the
   check fails, rerun with `--keep-key`.

It never changes the organisation's settings or its login methods (apart from
adding one-time PIN when there is none), and never touches other Access
applications.

### Sign in

```bash
wayglass login --human --worker https://wayglass.example.com
wayglass doctor        # "session" shows when the human session ends
```

The browser opens the Access login, then a Wayglass page asks you to confirm;
the key returns to a one-time listener on `127.0.0.1` in a form POST, never in
a URL, and is stored in `credentials.yaml` (mode 0600) with its expiry. On a
machine without a browser, open the printed URL in a browser on the same
machine. Run it again when the session ends. The web app signs a person in
through Access by itself: the Access login that let you reach the page is the
only one you see (a key can still be used at `#/signin/key`). **Sign out**
revokes that session's key on the Worker (`POST /v1/keys/self/revoke`, which
any bound key may call on itself) and ends the Access session at
`/cdn-cgi/access/logout`. Your identity provider's own session (Okta, for
example) is separate, so the next sign-in may not ask for a password.

### Login methods (identity providers)

Set the provider up in **Zero Trust › Integrations › Identity providers**
before running the script; Cloudflare's guide for each one is linked below.
Every provider that redirects back to Access uses the same callback URL:

```
https://<team>.cloudflareaccess.com/cdn-cgi/access/callback
```

Register it in the provider as the redirect URI (OIDC) or Assertion Consumer
Service URL (SAML).

| Provider | Cloudflare guide | `group:<name>` matches | Notes |
|---|---|---|---|
| Okta | [Okta](https://developers.cloudflare.com/cloudflare-one/integrations/identity-providers/okta/) | the Okta group name | Add a `groups` claim to the Okta app, or groups never reach Access. Okta set up as SAML counts as SAML below. |
| Microsoft Entra ID | [Entra ID](https://developers.cloudflare.com/cloudflare-one/integrations/identity-providers/entra-id/) | the group's **Object ID**, not its display name | Grant the app the directory permissions the guide lists, or group checks fail. |
| Google Workspace | [Google Workspace](https://developers.cloudflare.com/cloudflare-one/integrations/identity-providers/google-workspace/) | the group's email address, such as `group:reviewers@example.com` | Finish the admin consent link after saving. Group changes can take up to 24 hours to reach Access. Plain Google (consumer accounts) has no groups. |
| GitHub | [GitHub](https://developers.cloudflare.com/cloudflare-one/integrations/identity-providers/github/) | an organisation (`group:acme`) or a team (`group:acme/reviewers`) | The person must grant the OAuth app access to that organisation. |
| Generic OIDC | [Generic OIDC](https://developers.cloudflare.com/cloudflare-one/integrations/identity-providers/generic-oidc/) | the value of a `groups` claim | Add `groups` under the provider's custom OIDC claims, and make sure the provider sends an `email` claim. |
| Generic SAML | [Generic SAML](https://developers.cloudflare.com/cloudflare-one/integrations/identity-providers/generic-saml/) | the value of a `groups` attribute | Send an attribute named `groups` and an email in the assertion. |
| One-time PIN | [One-time PIN](https://developers.cloudflare.com/cloudflare-one/integrations/identity-providers/one-time-pin/) | not supported | Access emails a code to the address; only addresses `--allow` matches receive one. No provider to set up. |
| Cloudflare | [Cloudflare](https://developers.cloudflare.com/cloudflare-one/integrations/identity-providers/cloudflare/) | not supported | Members of your Cloudflare account sign in with their Cloudflare login. New organisations get this one by default. |

The Worker needs an email in the token Access issues; every provider above
supplies one when set up as its guide says. `group:` entries become one rule
per selected provider that has groups; the script refuses a `group:` entry when
none of the selected providers has groups.

### Add or remove people later

Rerun the same command with a new `--allow` list. The script updates the
policy on the web and sign-in applications in place. You can also edit the
`<name> reviewers` policy on both applications in **Zero Trust › Access
controls › Applications**; a rerun overwrites that policy with `--allow`, so
keep the two in step.

Removing someone from the policy stops new sign-ins. A human key they already
hold stays valid until it expires (12 hours by default). To cut it off now,
run `wayglass keys list` and `wayglass keys revoke <id>` with the admin key.

### Set it up by hand

If you prefer the dashboard, or your token cannot have Access permissions,
create the same three applications in **Zero Trust › Access controls ›
Applications** (each **Add an application › Self-hosted**):

1. `<domain>` and `<domain>/v1/public`, with an Allow policy for your
   reviewers. Copy its **Application Audience (AUD) Tag**.
2. `<domain>/v1/human`, session duration 12 h, with the same Allow policy.
   Copy its AUD tag. An application with no policy lets nobody in.
3. `<domain>/v1`, with a **Bypass** policy that includes **Everyone**.

Then deploy with the values (none is secret):

```bash
scripts/selfhost.sh --keep-key --domain wayglass.example.com \
  --access-team <team>.cloudflareaccess.com \
  --access-aud <AUD of the /v1/human app> --access-web-aud <AUD of the web app>
```

Without `--access-web-aud`, the Worker accepts the `--access-aud` tag on web
routes too (one application for the whole host). A gate with audiences but no
valid team domain refuses everything.

### How the Worker checks the token

The Worker does not trust Access blindly: it verifies the
`Cf-Access-Jwt-Assertion` header (or the `CF_Authorization` cookie) itself:
RS256 signature from `https://<team>/cdn-cgi/access/certs`, issuer, audience,
expiry, and an email claim. It refuses anything else. Service tokens carry no
email, so they cannot mint a human session. Every session is logged as
`key.created` with `source: access` (`GET /v1/keys/events`, admin key).

With every Access setting empty there is no gate, so the anonymous `--public`
view works as before. If you expose the web app to anyone but yourself, turn
Access on.

### Troubleshooting Access

| Symptom | Cause and fix |
|---|---|
| `selfhost.sh` says the account has no Zero Trust organisation | Create it once at [one.dash.cloudflare.com](https://one.dash.cloudflare.com/), then rerun. |
| `selfhost.sh` says the token cannot read the Zero Trust organisation | Cloudflare answers `403` with code `10000` on `GET .../access/organizations` when the token lacks **Access: Organizations, Identity Providers, and Groups › Read**, even if it can list applications. Add that permission (Edit if one-time PIN must be added) and rerun. |
| `selfhost.sh` says the account has no workers.dev subdomain | Register one in **Workers & Pages**, or pass `--domain`. |
| `selfhost.sh` says the token was refused on an `/access/` call | Add the two Access permissions above to the token. |
| `/` answers `302` to `<team>.cloudflareaccess.com` | Working as intended: Access wants a sign-in. |
| `/` answers `403` from the Worker, with no redirect | The request reached the Worker without an Access token: the web application does not cover this host, or Access is not in front of it yet. Check the application's hostname. |
| The Access page says "That account does not have access" | You signed in, but the policy does not include you. Add your email, domain or group to `--allow` and rerun. |
| The sign-in page says "wrong audience" | `ACCESS_AUD` or `ACCESS_WEB_AUD` does not match the application. Rerun `--access`, or copy the AUD tags again in the manual setup. |
| The sign-in page says "wrong issuer" | `--access-team` does not match the organisation's team domain. |
| The sign-in page says "no Cloudflare Access token" | The `/v1/human` application does not cover this host. |
| The sign-in page says "token names no person" | The provider sent no email claim. Fix the provider so it sends one. |
| Login fails at the provider, or groups do not match | In **Zero Trust › Integrations › Identity providers**, select **Test** next to the provider. It shows the identity and groups Access receives; compare them with your `--allow` entries. |
| `/v1/whoami` answers `302` instead of `401` | The `<name> key API` bypass application is missing, so agents cannot reach the API. Rerun `--access`. |

## Where EU project data lives

With `--eu-namespace`, a project made with `wayglass init --jurisdiction eu`
keeps its repos in that namespace and its Worker state (intents, decisions,
audit log, sessions, cost meters) in a Durable Object pinned to the EU
(`env.PROJECT.jurisdiction("eu")`). That needs nothing in `wrangler.jsonc`
beyond the existing `PROJECT` binding: jurisdiction is chosen per object when
it is created, not per binding. Only the name-to-jurisdiction directory and
the caller keys (the `REGISTRY` object) are global; see "Where a project's
data lives" in `docs/ARCHITECTURE.md`. An EU project created by a Worker
deployed before Oct 14, 2026 answers `residency_pending` (503) until the admin
runs `wayglass project migrate-residency <name>` once; the command is safe to
rerun.

## AI providers

The reviewer is off by default. Enable requests with `--ai-review manual`,
or use `auto` to review on propose. Policy may request AI review or advice
in manual mode. `--ai-providers workers-ai,anthropic,openai` selects the
providers the Worker may call; the default is `workers-ai`. Third-party
providers require `--ai-gateway <id>` and a provider key stored in that
Gateway under the `default` BYOK alias, or Unified Billing credit. No
provider keys go in Wayglass clients.

These settings become Worker vars: `AI_REVIEW`, `AI_PROVIDERS`,
`AI_GATEWAY_ID` and `AI_REVIEW_MODEL` (`--ai-model`, for Workers AI).
`AI_ANTHROPIC_MODEL` and `AI_OPENAI_MODEL` override third-party defaults.
Policy `ai.model` can select a model from an enabled provider. The reviewer
chooses a family different from the author’s and can fall back once to an
enabled provider if the first call fails. With no suitable provider, it
refuses review. Advice may use the author’s family because it cannot decide.

Use `--ai-budget <n>` (`AI_DAILY_REVIEWS`) to cap model calls per project per
UTC day; the default is 200, shared by reviews and advice. A value of zero
sends reviews to a person and skips advice. Every fallback needs its own
remaining call. `GET /v1/cost?project=<name>` reports `ai.budget`, usage,
tokens and estimates. Unknown model prices are reported as unpriced calls.

## Costs

Artifacts billing starts **Oct 14, 2026**: 10,000 operations and 1 GB of
storage a month are included. Each intent is one fork plus a few short-lived
tokens; `wayglass doctor` mints one 60-second read token. Durable Objects,
Queues and Worker requests are billed under Workers Paid like any other
Worker's.

The Workers AI reviewer (`--ai-review`) is off by default. With Workers AI, each call is billed to your account in
[neurons](https://developers.cloudflare.com/workers-ai/platform/pricing/)
(10,000 a day are included with Workers Paid). With the default model and a
typical change (a few thousand input tokens, about 100 output tokens) a review
costs on the order of 100 to 200 neurons, well under a cent; the diff sent to
the model is capped at 60 KB, and larger changes go to a person without a
call. The Worker's `ai` binding is always declared; without `--ai-review` it
is never called.

## Remove it

Deploying never deletes anything. To remove a deployment, pass the same
`--name` and `--queue` you deployed with:

```bash
scripts/selfhost.sh --name wayglass --queue artifacts-events --uninstall   # add --yes without a terminal
```

It deletes, in this order:

1. **The queue.** Cloudflare refuses to delete a Worker that is still a
   queue's consumer (`code: 10064`), so the queue goes first.
2. **The Worker**, with its Durable Objects: every intent, decision and audit
   event it held.
3. **The three Access applications** that `--access` created (`<name> web`,
   `<name> human sign-in`, `<name> key API`), found by name. It never deletes
   the Zero Trust organisation, its login methods or any other application.
   If the token has no Access permission, it says so and goes on; delete the
   applications in the dashboard if you created them.

After each delete it asks the API whether the resource still exists and
reports that, rather than trusting the delete call's exit code. It never
deletes the Artifacts namespaces or their repos; whether to delete those in
the Cloudflare dashboard is the account owner's call. The same API token as
for deploying is enough.

By hand, the order is the same: `npx wrangler queues delete <queue>`, then
`npx wrangler delete --config wrangler.selfhost.jsonc` from `worker/`. With
the token above, `wrangler delete` can end with
`Authentication error [code: 10000]` on `/storage/kv/namespaces`: it lists KV
namespaces after deleting the Worker, which needs **Workers KV Storage ›
Read**. The Worker is already gone at that point. Check instead of trusting
the exit code:

```bash
printf 'header = "Authorization: Bearer %s"\n' "$CLOUDFLARE_API_TOKEN" |
  curl -sS --config - "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/workers/services/wayglass" | jq -c '{success, errors}'
```

`"success": false` with code `10090` ("This Worker does not exist on this
account") means it is deleted.

## Troubleshooting

| `wayglass doctor` says | Do this |
|---|---|
| `worker` fail, "without a Wayglass envelope" | The URL is not the Worker's origin. Use the `https://<name>.<subdomain>.workers.dev` that `wrangler deploy` printed. |
| `auth` fail, `not_configured` | The Worker has no admin key. Rerun the script without `--keep-key`. |
| `auth` fail right after a deploy | New secrets take a few seconds to reach every request; run doctor again. |
| `trunk` fail, "no project" | You are logged in to a different Worker than the one this repo was initialised on. |
| `init` fails with `jurisdiction "eu" is not available` | Redeploy with `--eu-namespace <name>`. |
| `session` fail, "human session expired" | Run `wayglass login --human` again. |
| `login --human` fails with `access_not_configured` | The Worker has no Access settings. Redeploy with `--access` (or `--access-team` and `--access-aud`). |
| Anything about Access sign-in | See [Troubleshooting Access](#troubleshooting-access). |
