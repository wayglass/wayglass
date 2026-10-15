// Package textsafe keeps repo- and agent-written text from acting on the
// terminal that shows it.
//
// Goals, reasons, agent names, file paths, approval reasons, audit summaries
// and git notes are written by whoever declared, pushed or decided. Printed
// raw, an escape sequence in them can clear a reviewer's screen, overwrite a
// line, set the window title, write the clipboard (OSC 52) or hide text, and
// a bidi override can reorder what a person reads. These helpers treat that
// text as data: Clean strips it from single values, Writer filters a whole
// output stream while keeping the colour Wayglass itself adds, and Markdown
// escapes values placed into Markdown so they cannot fake headings or rules.
package textsafe

import (
	"strings"
	"unicode/utf8"
)

// unsafeRune reports runes no displayed value should carry: C0 controls
// other than tab and newline, DEL, C1 controls, and bidi embeddings,
// overrides and isolates.
func unsafeRune(r rune) bool {
	switch {
	case r == '\t' || r == '\n':
		return false
	case r < 0x20, r >= 0x7f && r <= 0x9f:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// Clean returns s without escape sequences and unsafe runes (see the
// package comment). Tabs and newlines are kept; invalid UTF-8 becomes U+FFFD.
func Clean(s string) string {
	if !needsClean(s) {
		return s
	}
	var b strings.Builder
	w := &Writer{w: &b, sgr: false}
	_, _ = w.Write([]byte(s))
	w.flush()
	return b.String()
}

func needsClean(s string) bool {
	for _, r := range s {
		if r == utf8.RuneError || r == 0x1b || unsafeRune(r) {
			return true
		}
	}
	return false
}

// Line is Clean, then every run of whitespace (newlines included) becomes
// one space: for values shown on a single line.
func Line(s string) string {
	return strings.Join(strings.Fields(Clean(s)), " ")
}

// Styled is Clean for text Wayglass styled itself (Lip Gloss, Glamour):
// SGR colour and style stay, everything else goes, hyperlinks (OSC 8) and
// window titles included. Use it on rendered output that carries text
// written by others and is drawn without the stream filter, as the TUI's
// output is.
func Styled(s string) string {
	var b strings.Builder
	w := &Writer{w: &b, sgr: true}
	_, _ = w.Write([]byte(s))
	w.flush()
	return b.String()
}

// Markdown escapes a cleaned, single-line value for use as inline Markdown
// text after fixed text on the same line, so it renders literally: no
// headings, emphasis, links, tables, code or HTML. Line-start syntax (lists,
// quotes) cannot occur because Line joins everything onto one line.
func Markdown(s string) string {
	s = Line(s)
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_[]<>#|~", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Code renders a cleaned, single-line value as a Markdown code span whose
// delimiter is longer than any backtick run inside it.
func Code(s string) string {
	s = Line(s)
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") || s == "" {
		s = " " + s + " "
	}
	return fence + s + fence
}

// CodeCell is Code for a GFM table cell: a pipe would end the cell even
// inside a code span, so it is escaped (tables unescape it).
func CodeCell(s string) string {
	return strings.ReplaceAll(Code(s), "|", `\|`)
}
