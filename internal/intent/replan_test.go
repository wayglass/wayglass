package intent

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSuggestReplan(t *testing.T) {
	t1 := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	side := func(id, status string, at time.Time, declared, actual []string) Side {
		return Side{ID: id, Status: status, CreatedAt: at, Declared: declared, Actual: actual}
	}
	tests := []struct {
		name        string
		a, b        Side
		action      string
		first, then string
		paths       []string
	}{
		{
			name:   "proposed side lands first even when newer",
			a:      side("iA", "open", t1, []string{"web/login.ts"}, []string{"web/login.ts"}),
			b:      side("iB", "proposed", t2, []string{"web/**"}, []string{"web/login.ts"}),
			action: ReplanLandFirst, first: "iB", then: "iA", paths: []string{"web/login.ts"},
		},
		{
			name:   "both proposed: older first",
			a:      side("iA", "proposed", t2, []string{"web/login.ts"}, nil),
			b:      side("iB", "proposed", t1, []string{"web/login.ts"}, nil),
			action: ReplanLandFirst, first: "iB", then: "iA", paths: []string{"web/login.ts"},
		},
		{
			name:   "both edited the file: stack newer on older",
			a:      side("iA", "open", t2, []string{"web/**"}, []string{"web/login.ts"}),
			b:      side("iB", "open", t1, []string{"web/login.ts"}, []string{"web/login.ts"}),
			action: ReplanStack, first: "iB", then: "iA", paths: []string{"web/login.ts"},
		},
		{
			name:   "only one edited: it lands first",
			a:      side("iA", "open", t1, []string{"web/login.ts"}, nil),
			b:      side("iB", "open", t2, []string{"web/**"}, []string{"web/login.ts"}),
			action: ReplanLandFirst, first: "iB", then: "iA", paths: []string{"web/login.ts"},
		},
		{
			name:   "same literal file declared, nothing pushed: older first",
			a:      side("iA", "open", t2, []string{"web/session.ts", "web/login.ts"}, nil),
			b:      side("iB", "open", t1, []string{"web/login.ts", "web/middleware/**"}, nil),
			action: ReplanLandFirst, first: "iB", then: "iA", paths: []string{"web/login.ts"},
		},
		{
			name:   "only globs collide: split the broadest",
			a:      side("iA", "open", t1, []string{"web/**"}, nil),
			b:      side("iB", "open", t2, []string{"web/components/**"}, nil),
			action: ReplanSplit, first: "iA", then: "iB", paths: []string{"web/**", "web/components/**"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, ok := SuggestReplan(tt.a, tt.b)
			if !ok {
				t.Fatal("expected an overlap")
			}
			if r.Action != tt.action || r.First != tt.first || r.Then != tt.then || !slices.Equal(r.Paths, tt.paths) || !r.Suggested || r.Summary == "" {
				t.Fatalf("got %+v, want %s %s→%s %v", r, tt.action, tt.first, tt.then, tt.paths)
			}
			if wantCmd := tt.action == ReplanStack; (r.Command != "") != wantCmd ||
				(wantCmd && (r.Command != RestackCommand(tt.then, tt.first) || !strings.Contains(r.Summary, r.Command))) {
				t.Fatalf("command = %q (summary %q); stack must name intent restack, others nothing", r.Command, r.Summary)
			}
			// Argument order never changes the answer.
			r2, _ := SuggestReplan(tt.b, tt.a)
			if r2.Action != r.Action || r2.First != r.First || r2.Then != r.Then {
				t.Fatalf("swapped: got %+v, want %+v", r2, r)
			}
		})
	}
	if _, ok := SuggestReplan(side("iA", "open", t1, []string{"docs/**"}, nil), side("iB", "open", t2, []string{"web/**"}, nil)); ok {
		t.Fatal("disjoint intents must not overlap")
	}
}
