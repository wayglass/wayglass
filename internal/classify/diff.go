package classify

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParseUnifiedDiff reads `git diff` output (unified format, with or without
// the `diff --git` headers) and returns one FileChange per file, in order.
// Hunk line counts from the @@ headers decide where a hunk ends, so added
// lines that begin with "++" or "--" are not mistaken for file headers.
func ParseUnifiedDiff(r io.Reader) ([]FileChange, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)

	var (
		files     []FileChange
		cur       *FileChange
		oldLeft   int // lines of the current hunk still expected on the old side
		newLeft   int
		lineNo    int
		inHunk    bool
		minusPath string
		deleted   bool // the current file has no new side
	)
	flush := func() {
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}
	start := func(path string) {
		flush()
		cur = &FileChange{Path: path}
		deleted = false
	}

	for sc.Scan() {
		lineNo++
		line := sc.Text()

		if inHunk {
			switch {
			case strings.HasPrefix(line, `\`): // "\ No newline at end of file"
				continue
			case strings.HasPrefix(line, "+"):
				cur.Added = append(cur.Added, line[1:])
				newLeft--
			case strings.HasPrefix(line, "-"):
				cur.Deleted++
				oldLeft--
			default: // context line (" " prefix, or empty when trailing whitespace was trimmed)
				oldLeft--
				newLeft--
			}
			if oldLeft <= 0 && newLeft <= 0 {
				inHunk = false
			}
			continue
		}

		switch {
		case strings.HasPrefix(line, "diff --git "):
			start(pathFromDiffGit(line))
		case strings.HasPrefix(line, "--- "):
			minusPath = stripPrefix(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "+++ "):
			p := stripPrefix(strings.TrimPrefix(line, "+++ "))
			gone := p == "/dev/null"
			if gone {
				p = minusPath
			}
			if cur == nil {
				start(p)
			} else if p != "" {
				cur.Path = p
			}
			deleted = gone
		case strings.HasPrefix(line, "deleted file mode "):
			deleted = true
		case strings.HasPrefix(line, "Binary files ") || line == "GIT binary patch":
			// A deleted binary brings nothing new to scan; only a binary new
			// side is unscanned content. Git prints no ---/+++ pair for
			// binaries, so the delete shows in the mode line or as a
			// /dev/null new side here.
			if cur != nil && !deleted && !strings.HasSuffix(line, " and /dev/null differ") {
				cur.Binary = true
			}
		case strings.HasPrefix(line, "@@ "):
			if cur == nil {
				return nil, fmt.Errorf("diff line %d: hunk before any file header", lineNo)
			}
			o, n, err := hunkCounts(line)
			if err != nil {
				return nil, fmt.Errorf("diff line %d: %w", lineNo, err)
			}
			oldLeft, newLeft, inHunk = o, n, o > 0 || n > 0
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	flush()
	return files, nil
}

// pathFromDiffGit takes the b/ side of "diff --git a/x b/y". Git quotes a
// path with non-ASCII, control, quote or backslash characters in C style
// (`"b/caf\303\251.md"`), so the quoted form is unquoted first: a tagger
// must see the same path the Worker reads from the tree, or `docs/**` would
// miss "docs/café.md" in the preview.
func pathFromDiffGit(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	if i := strings.LastIndex(rest, ` "b/`); i >= 0 {
		if p, ok := unquotePath(rest[i+1:]); ok {
			return strings.TrimPrefix(p, "b/")
		}
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return rest
}

func stripPrefix(p string) string {
	if q, ok := unquotePath(p); ok {
		p = q
	} else {
		p, _, _ = strings.Cut(p, "\t") // git appends a tab and timestamp in some modes
	}
	if p == "/dev/null" {
		return p
	}
	if rest, ok := strings.CutPrefix(p, "a/"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(p, "b/"); ok {
		return rest
	}
	return p
}

// unquotePath decodes a path git printed in C style: wrapped in double
// quotes, with \\, \", \a \b \f \n \r \t \v and three-digit octal escapes.
// It reports false when s is not a well-formed quoted string, so an unquoted
// path is left alone.
func unquotePath(s string) (string, bool) {
	if len(s) < 2 || s[0] != '"' {
		return "", false
	}
	var out []byte
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			if i != len(s)-1 {
				return "", false // text after the closing quote
			}
			return string(out), true
		case c != '\\':
			out = append(out, c)
			continue
		}
		i++
		if i >= len(s) {
			return "", false
		}
		switch e := s[i]; e {
		case '\\', '"':
			out = append(out, e)
		case 'a':
			out = append(out, '\a')
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'v':
			out = append(out, '\v')
		case '0', '1', '2', '3':
			if i+2 >= len(s) {
				return "", false
			}
			n, err := strconv.ParseUint(s[i:i+3], 8, 8)
			if err != nil {
				return "", false
			}
			out = append(out, byte(n))
			i += 2
		default:
			return "", false
		}
	}
	return "", false // no closing quote
}

// hunkCounts parses "@@ -a,b +c,d @@" and returns (b, d); a missing count is 1.
func hunkCounts(line string) (int, int, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return 0, 0, fmt.Errorf("malformed hunk header %q", line)
	}
	o, err := rangeCount(fields[1][1:])
	if err != nil {
		return 0, 0, err
	}
	n, err := rangeCount(fields[2][1:])
	if err != nil {
		return 0, 0, err
	}
	return o, n, nil
}

func rangeCount(s string) (int, error) {
	_, count, ok := strings.Cut(s, ",")
	if !ok {
		return 1, nil
	}
	n, err := strconv.Atoi(count)
	if err != nil {
		return 0, fmt.Errorf("bad hunk range %q", s)
	}
	return n, nil
}
