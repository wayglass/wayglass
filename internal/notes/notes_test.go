package notes

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/gitx"
)

var env = []string{
	"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
	"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
}

func repo(t *testing.T) (gitx.Git, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	g := gitx.Git{Dir: t.TempDir(), Env: env}
	if _, err := g.Run(t.Context(), "init", "--quiet", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.Dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := g.CommitAll(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	return g, sha
}

func TestWriteReadPushFetch(t *testing.T) {
	ctx := t.Context()
	g, sha := repo(t)

	if _, err := Read(ctx, g, sha); !errors.Is(err, ErrNoNote) {
		t.Fatalf("read before write: %v", err)
	}
	if ok, err := Exists(ctx, g); ok || err != nil {
		t.Fatalf("exists before write = %v, %v", ok, err)
	}
	want := Note{
		IntentID: "i0123456789", Goal: "Add login limits", Reason: "Brute force",
		Tags: classify.Tags{Areas: []string{"auth"}, Risk: "low"}, Rule: "rules[2]",
		Action: "human_approve", DecidedBy: "john", DecidedAt: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC),
	}
	if err := Write(ctx, g, sha, want); err != nil {
		t.Fatal(err)
	}
	want.Reason = "Brute force, confirmed"
	if err := Write(ctx, g, sha, want); err != nil { // overwrite must work
		t.Fatal(err)
	}
	got, err := Read(ctx, g, sha)
	if err != nil {
		t.Fatal(err)
	}
	if got.V != Version || got.Reason != want.Reason || got.Tags.Areas[0] != "auth" || !got.DecidedAt.Equal(want.DecidedAt) {
		t.Fatalf("got %+v", got)
	}

	remote := filepath.Join(t.TempDir(), "r.git")
	if _, err := (gitx.Git{Env: env}).Run(ctx, "init", "--quiet", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	if err := g.Push(ctx, remote, "HEAD:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if err := Push(ctx, g, remote); err != nil {
		t.Fatal(err)
	}

	clone := gitx.Git{Dir: t.TempDir(), Env: env}
	if _, err := clone.Run(ctx, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if err := clone.Fetch(ctx, remote, "refs/heads/main:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if err := Fetch(ctx, clone, remote); err != nil {
		t.Fatal(err)
	}
	back, err := Read(ctx, clone, sha)
	if err != nil || back.Reason != want.Reason {
		t.Fatalf("fetched note = %+v, %v", back, err)
	}
}
