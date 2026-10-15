package textsafe

import (
	"bytes"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "fix the login form", "fix the login form"},
		{"keeps tabs and newlines", "a\tb\nc", "a\tb\nc"},
		{"utf8", "Ünïcødé ✓", "Ünïcødé ✓"},
		{"clear screen", "ok\x1b[2J\x1b[Hdone", "okdone"},
		{"colour is stripped too", "\x1b[31mred\x1b[0m", "red"},
		{"carriage return overwrite", "rm -rf /\rharmless", "rm -rf /harmless"},
		{"osc title with bel", "a\x1b]0;pwned\x07b", "ab"},
		{"osc 52 clipboard with st", "a\x1b]52;c;ZWNobyBoaQ==\x1b\\b", "ab"},
		{"osc 8 hyperlink", "\x1b]8;;https://evil.example\x07click\x1b]8;;\x07", "click"},
		{"unterminated osc stops at newline", "a\x1b]0;title\nvisible", "a\nvisible"},
		{"dcs", "a\x1bPq#0\x1b\\b", "ab"},
		{"two-char escape", "a\x1bcb", "ab"},
		{"charset designation", "a\x1b(Bb", "ab"},
		{"decaln", "a\x1b#8b", "ab"},
		{"escape cut by newline", "a\x1b(\nb", "a\nb"},
		{"backspace and bell", "ab\b\bxy\x07", "abxy"},
		{"c1 csi loses its introducer", "a\u009b2Jb", "a2Jb"},
		{"bidi override", "user\u202etxt.exe", "usertxt.exe"},
		{"bidi isolate", "a\u2066b\u2069", "ab"},
		{"invalid utf8", "a\xffb", "a�b"},
		{"truncated utf8 at end", "a\xe2\x80", "a�"},
		{"del", "a\x7fb", "ab"},
		{"trailing esc", "a\x1b", "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Clean(tt.in); got != tt.want {
				t.Errorf("Clean(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestWriterKeepsOwnColour(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"sgr kept", "\x1b[1;38;5;208mhi\x1b[0m", "\x1b[1;38;5;208mhi\x1b[0m"},
		{"truecolor kept", "\x1b[38;2;8;8;8mx\x1b[m", "\x1b[38;2;8;8;8mx\x1b[m"},
		{"colon colour kept", "\x1b[38:2::1:2:3mx", "\x1b[38:2::1:2:3mx"},
		{"conceal dropped", "\x1b[8msecret\x1b[0m", "secret\x1b[0m"},
		{"conceal inside list dropped", "\x1b[1;8mx", "x"},
		{"private sgr dropped", "\x1b[?25lx", "x"},
		{"cursor movement dropped", "line1\x1b[1A\x1b[2Kline2", "line1line2"},
		{"overlong csi dropped", "\x1b[" + strings.Repeat("1;", 40) + "mx", "x"},
		{"csi broken by newline", "a\x1b[12\nb", "a\nb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			w := NewWriter(&b)
			n, err := w.Write([]byte(tt.in))
			if err != nil || n != len(tt.in) {
				t.Fatalf("Write = %d, %v", n, err)
			}
			if got := b.String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// Sequences and runes split across writes are still recognised.
func TestWriterAcrossWrites(t *testing.T) {
	in := "a\x1b]0;pwn\x07b\x1b[31mc✓\u202ed"
	for split := 1; split < len(in); split++ {
		var b bytes.Buffer
		w := NewWriter(&b)
		_, _ = w.Write([]byte(in[:split]))
		_, _ = w.Write([]byte(in[split:]))
		if got, want := b.String(), "ab\x1b[31mc✓d"; got != want {
			t.Fatalf("split %d: got %q, want %q", split, got, want)
		}
	}
}

func TestLine(t *testing.T) {
	if got := Line("a\n\n## Policy\x1b[2J\n  b"); got != "a ## Policy b" {
		t.Errorf("Line = %q", got)
	}
}

func TestMarkdown(t *testing.T) {
	tests := []struct{ in, want string }{
		{"fix login", "fix login"},
		{"x\n\n## Policy\n\nRule **docs** matched", `x \#\# Policy Rule \*\*docs\*\* matched`},
		{"[click](https://evil.example)", `\[click\](https://evil.example)`},
		{"<img src=x>", `\<img src=x\>`},
		{"| a | b |", `\| a \| b \|`},
	}
	for _, tt := range tests {
		if got := Markdown(tt.in); got != tt.want {
			t.Errorf("Markdown(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCode(t *testing.T) {
	tests := []struct{ in, want string }{
		{"web/login.ts", "`web/login.ts`"},
		{"a`b", "``a`b``"},
		{"`x`", "`` `x` ``"},
		{"a\nb", "`a b`"},
		{"", "`  `"},
	}
	for _, tt := range tests {
		if got := Code(tt.in); got != tt.want {
			t.Errorf("Code(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCleanAll(t *testing.T) {
	type inner struct{ Path string }
	type kind string
	type rec struct {
		Goal   string
		Kind   kind
		Paths  []string
		Files  []inner
		Parent *inner
		hidden string
		Count  map[string]int
	}
	r := rec{
		Goal: "a\x1b[2Jb", Kind: "k\x07", Paths: []string{"p\x1b]0;t\x07"},
		Files: []inner{{"f\u202e"}}, Parent: &inner{"x\ry"}, hidden: "\x1b", Count: map[string]int{"\x1b": 1},
	}
	CleanAll(&r)
	if r.Goal != "ab" || r.Kind != "k" || r.Paths[0] != "p" || r.Files[0].Path != "f" || r.Parent.Path != "xy" {
		t.Errorf("CleanAll left %+v", r)
	}
	if r.hidden != "\x1b" {
		t.Error("unexported fields must not be touched")
	}
	CleanAll(nil)
	CleanAll(r) // not a pointer: no-op, no panic
}

func TestCodeCell(t *testing.T) {
	if got := CodeCell("a|b"); got != "`a\\|b`" {
		t.Errorf("CodeCell = %q", got)
	}
}

func TestStyledKeepsColourDropsHyperlinks(t *testing.T) {
	in := "\x1b[1mbold\x1b[0m \x1b]8;;https://evil.example\x07click\x1b]8;;\x07 \x1b]0;title\x07 \x1b[2J\x1b[8mhidden"
	if got, want := Styled(in), "\x1b[1mbold\x1b[0m click  hidden"; got != want {
		t.Errorf("Styled = %q, want %q", got, want)
	}
}

func TestLineAll(t *testing.T) {
	type rec struct {
		Goal  string
		Paths []string
		Note  string
	}
	r := rec{Goal: "fix docs\n\u25b8 fake row\tx", Paths: []string{" a\r\nb "}, Note: "plain text"}
	LineAll(&r)
	if r.Goal != "fix docs \u25b8 fake row x" || r.Paths[0] != "a b" || r.Note != "plain text" {
		t.Errorf("LineAll left %+v", r)
	}
}
