package intent

import (
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Overlap rules mirror worker/src/paths.ts so `wayglass overlaps` agrees
// with what the Worker reports on declare and push:
//   - path vs path: equal
//   - glob vs path: the glob matches the path
//   - glob vs glob: conservative; they overlap unless their literal prefixes
//     diverge. A false positive costs a warning, a false negative a conflict.

// PathPair is one intersecting pair of patterns.
type PathPair struct {
	Mine   string `json:"mine"`
	Theirs string `json:"theirs"`
}

// Normalize cleans a path pattern the way the Worker does: forward slashes,
// no "./" prefix, and a trailing "/" meaning everything under it.
func Normalize(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	for {
		rest, ok := strings.CutPrefix(p, "./")
		if !ok {
			break
		}
		p = rest
	}
	if strings.HasSuffix(p, "/") {
		p += "**"
	}
	return p
}

func isGlob(p string) bool { return strings.ContainsAny(p, "*?") }

func staticPrefix(p string) string {
	if i := strings.IndexAny(p, "*?"); i >= 0 {
		return p[:i]
	}
	return p
}

func match(glob, path string) bool {
	ok, err := doublestar.Match(glob, path)
	return err == nil && ok
}

// PatternsIntersect reports whether two patterns could name the same file.
func PatternsIntersect(a, b string) bool {
	a, b = Normalize(a), Normalize(b)
	ga, gb := isGlob(a), isGlob(b)
	switch {
	case !ga && !gb:
		return a == b
	case ga && !gb:
		return match(a, b)
	case !ga && gb:
		return match(b, a)
	}
	pa, pb := staticPrefix(a), staticPrefix(b)
	return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
}

// Intersect returns every intersecting pair between two pattern sets.
func Intersect(mine, theirs []string) []PathPair {
	var out []PathPair
	for _, m := range dedupe(mine) {
		for _, t := range dedupe(theirs) {
			if PatternsIntersect(m, t) {
				out = append(out, PathPair{Mine: m, Theirs: t})
			}
		}
	}
	return out
}

// Paths joins declared and actual paths, deduplicated and sorted.
func Paths(declared, actual []string) []string {
	return dedupe(append(slices.Clone(declared), actual...))
}

func dedupe(ps []string) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, Normalize(p))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Undeclared returns the changed files that no declared pattern covers,
// normalized and sorted. An intent with no declared paths covers nothing.
func Undeclared(declared, files []string) []string {
	ds := dedupe(declared)
	out := []string{}
	for _, f := range dedupe(files) {
		if !slices.ContainsFunc(ds, func(d string) bool { return PatternsIntersect(d, f) }) {
			out = append(out, f)
		}
	}
	return out
}
