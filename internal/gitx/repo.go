package gitx

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Higher-level operations used by the CLI. Each is one or two git commands;
// none of them ever puts a token in arguments (see the package comment).

// TopLevel returns the root of the working tree.
func (g Git) TopLevel(ctx context.Context) (string, error) {
	return g.Run(ctx, "rev-parse", "--show-toplevel")
}

// Resolve returns the commit a revision points at, or ok=false if it does
// not exist.
func (g Git) Resolve(ctx context.Context, rev string) (string, bool, error) {
	if err := checkArgs(rev); err != nil {
		return "", false, err
	}
	out, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// Head returns the HEAD commit. A repository without commits is an error.
func (g Git) Head(ctx context.Context) (string, error) {
	sha, ok, err := g.Resolve(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("this repository has no commits yet; commit something first")
	}
	return sha, nil
}

// Dirty reports whether the working tree has staged, unstaged or untracked
// changes (ignored files excluded).
func (g Git) Dirty(ctx context.Context) (bool, error) {
	out, err := g.Run(ctx, "status", "--porcelain", "--untracked-files=normal")
	return out != "", err
}

// CommitAll stages every change and commits it. Hooks run as usual.
func (g Git) CommitAll(ctx context.Context, message string) (string, error) {
	if _, err := g.Run(ctx, "add", "--all"); err != nil {
		return "", err
	}
	if _, err := g.RunInput(ctx, strings.NewReader(message), "commit", "--quiet", "--file=-"); err != nil {
		return "", err
	}
	return g.Head(ctx)
}

// CommitPaths stages and commits only the given paths, leaving anything
// else the user has staged alone. If those paths have no changes it commits
// nothing and returns HEAD.
func (g Git) CommitPaths(ctx context.Context, message string, paths ...string) (string, error) {
	if _, err := g.Run(ctx, append([]string{"add", "--"}, paths...)...); err != nil {
		return "", err
	}
	_, hasHead, err := g.Resolve(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	if hasHead {
		_, err := g.Run(ctx, append([]string{"diff", "--cached", "--quiet", "HEAD", "--"}, paths...)...)
		if err == nil {
			return g.Head(ctx)
		}
		var ge *Error
		if !errors.As(err, &ge) || ge.ExitCode != 1 {
			return "", err
		}
	}
	if _, err := g.RunInput(ctx, strings.NewReader(message), append([]string{"commit", "--quiet", "--file=-", "--"}, paths...)...); err != nil {
		return "", err
	}
	return g.Head(ctx)
}

// Fetch fetches refspecs from a remote URL or name.
func (g Git) Fetch(ctx context.Context, remote string, refspecs ...string) error {
	if err := checkArgs(append([]string{remote}, refspecs...)...); err != nil {
		return err
	}
	_, err := g.Run(ctx, append([]string{"fetch", "--quiet", "--no-tags", remote}, refspecs...)...)
	return err
}

// Push pushes refspecs to a remote URL or name. Artifacts only supports
// receive-pack over protocol v1, which stock git push uses.
func (g Git) Push(ctx context.Context, remote string, refspecs ...string) error {
	if err := checkArgs(append([]string{remote}, refspecs...)...); err != nil {
		return err
	}
	_, err := g.Run(ctx, append([]string{"push", "--quiet", "--no-verify", remote}, refspecs...)...)
	return err
}

// Clone clones remote into dir (missing or empty), without tags. A token,
// if set, reaches git through its environment like every other command.
func (g Git) Clone(ctx context.Context, remote, dir string) error {
	if err := checkArgs(remote, dir); err != nil {
		return err
	}
	_, err := g.Run(ctx, "clone", "--quiet", "--no-tags", remote, dir)
	return err
}

// RemoteDefaultBranch asks the remote which branch HEAD points at. It
// returns "" when the remote does not say (for example an empty repo).
func (g Git) RemoteDefaultBranch(ctx context.Context, remote string) (string, error) {
	if err := checkArgs(remote); err != nil {
		return "", err
	}
	out, err := g.Run(ctx, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(out) {
		// "ref: refs/heads/main\tHEAD"
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			branch, _, _ := strings.Cut(rest, "\t")
			return strings.TrimSpace(branch), nil
		}
	}
	return "", nil
}

// EmptyTree returns the id of the empty tree in this repo's hash format.
func (g Git) EmptyTree(ctx context.Context) (string, error) {
	return g.RunInput(ctx, strings.NewReader(""), "hash-object", "-t", "tree", "--stdin")
}

// diffArgs builds the range: base...head (changes on head since they
// diverged) when base exists, otherwise everything since the empty tree.
func (g Git) diffRange(ctx context.Context, base, head string) ([]string, error) {
	if err := checkArgs(base, head); err != nil {
		return nil, err
	}
	if base != "" {
		return []string{base + "..." + head}, nil
	}
	empty, err := g.EmptyTree(ctx)
	if err != nil {
		return nil, err
	}
	return []string{empty, head}, nil
}

// ChangedFiles lists the files changed on head since base, sorted. With an
// empty base it lists every file in head.
func (g Git) ChangedFiles(ctx context.Context, base, head string) ([]string, error) {
	rng, err := g.diffRange(ctx, base, head)
	if err != nil {
		return nil, err
	}
	out, err := g.Run(ctx, append([]string{"diff", "--name-only", "--no-renames", "-z"}, rng...)...)
	if err != nil {
		return nil, err
	}
	var files []string
	for f := range strings.SplitSeq(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	slices.Sort(files)
	return files, nil
}

// Diff returns the unified diff of head since base, without colour, external
// diff drivers or rename detection, so internal/classify can parse it.
func (g Git) Diff(ctx context.Context, base, head string) (string, error) {
	rng, err := g.diffRange(ctx, base, head)
	if err != nil {
		return "", err
	}
	return g.Run(ctx, append([]string{"diff", "--no-color", "--no-ext-diff", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/"}, rng...)...)
}

// BlobSizes returns the size in bytes of each path at rev, in one git call.
// A path that does not exist at rev is left out of the result.
func (g Git) BlobSizes(ctx context.Context, rev string, paths []string) (map[string]int, error) {
	if err := checkArgs(rev); err != nil {
		return nil, err
	}
	sizes := map[string]int{}
	if len(paths) == 0 {
		return sizes, nil
	}
	var in strings.Builder
	for _, p := range paths {
		if strings.ContainsAny(p, "\n") {
			return nil, fmt.Errorf("%w: path with a newline %q", ErrBadArg, p)
		}
		in.WriteString(rev + ":" + p + "\n")
	}
	// One output line per input line, in order: "<hash> <type> <size>" or
	// "<input> missing".
	out, err := g.RunInput(ctx, strings.NewReader(in.String()), "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(out, "\n")
	for i, p := range paths {
		if i >= len(lines) {
			break
		}
		fields := strings.Fields(lines[i])
		if len(fields) != 3 || fields[1] != "blob" {
			continue
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("git cat-file: bad size %q for %s", fields[2], p)
		}
		sizes[p] = n
	}
	return sizes, nil
}

// BlameLine returns the commit that last changed line n (1-based) of file
// at HEAD.
func (g Git) BlameLine(ctx context.Context, file string, n int) (string, error) {
	if n < 1 {
		return "", fmt.Errorf("line must be >= 1, got %d", n)
	}
	l := strconv.Itoa(n)
	out, err := g.Run(ctx, "blame", "--porcelain", "-L", l+","+l, "HEAD", "--", file)
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	if !sc.Scan() {
		return "", fmt.Errorf("git blame returned nothing for %s:%d", file, n)
	}
	sha, _, _ := strings.Cut(sc.Text(), " ")
	if strings.Trim(sha, "0") == "" {
		return "", fmt.Errorf("%s:%d is not committed yet", file, n)
	}
	return sha, nil
}

// Commit is one entry from Commits.
type Commit struct {
	SHA string `json:"sha"`
	// Parent is the first parent, or "" for a root commit.
	Parent  string `json:"parent,omitempty"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
}

// Commits lists up to limit commits reachable from rev along first parents,
// newest first. rev may be a range such as "abc..HEAD". limit <= 0 means no
// limit.
func (g Git) Commits(ctx context.Context, rev string, limit int) ([]Commit, error) {
	if err := checkArgs(rev); err != nil {
		return nil, err
	}
	args := []string{"log", "--first-parent", "--no-color", "--format=%H%x00%P%x00%an%x00%s%x1e"}
	if limit > 0 {
		args = append(args, "-n", strconv.Itoa(limit))
	}
	out, err := g.Run(ctx, append(args, rev, "--")...)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for rec := range strings.SplitSeq(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x00", 4)
		if len(f) != 4 {
			return nil, fmt.Errorf("git log: unexpected record %q", rec)
		}
		parent, _, _ := strings.Cut(f[1], " ")
		commits = append(commits, Commit{SHA: f[0], Parent: parent, Author: f[2], Subject: f[3]})
	}
	return commits, nil
}

// CurrentBranch returns the branch HEAD is on, or "" when HEAD is detached.
func (g Git) CurrentBranch(ctx context.Context) (string, error) {
	out, err := g.Run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode == 1 {
			return "", nil // detached
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Blob is a file object in the repository, with the path it was first seen
// at in the history that was scanned.
type Blob struct {
	Hash string `json:"hash"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// LargeBlobs lists the blobs reachable from rev (and, with tags, from every
// tag) that are larger than limit bytes, biggest first. A remote with a
// per-object cap (Artifacts: 32 MB) rejects a push holding any of them
// without naming the file; this names it before anything is pushed.
func (g Git) LargeBlobs(ctx context.Context, limit int64, rev string, tags bool) ([]Blob, error) {
	if err := checkArgs(rev); err != nil {
		return nil, err
	}
	args := []string{"rev-list", "--objects", rev}
	if tags {
		args = append(args, "--tags")
	}
	objects, err := g.Run(ctx, append(args, "--")...)
	if err != nil {
		return nil, err
	}
	// Each rev-list line is "<hash> <path>"; cat-file keeps the path in %(rest).
	out, err := g.RunInput(ctx, strings.NewReader(objects+"\n"), "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize) %(rest)")
	if err != nil {
		return nil, err
	}
	var big []Blob
	for line := range strings.Lines(out) {
		f := strings.SplitN(strings.TrimRight(line, "\n"), " ", 4)
		if len(f) < 3 || f[1] != "blob" {
			continue
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || size <= limit {
			continue
		}
		b := Blob{Hash: f[0], Size: size}
		if len(f) == 4 {
			b.Path = f[3]
		}
		big = append(big, b)
	}
	slices.SortStableFunc(big, func(a, b Blob) int { return cmp.Compare(b.Size, a.Size) })
	return big, nil
}

// ObjectsSize returns the bytes the repository's objects take on disk, loose
// and packed: an upper bound on what a push of everything would send.
func (g Git) ObjectsSize(ctx context.Context) (int64, error) {
	out, err := g.Run(ctx, "count-objects", "-v")
	if err != nil {
		return 0, err
	}
	var total int64
	for line := range strings.Lines(out) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if !ok || (k != "size" && k != "size-pack") {
			continue
		}
		kib, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("count-objects: %q: %w", strings.TrimSpace(line), err)
		}
		total += kib * 1024
	}
	return total, nil
}

// RemoteURL returns the fetch URL of the named remote, or ok=false when the
// repository has no such remote.
func (g Git) RemoteURL(ctx context.Context, name string) (string, bool, error) {
	if err := checkArgs(name); err != nil {
		return "", false, err
	}
	out, err := g.Run(ctx, "config", "--local", "--get", "remote."+name+".url")
	if err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.ExitCode == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(out), true, nil
}

// AddRemote adds a named remote for url. The URL must carry no credential:
// it lands in .git/config.
func (g Git) AddRemote(ctx context.Context, name, url string) error {
	if err := checkArgs(name, url); err != nil {
		return err
	}
	if artifactsToken.MatchString(url) || strings.Contains(url, "@") {
		return errors.New("gitx: a remote URL must not carry a credential")
	}
	_, err := g.Run(ctx, "remote", "add", name, url)
	return err
}
