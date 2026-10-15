package gitx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testEnv makes commits deterministic and independent of the user's config.
var testEnv = []string{
	"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
	"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
}

func newRepo(t *testing.T) Git {
	t.Helper()
	needGit(t)
	g := Git{Dir: t.TempDir(), Env: testEnv}
	if _, err := g.Run(t.Context(), "init", "--quiet", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	return g
}

func write(t *testing.T, g Git, name, body string) {
	t.Helper()
	p := filepath.Join(g.Dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepoWorkflow(t *testing.T) {
	ctx := t.Context()
	g := newRepo(t)

	if _, err := g.Head(ctx); err == nil {
		t.Fatal("Head on an empty repo should fail")
	}
	write(t, g, "README.md", "hi\n")
	base, err := g.CommitAll(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := g.Dirty(ctx)
	if err != nil || dirty {
		t.Fatalf("dirty = %v, %v", dirty, err)
	}

	write(t, g, "web/login.ts", "a\nssn = 1\n")
	write(t, g, "README.md", "hi\nmore\n")
	if dirty, _ := g.Dirty(ctx); !dirty {
		t.Fatal("expected dirty tree")
	}
	head, err := g.CommitAll(ctx, "second\n\nWhy: test")
	if err != nil {
		t.Fatal(err)
	}

	files, err := g.ChangedFiles(ctx, base, head)
	if err != nil || !slices.Equal(files, []string{"README.md", "web/login.ts"}) {
		t.Fatalf("changed = %v, %v", files, err)
	}
	all, err := g.ChangedFiles(ctx, "", head)
	if err != nil || len(all) != 2 {
		t.Fatalf("changed since empty = %v, %v", all, err)
	}
	diff, err := g.Diff(ctx, base, head)
	if err != nil || !strings.Contains(diff, "+ssn = 1") || !strings.Contains(diff, "b/web/login.ts") {
		t.Fatalf("diff = %q, %v", diff, err)
	}

	sha, err := g.BlameLine(ctx, "web/login.ts", 2)
	if err != nil || sha != head {
		t.Fatalf("blame = %q, %v; want %s", sha, err, head)
	}
	if sha, _ := g.BlameLine(ctx, "README.md", 1); sha != base {
		t.Fatalf("blame line 1 = %q, want %s", sha, base)
	}

	if _, ok, err := g.Resolve(ctx, "refs/nope"); ok || err != nil {
		t.Fatalf("resolve missing = %v, %v", ok, err)
	}
}

func TestPushFetchAndDefaultBranch(t *testing.T) {
	ctx := t.Context()
	g := newRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	if _, err := (Git{Env: testEnv}).Run(ctx, "init", "--quiet", "--bare", "--initial-branch=main", remote); err != nil {
		t.Fatal(err)
	}
	write(t, g, "a.txt", "a\n")
	head, err := g.CommitAll(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Push(ctx, remote, "HEAD:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	b, err := g.RemoteDefaultBranch(ctx, remote)
	if err != nil || b != "main" {
		t.Fatalf("default branch = %q, %v", b, err)
	}
	if err := g.Fetch(ctx, remote, "+refs/heads/main:refs/wayglass/test/base"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := g.Resolve(ctx, "refs/wayglass/test/base")
	if err != nil || !ok || got != head {
		t.Fatalf("fetched = %q %v %v", got, ok, err)
	}
}

func TestCommits(t *testing.T) {
	ctx := t.Context()
	g := newRepo(t)
	var shas []string
	for i, subject := range []string{"first", "second: with: colons", "third"} {
		write(t, g, "f.txt", strings.Repeat("x", i+1))
		sha, err := g.CommitAll(ctx, subject+"\n\nbody line\n")
		if err != nil {
			t.Fatal(err)
		}
		shas = append(shas, sha)
	}
	tests := []struct {
		name  string
		rev   string
		limit int
		want  []string
	}{
		{"all", "HEAD", 0, []string{"third", "second: with: colons", "first"}},
		{"limited", "HEAD", 2, []string{"third", "second: with: colons"}},
		{"range", shas[0] + "..HEAD", 0, []string{"third", "second: with: colons"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := g.Commits(ctx, tt.rev, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			var subjects []string
			for _, c := range got {
				subjects = append(subjects, c.Subject)
				if c.Author != "Test" {
					t.Errorf("author = %q", c.Author)
				}
			}
			if !slices.Equal(subjects, tt.want) {
				t.Fatalf("subjects = %q, want %q", subjects, tt.want)
			}
		})
	}
	all, err := g.Commits(ctx, "HEAD", 0)
	if err != nil {
		t.Fatal(err)
	}
	if all[0].SHA != shas[2] || all[0].Parent != shas[1] || all[2].Parent != "" {
		t.Fatalf("parents = %+v", all)
	}
}

// The policy preview sizes binary files with one cat-file call; a missing
// path (deleted in the commit) is simply absent, and a diff parsed from a
// unified diff carries binary files marked without their size.
func TestBlobSizes(t *testing.T) {
	ctx := t.Context()
	g := newRepo(t)
	write(t, g, "img.png", "\x89PNG\x00\x01\x02")
	write(t, g, "docs/a.md", "hello\n")
	head, err := g.CommitAll(ctx, "assets")
	if err != nil {
		t.Fatal(err)
	}
	sizes, err := g.BlobSizes(ctx, head, []string{"img.png", "gone.bin", "docs/a.md", "docs"})
	if err != nil {
		t.Fatal(err)
	}
	if sizes["img.png"] != 7 || sizes["docs/a.md"] != 6 || len(sizes) != 2 {
		t.Fatalf("sizes = %v", sizes)
	}
	if _, err := g.BlobSizes(ctx, "--output=x", []string{"img.png"}); err == nil {
		t.Fatal("a revision that looks like an option must be refused")
	}
	diff, err := g.Diff(ctx, "", head)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "Binary files /dev/null and b/img.png differ") {
		t.Fatalf("diff does not mark the binary:\n%s", diff)
	}
}

func TestLargeBlobsCurrentBranchAndRemotes(t *testing.T) {
	ctx := t.Context()
	g := newRepo(t)
	write(t, g, "small.txt", "tiny\n")
	write(t, g, "assets/big.bin", strings.Repeat("x", 3000))
	if _, err := g.CommitAll(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	// A file that was big once and is gone from HEAD's tree still travels
	// with the history, so it must be found too.
	write(t, g, "old.bin", strings.Repeat("y", 5000))
	if _, err := g.CommitAll(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "rm", "--quiet", "old.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.CommitAll(ctx, "third"); err != nil {
		t.Fatal(err)
	}
	// A big file only a tag reaches is found only when tags are scanned.
	if _, err := g.Run(ctx, "checkout", "--quiet", "-b", "release"); err != nil {
		t.Fatal(err)
	}
	write(t, g, "tagged.bin", strings.Repeat("z", 8000))
	if _, err := g.CommitAll(ctx, "tagged"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "tag", "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "checkout", "--quiet", "main"); err != nil {
		t.Fatal(err)
	}

	big, err := g.LargeBlobs(ctx, 2000, "HEAD", false)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, b := range big {
		paths = append(paths, b.Path)
	}
	if !slices.Equal(paths, []string{"old.bin", "assets/big.bin"}) || big[0].Size != 5000 {
		t.Fatalf("large blobs = %+v", big)
	}
	if big, _ := g.LargeBlobs(ctx, 2000, "HEAD", true); len(big) != 3 || big[0].Path != "tagged.bin" {
		t.Fatalf("large blobs with tags = %+v", big)
	}
	if big, _ := g.LargeBlobs(ctx, 1<<20, "HEAD", true); len(big) != 0 {
		t.Fatalf("large blobs above 1 MiB = %+v", big)
	}
	if _, err := g.LargeBlobs(ctx, 1, "--all", false); !errors.Is(err, ErrBadArg) {
		t.Fatalf("option as rev = %v", err)
	}
	if n, err := g.ObjectsSize(ctx); err != nil || n < 16000 {
		t.Fatalf("objects size = %d, %v", n, err)
	}

	if b, err := g.CurrentBranch(ctx); err != nil || b != "main" {
		t.Fatalf("branch = %q, %v", b, err)
	}
	if _, err := g.Run(ctx, "checkout", "--quiet", "--detach", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if b, err := g.CurrentBranch(ctx); err != nil || b != "" {
		t.Fatalf("detached branch = %q, %v", b, err)
	}

	if _, ok, err := g.RemoteURL(ctx, "wayglass"); ok || err != nil {
		t.Fatalf("missing remote = %v, %v", ok, err)
	}
	if err := g.AddRemote(ctx, "wayglass", "https://x.example/git/ns/repo.git?token="+testToken); err == nil {
		t.Fatal("a URL carrying a token must be refused")
	}
	if err := g.AddRemote(ctx, "wayglass", "https://user:pw@x.example/git/ns/repo.git"); err == nil {
		t.Fatal("a URL carrying a password must be refused")
	}
	if err := g.AddRemote(ctx, "wayglass", "https://x.example/git/ns/repo.git"); err != nil {
		t.Fatal(err)
	}
	if u, ok, err := g.RemoteURL(ctx, "wayglass"); err != nil || !ok || u != "https://x.example/git/ns/repo.git" {
		t.Fatalf("remote = %q, %v, %v", u, ok, err)
	}
}
