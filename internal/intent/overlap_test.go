package intent

import (
	"slices"
	"testing"
)

// Cases kept in step with worker/test/paths.test.ts.
func TestPatternsIntersect(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"web/login.ts", "web/login.ts", true},
		{"web/login.ts", "web/logout.ts", false},
		{"web/**", "web/login.ts", true},
		{"web/*.ts", "web/sub/login.ts", false},
		{"**/login*", "web/login.ts", true},
		{"**/*.md", "README.md", true},
		{"docs/*.md", "src/**", false},
		{"**/*.md", "src/**", true},
		{"web/", "web/a/b.ts", true},
		{"./web/login.ts", "web/login.ts", true},
		{"worker/src/limits/**", "web/login.ts", false},
		{"web/log?n.ts", "web/login.ts", true},
	}
	for _, tt := range tests {
		if got := PatternsIntersect(tt.a, tt.b); got != tt.want {
			t.Errorf("PatternsIntersect(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
		if got := PatternsIntersect(tt.b, tt.a); got != tt.want {
			t.Errorf("PatternsIntersect(%q, %q) = %v, want %v (symmetry)", tt.b, tt.a, got, tt.want)
		}
	}
}

func TestIntersectAndPaths(t *testing.T) {
	got := Intersect([]string{"web/**", "docs/a.md", "web/**"}, []string{"web/login.ts", "README.md"})
	if len(got) != 1 || got[0] != (PathPair{Mine: "web/**", Theirs: "web/login.ts"}) {
		t.Fatalf("Intersect = %+v", got)
	}
	if p := Paths([]string{"b", "a/"}, []string{"b", "c"}); len(p) != 3 || p[0] != "a/**" {
		t.Fatalf("Paths = %v", p)
	}
}

func TestUndeclared(t *testing.T) {
	tests := []struct {
		declared, files, want []string
	}{
		{[]string{"web/**"}, []string{"web/login.ts", "web/a/b.ts"}, []string{}},
		{[]string{"web/"}, []string{"./web/login.ts", "README.md"}, []string{"README.md"}},
		{[]string{"internal/cli/change.go"}, []string{"internal/cli/land.go", "internal/cli/change.go"}, []string{"internal/cli/land.go"}},
		{[]string{"**/*.md"}, []string{"docs/a.md", "go.mod"}, []string{"go.mod"}},
		{nil, []string{"b", "a", "b"}, []string{"a", "b"}},
		{[]string{"web/**"}, nil, []string{}},
	}
	for _, tt := range tests {
		got := Undeclared(tt.declared, tt.files)
		if !slices.Equal(got, tt.want) {
			t.Errorf("Undeclared(%v, %v) = %v, want %v", tt.declared, tt.files, got, tt.want)
		}
	}
}
