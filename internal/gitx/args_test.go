package gitx

import (
	"errors"
	"testing"
)

// TestRejectsOptionLikeArguments covers every helper that passes a value
// from outside this package (a remote, revision, refspec or directory) to
// git as a positional argument: a value starting with "-" must be refused
// before git could read it as an option such as --upload-pack=<command>.
func TestRejectsOptionLikeArguments(t *testing.T) {
	ctx := t.Context()
	g := Git{Dir: t.TempDir()}
	evil := "--upload-pack=touch pwned"
	cases := []struct {
		name string
		call func() error
	}{
		{"Resolve", func() error { _, _, err := g.Resolve(ctx, evil); return err }},
		{"Fetch remote", func() error { return g.Fetch(ctx, evil, "+refs/heads/main:refs/x") }},
		{"Fetch refspec", func() error { return g.Fetch(ctx, "origin", "--output=x") }},
		{"Push remote", func() error { return g.Push(ctx, evil, "HEAD:refs/heads/main") }},
		{"Push refspec", func() error { return g.Push(ctx, "origin", "-f") }},
		{"Clone remote", func() error { return g.Clone(ctx, evil, t.TempDir()) }},
		{"Clone dir", func() error { return g.Clone(ctx, "https://example.invalid/r", "--separate-git-dir=x") }},
		{"RemoteDefaultBranch", func() error { _, err := g.RemoteDefaultBranch(ctx, evil); return err }},
		{"ChangedFiles base", func() error { _, err := g.ChangedFiles(ctx, "--output=x", "HEAD"); return err }},
		{"ChangedFiles head", func() error { _, err := g.ChangedFiles(ctx, "", "--output=x"); return err }},
		{"Diff", func() error { _, err := g.Diff(ctx, "-x", "HEAD"); return err }},
		{"Commits", func() error { _, err := g.Commits(ctx, "--output=x", 1); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrBadArg) {
				t.Errorf("err = %v, want ErrBadArg", err)
			}
		})
	}
	// A force refspec starts with "+", which is not an option.
	if err := checkArgs("+refs/notes/wayglass:refs/notes/wayglass", "origin", "HEAD", ""); err != nil {
		t.Errorf("checkArgs refused a legitimate value: %v", err)
	}
}
