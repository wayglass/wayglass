package gitx

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Source is a git repository somewhere else that a person wants to bring
// into Wayglass: a URL git can clone with the person's own credentials.
type Source struct {
	// URL is the value to hand to git, unchanged.
	URL string
	// Kind is "https", "ssh" or "local".
	Kind string
	// Host is the server (empty for a local path).
	Host string
	// Name is the repository's own name: the last path element without
	// ".git", e.g. "fzf" for https://github.com/junegunn/fzf.git.
	Name string
}

// ErrBadSource is returned for a URL git must not be given.
var ErrBadSource = errors.New("gitx: not a usable repository URL")

// ParseSource checks a clone URL before git sees it. Accepted: https://,
// ssh://, scp-like user@host:path, and (only with allowLocal) a local path
// or file:// URL. Refused: anything starting with "-" (git would read it as
// an option), remote helpers such as ext:: or fd:: (they run commands),
// git:// and http:// (no authentication, no integrity), and a URL carrying
// a password or token, which would end up in .git/config and in error
// messages. The returned error never repeats a credential.
func ParseSource(raw string, allowLocal bool) (Source, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return Source{}, fmt.Errorf("%w: empty", ErrBadSource)
	case strings.HasPrefix(raw, "-"):
		return Source{}, fmt.Errorf("%w: %q starts with '-'", ErrBadSource, raw)
	}
	if i := strings.Index(raw, "::"); i >= 0 {
		return Source{}, fmt.Errorf("%w: %q uses a git remote helper, which can run commands", ErrBadSource, raw[:i+2])
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return Source{}, fmt.Errorf("%w: contains whitespace", ErrBadSource)
	}
	if scheme, rest, ok := strings.Cut(raw, "://"); ok {
		switch strings.ToLower(scheme) {
		case "https", "ssh":
			u, err := url.Parse(raw)
			if err != nil || u.Host == "" {
				return Source{}, fmt.Errorf("%w: %s:// URL does not parse", ErrBadSource, scheme)
			}
			kind := strings.ToLower(scheme)
			if kind == "https" && u.User != nil {
				return Source{}, fmt.Errorf("%w: the URL carries a username or password; let your git credential helper supply them", ErrBadSource)
			}
			if kind == "ssh" && u.User != nil {
				if _, has := u.User.Password(); has {
					return Source{}, fmt.Errorf("%w: the URL carries a password; use an SSH key", ErrBadSource)
				}
			}
			return Source{URL: raw, Kind: kind, Host: strings.ToLower(u.Hostname()), Name: repoName(u.Path)}, nil
		case "file":
			if !allowLocal {
				return Source{}, fmt.Errorf("%w: file:// needs --allow-local", ErrBadSource)
			}
			return Source{URL: raw, Kind: "local", Name: repoName(rest)}, nil
		case "git", "http":
			return Source{}, fmt.Errorf("%w: %s:// is not authenticated or encrypted; use https:// or ssh", ErrBadSource, scheme)
		default:
			return Source{}, fmt.Errorf("%w: unknown scheme %q", ErrBadSource, scheme)
		}
	}
	// scp-like: user@host:path or host:path, as long as it is not a local
	// path ("./x", "/x", "C:\x").
	if before, after, ok := strings.Cut(raw, ":"); ok && !strings.ContainsAny(before, "/\\") && after != "" && !strings.HasPrefix(after, "\\") {
		user, host, hasUser := strings.Cut(before, "@")
		if !hasUser {
			host, user = user, ""
		}
		if strings.Contains(user, ":") || host == "" {
			return Source{}, fmt.Errorf("%w: %q is not user@host:path", ErrBadSource, before)
		}
		return Source{URL: raw, Kind: "ssh", Host: host, Name: repoName(after)}, nil
	}
	if !allowLocal {
		return Source{}, fmt.Errorf("%w: %q is a local path; use https:// or ssh, or pass --allow-local", ErrBadSource, raw)
	}
	return Source{URL: raw, Kind: "local", Name: repoName(raw)}, nil
}

// repoName is the last path element of p without a ".git" suffix.
func repoName(p string) string {
	p = strings.TrimRight(strings.ReplaceAll(p, "\\", "/"), "/")
	p = strings.TrimSuffix(p, ".git")
	p = strings.TrimRight(p, "/")
	return path.Base(p)
}

// CloneSource clones src into dir with the person's own git credentials
// (ssh-agent, credential helpers): no token is set and nothing is stored.
// Tags come along, unlike Clone, because a repository's own history is the
// point. Git never prompts on a terminal here; a missing credential fails.
func (g Git) CloneSource(ctx context.Context, src Source, dir string) error {
	if err := checkArgs(src.URL, dir); err != nil {
		return err
	}
	_, err := g.Run(ctx, "clone", "--quiet", "--", src.URL, dir)
	return err
}
