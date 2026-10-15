package classify

import (
	"slices"
	"strings"
	"testing"
)

// exampleConfig mirrors the classify section of examples/policy.yaml.
func exampleConfig() Config {
	return Config{
		Area: map[string][]string{
			"docs":  {"docs/**", "**/*.md"},
			"web":   {"web/**"},
			"auth":  {"**/auth/**", "**/login*"},
			"infra": {"worker/wrangler.jsonc", ".github/**"},
		},
		DataClass: map[string]DataClassRule{
			"secrets": {Content: []string{`(?i)api[_-]?key`, `art_v[0-9]+_([a-z]+_)?[0-9a-f]{40}`}},
			"pii":     {Content: []string{`(?i)\bssn\b`, `(?i)date_of_birth`}},
		},
		Risk: map[string]RiskRule{
			"high": {LinesChangedOver: 400},
		},
	}
}

func lines(n int, s string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestClassify(t *testing.T) {
	tagger, err := New(exampleConfig())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		change       Change
		areas        []string
		dataClasses  []string
		risk         string
		jurisdiction string
		linesChanged int
	}{
		{
			name:        "root markdown matches docs via **/*.md",
			change:      Change{Files: []FileChange{{Path: "README.md", Added: []string{"hello"}}}},
			areas:       []string{"docs"},
			dataClasses: []string{},
			risk:        RiskLow, linesChanged: 1,
		},
		{
			name:        "nested docs path",
			change:      Change{Files: []FileChange{{Path: "docs/guide/setup.txt", Added: []string{"x"}, Deleted: 2}}},
			areas:       []string{"docs"},
			dataClasses: []string{},
			risk:        RiskLow, linesChanged: 3,
		},
		{
			name:        "login file under web is web and auth",
			change:      Change{Files: []FileChange{{Path: "web/login.ts", Added: []string{"const x = 1"}}}},
			areas:       []string{"auth", "web"},
			dataClasses: []string{},
			risk:        RiskLow, linesChanged: 1,
		},
		{
			name:        "auth directory at any depth",
			change:      Change{Files: []FileChange{{Path: "internal/auth/token.go", Added: []string{"package auth"}}}},
			areas:       []string{"auth"},
			dataClasses: []string{},
			risk:        RiskLow, linesChanged: 1,
		},
		{
			name:        "auth directory at the root via ** matching zero dirs",
			change:      Change{Files: []FileChange{{Path: "auth/x.go"}}},
			areas:       []string{"auth"},
			dataClasses: []string{},
			risk:        RiskLow,
		},
		{
			name:        "infra exact path and github dir",
			change:      Change{Files: []FileChange{{Path: "worker/wrangler.jsonc"}, {Path: ".github/workflows/ci.yml"}}},
			areas:       []string{"infra"},
			dataClasses: []string{},
			risk:        RiskLow,
		},
		{
			name:        "worker source is not infra",
			change:      Change{Files: []FileChange{{Path: "worker/src/index.ts", Added: []string{"export {}"}}}},
			areas:       []string{},
			dataClasses: []string{},
			risk:        RiskLow, linesChanged: 1,
		},
		{
			name:        "leading ./ is ignored",
			change:      Change{Files: []FileChange{{Path: "./web/index.html"}}},
			areas:       []string{"web"},
			dataClasses: []string{},
			risk:        RiskLow,
		},
		{
			name: "pii by content regex, eu jurisdiction normalised",
			change: Change{Jurisdiction: " EU ", Files: []FileChange{{
				Path: "web/profile.ts", Added: []string{"const f = {", "  date_of_birth: input.dob,", "}"},
			}}},
			areas:        []string{"web"},
			dataClasses:  []string{"pii"},
			risk:         RiskLow,
			jurisdiction: "eu",
			linesChanged: 3,
		},
		{
			name:        "ssn needs a word boundary",
			change:      Change{Files: []FileChange{{Path: "x.go", Added: []string{"lessness := true"}}}},
			areas:       []string{},
			dataClasses: []string{},
			risk:        RiskLow, linesChanged: 1,
		},
		{
			name:        "ssn word matches case-insensitively",
			change:      Change{Files: []FileChange{{Path: "x.go", Added: []string{"user.SSN = v"}}}},
			areas:       []string{},
			dataClasses: []string{"pii"},
			risk:        RiskLow, linesChanged: 1,
		},
		{
			name: "secrets by artifacts token pattern",
			change: Change{Files: []FileChange{{
				Path: "scripts/run.sh", Added: []string{"T=art_v1_" + strings.Repeat("ab", 20)},
			}}},
			areas:       []string{},
			dataClasses: []string{"secrets"},
			risk:        RiskLow, linesChanged: 1,
		},
		{
			name: "secrets by current (v2) artifacts token format",
			change: Change{Files: []FileChange{{
				Path: "scripts/run.sh", Added: []string{"T=art_v2_u_" + strings.Repeat("ab", 20) + "?expires=1791788706"},
			}}},
			areas:       []string{},
			dataClasses: []string{"secrets"},
			risk:        RiskLow, linesChanged: 1,
		},
		{
			name:        "deleted lines are never scanned for data classes",
			change:      Change{Files: []FileChange{{Path: "x.go", Deleted: 1}}},
			areas:       []string{},
			dataClasses: []string{},
			risk:        RiskLow, linesChanged: 1,
		},
		{
			name:        "exactly 400 lines is still low",
			change:      Change{Files: []FileChange{{Path: "a.go", Added: lines(400, "x")}}},
			areas:       []string{},
			dataClasses: []string{},
			risk:        RiskLow, linesChanged: 400,
		},
		{
			name:        "401 lines across files is high",
			change:      Change{Files: []FileChange{{Path: "a.go", Added: lines(300, "x")}, {Path: "b.go", Deleted: 101}}},
			areas:       []string{},
			dataClasses: []string{},
			risk:        RiskHigh, linesChanged: 401,
		},
		{
			name:        "empty change",
			change:      Change{},
			areas:       []string{},
			dataClasses: []string{},
			risk:        RiskLow,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tagger.Classify(tt.change)
			if !slices.Equal(got.Areas, tt.areas) {
				t.Errorf("areas = %v, want %v", got.Areas, tt.areas)
			}
			if !slices.Equal(got.DataClasses, tt.dataClasses) {
				t.Errorf("data classes = %v, want %v", got.DataClasses, tt.dataClasses)
			}
			if got.Risk != tt.risk {
				t.Errorf("risk = %q, want %q", got.Risk, tt.risk)
			}
			if got.Jurisdiction != tt.jurisdiction {
				t.Errorf("jurisdiction = %q, want %q", got.Jurisdiction, tt.jurisdiction)
			}
			if got.LinesChanged != tt.linesChanged {
				t.Errorf("lines changed = %d, want %d", got.LinesChanged, tt.linesChanged)
			}
			// Every area and data class must be explained by evidence.
			for _, a := range got.Areas {
				if !hasEvidence(got, "area", a) {
					t.Errorf("no evidence for area %q", a)
				}
			}
			for _, d := range got.DataClasses {
				if !hasEvidence(got, "data_class", d) {
					t.Errorf("no evidence for data class %q", d)
				}
			}
		})
	}
}

func hasEvidence(tags Tags, tag, value string) bool {
	return slices.ContainsFunc(tags.Evidence, func(e Evidence) bool { return e.Tag == tag && e.Value == value })
}

func TestEvidenceNeverCopiesMatchedText(t *testing.T) {
	tagger, err := New(exampleConfig())
	if err != nil {
		t.Fatal(err)
	}
	secret := "art_v1_" + strings.Repeat("0f", 20)
	tags := tagger.Classify(Change{Files: []FileChange{{Path: "a.env", Added: []string{"ok", "TOKEN=" + secret}}}})
	for _, e := range tags.Evidence {
		if strings.Contains(e.Path+e.Pattern+e.Detail+e.Value, secret) {
			t.Fatalf("evidence leaks the secret: %+v", e)
		}
		if e.Tag == "data_class" && e.Line != 2 {
			t.Fatalf("evidence line = %d, want 2", e.Line)
		}
	}
}

func TestRiskLevelsPickHighestCrossed(t *testing.T) {
	tagger, err := New(Config{Risk: map[string]RiskRule{
		"medium":   {LinesChangedOver: 50},
		"high":     {LinesChangedOver: 400, FilesChangedOver: 20},
		"critical": {LinesChangedOver: 2000},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		lines, files int
		want         string
	}{
		{10, 1, RiskLow},
		{51, 1, RiskMedium},
		{401, 1, RiskHigh},
		{10, 21, RiskHigh},
		{2001, 1, RiskCritical},
	}
	for _, tt := range tests {
		files := make([]FileChange, tt.files)
		for i := range files {
			files[i].Path = "f" + strings.Repeat("x", i)
		}
		files[0].Deleted = tt.lines
		if got := tagger.Classify(Change{Files: files}).Risk; got != tt.want {
			t.Errorf("lines=%d files=%d: risk %q, want %q", tt.lines, tt.files, got, tt.want)
		}
	}
}

func TestDataClassByPath(t *testing.T) {
	tagger, err := New(Config{DataClass: map[string]DataClassRule{
		"secrets": {Paths: []string{"**/.env", "**/*.pem"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{".env": true, "app/.env": true, "certs/key.pem": true, "env.go": false} {
		got := tagger.Classify(Change{Files: []FileChange{{Path: path}}}).HasDataClass("secrets")
		if got != want {
			t.Errorf("%s: secrets = %v, want %v", path, got, want)
		}
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"bad regex", Config{DataClass: map[string]DataClassRule{"x": {Content: []string{"("}}}}, "bad regex"},
		{"bad glob", Config{Area: map[string][]string{"x": {"[a"}}}, "invalid glob"},
		{"empty area", Config{Area: map[string][]string{"x": {}}}, "at least one glob"},
		{"empty data class", Config{DataClass: map[string]DataClassRule{"x": {}}}, "needs content or paths"},
		{"unknown risk level", Config{Risk: map[string]RiskRule{"extreme": {LinesChangedOver: 1}}}, "unknown level"},
		{"low takes no threshold", Config{Risk: map[string]RiskRule{"low": {LinesChangedOver: 1}}}, "default level"},
		{"risk without threshold", Config{Risk: map[string]RiskRule{"high": {}}}, "needs lines_changed_over"},
		{"negative threshold", Config{Risk: map[string]RiskRule{"high": {LinesChangedOver: -1}}}, "negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseUnifiedDiff(t *testing.T) {
	diff := `diff --git a/web/login.ts b/web/login.ts
index 1111111..2222222 100644
--- a/web/login.ts
+++ b/web/login.ts
@@ -1,3 +1,4 @@
 import x from "y";
-const a = 1;
+const a = 2;
+++counter;
 export {};
diff --git a/docs/old.md b/docs/old.md
deleted file mode 100644
--- a/docs/old.md
+++ /dev/null
@@ -1,2 +0,0 @@
-one
---two
diff --git a/img.png b/img.png
new file mode 100644
Binary files /dev/null and b/img.png differ
diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+hello
\ No newline at end of file
diff --git a/gone.bin b/gone.bin
deleted file mode 100644
Binary files a/gone.bin and /dev/null differ
diff --git a/swap.bin b/swap.bin
index 1111111..2222222 100644
Binary files a/swap.bin and b/swap.bin differ
diff --git a/gone2.bin b/gone2.bin
deleted file mode 100644
index 1111111..0000000
GIT binary patch
literal 0
HcmV?d00001

diff --git a/notes.txt b/notes.txt
new file mode 100644
index 0000000..2222222
GIT binary patch
literal 12
TcmZQzU|?VXVn$>{U}OLJ3

`
	files, err := ParseUnifiedDiff(strings.NewReader(diff))
	if err != nil {
		t.Fatal(err)
	}
	want := []FileChange{
		{Path: "web/login.ts", Added: []string{"const a = 2;", "++counter;"}, Deleted: 1},
		{Path: "docs/old.md", Deleted: 2},
		{Path: "img.png", Binary: true},
		{Path: "new.txt", Added: []string{"hello"}},
		{Path: "gone.bin"}, // a deleted binary brings nothing to scan
		{Path: "swap.bin", Binary: true},
		{Path: "gone2.bin"}, // the same, in --binary form
		{Path: "notes.txt", Binary: true},
	}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(files), len(want), files)
	}
	for i, w := range want {
		g := files[i]
		if g.Path != w.Path || g.Deleted != w.Deleted || g.Binary != w.Binary || !slices.Equal(g.Added, w.Added) {
			t.Errorf("file %d = %+v, want %+v", i, g, w)
		}
	}
}

// Git quotes paths with non-ASCII, quote or backslash characters in C style.
// The parser must return the path the Worker reads from the tree, or the
// preview tags a path like "docs/caf\303\251.md" that no glob matches.
func TestParseUnifiedDiffQuotedPaths(t *testing.T) {
	diff := `diff --git "a/docs/caf\303\251.md" "b/docs/caf\303\251.md"
index 1111111..2222222 100644
--- "a/docs/caf\303\251.md"
+++ "b/docs/caf\303\251.md"
@@ -1 +1,2 @@
 a
+a2
diff --git "a/docs/quo\"te.md" "b/docs/quo\"te.md"
--- "a/docs/quo\"te.md"
+++ "b/docs/quo\"te.md"
@@ -1 +1,2 @@
 b
+b2
diff --git "a/docs/back\\slash.md" "b/docs/back\\slash.md"
--- "a/docs/back\\slash.md"
+++ "b/docs/back\\slash.md"
@@ -1 +1,2 @@
 c
+c2
diff --git "a/docs/tab\tbed.md" "b/docs/tab\tbed.md"
deleted file mode 100644
--- "a/docs/tab\tbed.md"
+++ /dev/null
@@ -1 +0,0 @@
-gone
diff --git a/sp ace.md b/sp ace.md
deleted file mode 100644
--- a/sp ace.md
+++ /dev/null
@@ -1 +0,0 @@
-d
`
	files, err := ParseUnifiedDiff(strings.NewReader(diff))
	if err != nil {
		t.Fatal(err)
	}
	want := []FileChange{
		{Path: "docs/café.md", Added: []string{"a2"}},
		{Path: `docs/quo"te.md`, Added: []string{"b2"}},
		{Path: `docs/back\slash.md`, Added: []string{"c2"}},
		{Path: "docs/tab\tbed.md", Deleted: 1},
		{Path: "sp ace.md", Deleted: 1},
	}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d: %+v", len(files), len(want), files)
	}
	for i, w := range want {
		g := files[i]
		if g.Path != w.Path || g.Deleted != w.Deleted || !slices.Equal(g.Added, w.Added) {
			t.Errorf("file %d = %+v, want %+v", i, g, w)
		}
	}
	tagger, err := New(exampleConfig())
	if err != nil {
		t.Fatal(err)
	}
	if tags := tagger.Classify(Change{Files: files}); !tags.OnlyIn("docs") {
		t.Fatalf("quoted docs paths were not all tagged docs: %v", tags.AreaSets)
	}
}

func TestUnquotePath(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{`"a/b.md"`, "a/b.md", true},
		{`"caf\303\251"`, "café", true},
		{`"q\"t\\n\t"`, "q\"t\\n\t", true},
		{`"\a\b\f\n\r\v"`, "\a\b\f\n\r\v", true},
		{`a/b.md`, "", false},
		{`"unterminated`, "", false},
		{`"bad\q"`, "", false},
		{`"short\30"`, "", false},
		{`"\999"`, "", false},
		{`"a" trailing`, "", false},
		{`"`, "", false},
	}
	for _, tt := range tests {
		got, ok := unquotePath(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("unquotePath(%s) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// Paths an agent could shape to slip past a glob. Backslashes and "./" are
// normalised; case is not, and dotfiles are not special.
func TestPathMatchingEdges(t *testing.T) {
	tagger, err := New(exampleConfig())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path  string
		areas []string
	}{
		{`web\login.ts`, []string{"auth", "web"}},
		{`web\auth\token.ts`, []string{"auth", "web"}},
		{"././web/index.html", []string{"web"}},
		{"docs/.hidden.md", []string{"docs"}},
		{".github/.workflows/ci.yml", []string{"infra"}},
		{"docs/x.md/y", []string{"docs"}},
		{"Web/index.html", []string{}},
		{"internal/Auth/token.go", []string{}},
		{"README.MD", []string{}},
		{"auth", []string{"auth"}},
		{"web/login", []string{"auth", "web"}},
		{"web/login/x.ts", []string{"web"}},
		{"notdocs/README.txt", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := tagger.Classify(Change{Files: []FileChange{{Path: tt.path}}})
			if !slices.Equal(got.Areas, tt.areas) {
				t.Fatalf("areas = %v, want %v", got.Areas, tt.areas)
			}
		})
	}
}

// Content detectors only see added text. A binary file carries none, so a
// secret inside it is caught only by a paths detector; a deleted or renamed
// file contributes its new side only.
func TestContentDetectorsSeeOnlyAddedText(t *testing.T) {
	token := "art_v1_" + strings.Repeat("ab", 20)
	tagger, err := New(Config{DataClass: map[string]DataClassRule{
		"secrets": {Content: []string{`art_v[0-9]+_([a-z]+_)?[0-9a-f]{40}`}, Paths: []string{"**/*.pem"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		files []FileChange
		want  bool
	}{
		{"binary file with no text is not scanned", []FileChange{{Path: "scripts/run.sh", Binary: true}}, false},
		{"binary file is still tagged by path", []FileChange{{Path: "certs/key.pem", Binary: true}}, true},
		{"deleted file has no added text", []FileChange{{Path: "old.go", Deleted: 3}}, false},
		{"rename shows up as delete plus add and the add is scanned", []FileChange{
			{Path: "old.go", Deleted: 1}, {Path: "new.go", Added: []string{"t := \"" + token + "\""}},
		}, true},
		{"a secret split across two lines evades a single-line regex", []FileChange{
			{Path: "x.go", Added: []string{"t := \"art_v1_\" +", "\"" + strings.Repeat("ab", 20) + "\""}},
		}, false},
		{"base64 of a token evades the literal regex", []FileChange{
			{Path: "x.go", Added: []string{"t := decode(\"YXJ0X3YxX2FiYWJhYmFi\")"}},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tagger.Classify(Change{Files: tt.files}).HasDataClass("secrets"); got != tt.want {
				t.Fatalf("secrets = %v, want %v", got, tt.want)
			}
		})
	}
}

// A binary file is tagged unscanned with its path as evidence, and its size
// counts toward risk through binary_bytes_over, since it has no lines.
func TestUnscannedBinaries(t *testing.T) {
	tagger, err := New(Config{Risk: map[string]RiskRule{"high": {BinaryBytesOver: 1000}}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		files     []FileChange
		unscanned []string
		bytes     int
		risk      string
	}{
		{"text only", []FileChange{{Path: "a.go", Added: []string{"x"}}}, []string{}, 0, RiskLow},
		{"one small binary", []FileChange{{Path: "img.png", Binary: true, Bytes: 500}}, []string{"img.png"}, 500, RiskLow},
		{"a binary of unknown size", []FileChange{{Path: "blob.bin", Binary: true}}, []string{"blob.bin"}, 0, RiskLow},
		{"binaries add up", []FileChange{
			{Path: "a.png", Binary: true, Bytes: 600}, {Path: "b.go", Added: []string{"y"}}, {Path: "c.png", Binary: true, Bytes: 600},
		}, []string{"a.png", "c.png"}, 1200, RiskHigh},
		{"text bytes do not count", []FileChange{{Path: "a.go", Added: []string{"x"}, Bytes: 5000}}, []string{}, 0, RiskLow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tagger.Classify(Change{Files: tt.files})
			if !slices.Equal(got.Unscanned, tt.unscanned) || got.BinaryBytes != tt.bytes || got.Risk != tt.risk {
				t.Fatalf("unscanned = %v (%d bytes), risk %s; want %v (%d), %s", got.Unscanned, got.BinaryBytes, got.Risk, tt.unscanned, tt.bytes, tt.risk)
			}
			if got.Unscanned == nil {
				t.Fatal("unscanned must be a list, not null")
			}
			if got.HasUnscanned() != (len(tt.unscanned) > 0) {
				t.Fatalf("HasUnscanned = %v", got.HasUnscanned())
			}
			var paths []string
			for _, e := range got.Evidence {
				if e.Tag == "unscanned" {
					if e.Value != "binary" || e.Detail == "" {
						t.Fatalf("evidence = %+v", e)
					}
					paths = append(paths, e.Path)
				}
			}
			if !slices.Equal(paths, tt.unscanned) {
				t.Fatalf("evidence paths = %v, want %v", paths, tt.unscanned)
			}
		})
	}
	if _, err := New(Config{Risk: map[string]RiskRule{"high": {BinaryBytesOver: -1}}}); err == nil {
		t.Fatal("a negative binary_bytes_over must be refused")
	}
}

func TestParseUnifiedDiffRejectsOrphanHunk(t *testing.T) {
	if _, err := ParseUnifiedDiff(strings.NewReader("@@ -1 +1 @@\n-a\n+b\n")); err == nil {
		t.Fatal("expected error for hunk without file header")
	}
}

func TestAreaSetsAndOnlyIn(t *testing.T) {
	tagger, err := New(Config{Area: map[string][]string{
		"docs":  {"docs/**", "**/*.md"},
		"tests": {"**/*_test.go"},
		"app":   {"internal/**"},
		"auth":  {"internal/auth/**"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	files := func(paths ...string) Change {
		c := Change{}
		for _, p := range paths {
			c.Files = append(c.Files, FileChange{Path: p, Added: []string{"x"}})
		}
		return c
	}
	tests := []struct {
		name   string
		change Change
		sets   [][]string
		only   map[string]bool // OnlyIn(key split on ",") -> want
	}{
		{"docs only", files("docs/a.md", "README.md"), [][]string{{"docs"}},
			map[string]bool{"docs": true, "tests": false, "docs,tests": true}},
		{"docs plus code is not docs only", files("docs/a.md", "internal/shop/cart.go"), [][]string{{"app"}, {"docs"}},
			map[string]bool{"docs": false, "app": false, "app,docs": true}},
		{"tests only, also in app", files("internal/shop/cart_test.go", "internal/shop/price_test.go"), [][]string{{"app", "tests"}},
			map[string]bool{"tests": true, "app": true, "docs": false}},
		{"tests plus the code under test", files("internal/shop/cart_test.go", "internal/shop/cart.go"), [][]string{{"app"}, {"app", "tests"}},
			map[string]bool{"tests": false, "app": true}},
		{"a file in no area breaks every only", files("docs/a.md", "Makefile"), [][]string{{}, {"docs"}},
			map[string]bool{"docs": false, "app,docs,tests,auth": false}},
		{"auth test file is in auth too", files("internal/auth/login_test.go"), [][]string{{"app", "auth", "tests"}},
			map[string]bool{"tests": true, "auth": true}},
		{"no files is never only", Change{}, [][]string{},
			map[string]bool{"docs": false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tagger.Classify(tt.change)
			if !slices.EqualFunc(got.AreaSets, tt.sets, slices.Equal) {
				t.Fatalf("AreaSets = %v, want %v", got.AreaSets, tt.sets)
			}
			for areas, want := range tt.only {
				if g := got.OnlyIn(strings.Split(areas, ",")...); g != want {
					t.Errorf("OnlyIn(%s) = %v, want %v", areas, g, want)
				}
			}
		})
	}
}

func TestOnlyInWithoutAreaSets(t *testing.T) {
	// Tags recorded before AreaSets existed carry none; "only" must not
	// match them, so a permissive rule never fires on missing data.
	old := Tags{Areas: []string{"docs"}}
	if old.OnlyIn("docs") {
		t.Fatal("OnlyIn matched tags without AreaSets")
	}
}
