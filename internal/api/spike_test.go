//go:build spike

// Day-one spike against a DEPLOYED Worker and
// real Artifacts. It creates a project (one trunk repo) and one fork, so it
// costs Artifacts operations. Never runs in CI; run it with scripts/spike.sh.
//
//	WAYGLASS_URL=https://<worker> WAYGLASS_KEY=<admin key> scripts/spike.sh
package api_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/gitx"
	"github.com/wayglass/wayglass/internal/notes"
)

func TestSpike(t *testing.T) {
	url, key := os.Getenv(api.EnvURL), os.Getenv(api.EnvKey)
	if url == "" || key == "" {
		t.Fatalf("set %s and %s to a deployed Worker", api.EnvURL, api.EnvKey)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git not installed")
	}
	project := os.Getenv("WAYGLASS_SPIKE_PROJECT")
	if project == "" {
		project = fmt.Sprintf("spike-%d", time.Now().Unix())
	}
	ctx := t.Context()
	c := api.New(url, key)
	c.Agent = "spike"
	identity := []string{
		"GIT_AUTHOR_NAME=Wayglass Spike", "GIT_AUTHOR_EMAIL=spike@wayglass.dev",
		"GIT_COMMITTER_NAME=Wayglass Spike", "GIT_COMMITTER_EMAIL=spike@wayglass.dev",
	}
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Worker creates the trunk and returns remote + short-lived token.
	created, err := c.CreateProject(ctx, project, "Wayglass spike "+time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("step 1 create project: %v", err)
	}
	t.Logf("step 1 OK: trunk %s remote %s token scope %s expires %s", created.Trunk.Name, created.Trunk.Remote, created.Token.Scope, created.Token.ExpiresAt)
	if !strings.HasPrefix(created.Token.Value, "art_v") || created.Trunk.Remote == "" {
		t.Fatalf("step 1: unexpected token or remote shape (remote %q, token shape %q)", created.Trunk.Remote, tokenShape(created.Token.Value))
	}
	trunkBranch := created.Trunk.DefaultBranch
	if trunkBranch == "" {
		trunkBranch = "main"
	}

	seed := gitx.Git{Dir: t.TempDir(), Env: identity}
	if _, err := seed.Run(ctx, "init", "--quiet", "--initial-branch="+trunkBranch); err != nil {
		t.Fatal(err)
	}
	write(seed.Dir, "README.md", "# spike\n")
	if _, err := seed.CommitAll(ctx, "chore: seed trunk"); err != nil {
		t.Fatal(err)
	}
	trunk := seed
	trunk.Token = created.Token.Value
	if err := trunk.Push(ctx, created.Trunk.Remote, "HEAD:refs/heads/"+trunkBranch); err != nil {
		t.Fatalf("step 1 seed push to trunk: %v", err)
	}

	// 2. Worker forks for an intent and mints a write token for the fork only.
	decl, err := c.DeclareIntent(ctx, api.DeclareIntentInput{
		Project: project, Goal: "Spike: prove fork, push and notes", Reason: "day-one spike", Paths: []string{"spike.txt"},
	})
	if err != nil {
		t.Fatalf("step 2 declare intent: %v", err)
	}
	t.Logf("step 2 OK: intent %s fork %s remote %s token scope %s expires %s",
		decl.Intent.ID, decl.Fork.Name, decl.Fork.Remote, decl.Token.Scope, decl.Token.ExpiresAt)
	if decl.Token.Scope != "write" {
		t.Errorf("step 2: fork token scope %q, want write", decl.Token.Scope)
	}

	// 3. Clone the fork with the header, commit, push.
	// git runs in Dir, which clone has yet to create, so clone from its parent.
	parent := gitx.Git{Dir: t.TempDir(), Env: identity, Token: decl.Token.Value}
	work := parent
	work.Dir = filepath.Join(parent.Dir, "fork")
	if _, err := parent.Run(ctx, "clone", "--quiet", decl.Fork.Remote, work.Dir); err != nil {
		t.Fatalf("step 3 clone fork (did the fork copy the trunk's %s?): %v", trunkBranch, err)
	}
	write(work.Dir, "spike.txt", "one\n")
	first, err := work.CommitAll(ctx, "feat: spike commit\n\nWayglass-Intent: "+decl.Intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := work.Push(ctx, "origin", "HEAD:refs/heads/"+trunkBranch); err != nil {
		t.Fatalf("step 3 push to fork: %v", err)
	}
	// --local: the token is meant to be in the GIT_CONFIG_* env config, never in .git/config.
	if out, err := work.Run(ctx, "config", "--local", "--get-regexp", "."); err == nil && strings.Contains(out, "art_v") {
		t.Errorf("step 3: a token reached .git/config")
	}
	t.Logf("step 3 OK: pushed %s to the fork", first)

	// Zero Trust check: the fork token must not write to the trunk.
	wrong := work
	if err := wrong.Push(ctx, created.Trunk.Remote, "HEAD:refs/heads/spike-should-fail"); err == nil {
		t.Errorf("ZERO TRUST BROKEN: the fork token pushed to the trunk")
	} else {
		t.Logf("zero trust OK: fork token refused by the trunk")
	}

	// Report the push like `wayglass push` does.
	if _, err := c.ReportPush(ctx, decl.Intent.ID, api.PushInput{Project: project, Paths: []string{"spike.txt"}, Commit: first}); err != nil {
		t.Errorf("report push: %v", err)
	}

	// 4. Push refs/notes/wayglass and fetch it back into a fresh repo.
	note := notes.Note{IntentID: decl.Intent.ID, Goal: decl.Intent.Goal, Reason: decl.Intent.Reason,
		Tags: classify.Tags{Areas: []string{}, DataClasses: []string{}, Risk: "low"}, Rule: "spike", Action: "auto_merge",
		DecidedBy: "spike", DecidedAt: time.Now().UTC()}
	if err := notes.Write(ctx, work, first, note); err != nil {
		t.Fatal(err)
	}
	if err := notes.Push(ctx, work, "origin"); err != nil {
		t.Errorf("step 4 FAILED, notes push refused: %v\nFall back to the DO plus a Why: commit trailer.", err)
	} else {
		fresh := gitx.Git{Dir: t.TempDir(), Env: identity, Token: decl.Token.Value}
		if _, err := fresh.Run(ctx, "init", "--quiet"); err != nil {
			t.Fatal(err)
		}
		// Not refs/heads/: init checked out an unborn branch of that name, and git refuses to fetch into it.
		if err := fresh.Fetch(ctx, decl.Fork.Remote, "+refs/heads/"+trunkBranch+":refs/remotes/fork/"+trunkBranch); err != nil {
			t.Fatal(err)
		}
		if err := notes.Fetch(ctx, fresh, decl.Fork.Remote); err != nil {
			t.Errorf("step 4 FAILED, notes fetch: %v", err)
		} else if got, err := notes.Read(ctx, fresh, first); err != nil || got.IntentID != decl.Intent.ID {
			t.Errorf("step 4 FAILED, note did not round-trip: %+v, %v", got, err)
		} else {
			t.Logf("step 4 OK: %s pushed and fetched back intact", notes.Ref)
		}
	}

	// 5. A further push to the trunk must reach the queue consumer as
	// cf.artifacts.repo.pushed. repo.pushed subscriptions are per repo
	// (ARCHITECTURE D4) and this trunk is new, so the subscription can only
	// be created now; the test re-pushes every 30 s while it waits.
	wait := 5 * time.Minute // the trunk token from step 1 lives 15 minutes
	if v := os.Getenv("WAYGLASS_SPIKE_EVENT_WAIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			wait = d
		}
	}
	t.Logf("step 5: if no subscription exists for this trunk yet, create it now (needs the account owner):\n"+
		"  npx wrangler queues subscription create artifacts-events --source artifacts.repo "+
		"--source-namespace <namespace from worker/wrangler.jsonc> --source-repo-name %s --events pushed\n"+
		"waiting up to %s", created.Trunk.Name, wait)
	pushed := map[string]bool{}
	pushTrunk := func(n int) {
		write(seed.Dir, "README.md", fmt.Sprintf("# spike\n\npush %d\n", n))
		sha, err := seed.CommitAll(ctx, fmt.Sprintf("chore: trunk push %d", n))
		if err != nil {
			t.Fatal(err)
		}
		if err := trunk.Push(ctx, created.Trunk.Remote, "HEAD:refs/heads/"+trunkBranch); err != nil {
			t.Fatalf("step 5 push %d to trunk: %v", n, err)
		}
		pushed[sha] = true
	}
	pushTrunk(2)
	deadline := time.Now().Add(wait)
	lastPush := time.Now()
	since := 0
	for n := 3; ; {
		page, err := c.Audit(ctx, project, since, 500)
		if err != nil {
			t.Fatalf("step 5 audit: %v", err)
		}
		for _, ev := range page.Events {
			if ev.Kind != "artifacts.repo.pushed" {
				continue
			}
			var d struct {
				Repo, Ref, Before, After string
			}
			_ = json.Unmarshal(ev.Data, &d)
			t.Logf("step 5 event: repo %s ref %s before %s after %s", d.Repo, d.Ref, d.Before, d.After)
			if pushed[d.After] {
				t.Logf("step 5 OK: repo.pushed for %s reached the queue consumer", d.After)
				return
			}
		}
		since = page.Next
		if time.Now().After(deadline) {
			t.Fatalf("step 5 FAILED: no artifacts.repo.pushed event within %s. Is the subscription on trunk %s created and the consumer deployed?", wait, created.Trunk.Name)
		}
		if time.Since(lastPush) > 30*time.Second {
			pushTrunk(n)
			n++
			lastPush = time.Now()
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

// tokenShape describes a secret without revealing it: digits become 9,
// letters a or A, punctuation is kept. Enough to see a format change.
func tokenShape(v string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9':
			return '9'
		case r >= 'a' && r <= 'z':
			return 'a'
		case r >= 'A' && r <= 'Z':
			return 'A'
		}
		return r
	}, v)
}
