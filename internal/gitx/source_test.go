package gitx

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSource(t *testing.T) {
	tests := []struct {
		raw        string
		allowLocal bool
		kind, host string
		name       string
		refuse     string // substring of the error; "" means accepted
	}{
		{raw: "https://github.com/junegunn/fzf.git", kind: "https", host: "github.com", name: "fzf"},
		{raw: "https://github.com/junegunn/fzf", kind: "https", host: "github.com", name: "fzf"},
		{raw: "https://gitlab.com/group/sub/project.git/", kind: "https", host: "gitlab.com", name: "project"},
		{raw: "HTTPS://Example.com/x/y.git", kind: "https", host: "example.com", name: "y"},
		{raw: "git@github.com:junegunn/fzf.git", kind: "ssh", host: "github.com", name: "fzf"},
		{raw: "github.com:junegunn/fzf.git", kind: "ssh", host: "github.com", name: "fzf"},
		{raw: "ssh://git@github.com/junegunn/fzf.git", kind: "ssh", host: "github.com", name: "fzf"},
		{raw: "ssh://git@bitbucket.example:7999/proj/repo.git", kind: "ssh", host: "bitbucket.example", name: "repo"},
		{raw: " https://github.com/a/b.git\n", kind: "https", host: "github.com", name: "b"},
		{raw: "/srv/git/repo.git", allowLocal: true, kind: "local", name: "repo"},
		{raw: "../repo", allowLocal: true, kind: "local", name: "repo"},
		{raw: "file:///srv/git/repo.git", allowLocal: true, kind: "local", name: "repo"},

		{raw: "", refuse: "empty"},
		{raw: "-oProxyCommand=evil", refuse: "starts with '-'"},
		{raw: "--upload-pack=evil", refuse: "starts with '-'"},
		{raw: "ext::sh -c evil", refuse: "remote helper"},
		{raw: "fd::17", refuse: "remote helper"},
		{raw: "https://github.com/a/b.git ext::x", refuse: "remote helper"},
		{raw: "https://github.com/a/b.git c.git", refuse: "whitespace"},
		{raw: "https://user:token@github.com/a/b.git", refuse: "username or password"},
		{raw: "https://token@github.com/a/b.git", refuse: "username or password"},
		{raw: "ssh://git:pw@github.com/a/b.git", refuse: "password"},
		{raw: "git://github.com/a/b.git", refuse: "not authenticated"},
		{raw: "http://github.com/a/b.git", refuse: "not authenticated"},
		{raw: "ftp://x/y", refuse: "unknown scheme"},
		{raw: "https://", refuse: "does not parse"},
		{raw: "/srv/git/repo.git", refuse: "--allow-local"},
		{raw: "file:///srv/git/repo.git", refuse: "--allow-local"},
		{raw: "./repo", refuse: "--allow-local"},
		{raw: "C:\\repos\\x", refuse: "--allow-local"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			src, err := ParseSource(tt.raw, tt.allowLocal)
			if tt.refuse != "" {
				if !errors.Is(err, ErrBadSource) || !strings.Contains(err.Error(), tt.refuse) {
					t.Fatalf("err = %v, want %q", err, tt.refuse)
				}
				if strings.Contains(err.Error(), "token") && strings.Contains(err.Error(), "@") {
					t.Fatalf("error repeats the credential: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if src.Kind != tt.kind || src.Host != tt.host || src.Name != tt.name || src.URL != strings.TrimSpace(tt.raw) {
				t.Fatalf("source = %+v", src)
			}
		})
	}
}

func TestCloneSourceKeepsTags(t *testing.T) {
	ctx := t.Context()
	g := newRepo(t)
	write(t, g, "a.txt", "a\n")
	if _, err := g.CommitAll(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "tag", "v1"); err != nil {
		t.Fatal(err)
	}
	src, err := ParseSource(g.Dir, true)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "clone")
	if err := (Git{Env: testEnv}).CloneSource(ctx, src, dir); err != nil {
		t.Fatal(err)
	}
	c := Git{Dir: dir, Env: testEnv}
	if out, err := c.Run(ctx, "tag"); err != nil || out != "v1" {
		t.Fatalf("tags = %q, %v", out, err)
	}
	if b, _ := c.CurrentBranch(ctx); b != "main" {
		t.Fatalf("branch = %q", b)
	}
	if err := (Git{}).CloneSource(ctx, Source{URL: "-x"}, dir); !errors.Is(err, ErrBadArg) {
		t.Fatalf("option as URL = %v", err)
	}
}
