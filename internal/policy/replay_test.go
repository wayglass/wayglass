package policy

import (
	"slices"
	"testing"

	"github.com/wayglass/wayglass/internal/classify"
)

func TestHitsAndSummarize(t *testing.T) {
	p, err := Parse([]byte(`
version: 1
classify:
  area: { docs: ["docs/**"], web: ["web/**"] }
rules:
  - { name: trial, when: { area: web }, action: quarantine, reason: "Trial.", mode: shadow }
  - { name: docs, when: { area: docs }, action: auto_merge, reason: "Docs." }
  - { name: web, when: { area: web }, action: agent_review, reason: "Web." }
  - { name: any-docs, when: { area: docs }, action: block, reason: "Never reached." }
default: { action: agent_review, reason: "Fallback." }
`))
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string][]classify.FileChange{
		"docs":     {file("docs/a.md")},
		"web":      {file("web/a.ts")},
		"web+docs": {file("web/a.ts"), file("docs/a.md")},
		"other":    {file("x.go")},
		"policy":   {file(".wayglass/policy.yaml")},
	}
	hits := map[string][]string{
		"docs":     {"docs", "any-docs"},
		"web":      {"trial", "web"},
		"web+docs": {"trial", "docs", "web", "any-docs"},
		"other":    {},
		"policy":   {},
	}
	var ds []Decision
	for _, name := range []string{"docs", "web", "web+docs", "other", "policy"} {
		d := p.Evaluate(classify.Change{Files: changes[name]})
		ds = append(ds, d)
		got := []string{}
		for _, h := range p.Hits(d.Tags) {
			got = append(got, h.Rule)
		}
		if !slices.Equal(got, hits[name]) {
			t.Errorf("%s: hits = %v, want %v", name, got, hits[name])
		}
	}

	want := []RuleStats{
		{Rule: "trial", Index: 0, Mode: Shadow, Action: Quarantine, Matched: 2, Decided: 2, Differed: 2},
		{Rule: "docs", Index: 1, Mode: Enforce, Action: AutoMerge, Matched: 2, Decided: 2},
		{Rule: "web", Index: 2, Mode: Enforce, Action: AgentReview, Matched: 2, Decided: 1},
		{Rule: "any-docs", Index: 3, Mode: Enforce, Action: Block, Matched: 2},
		{Rule: "default", Index: -1, Action: AgentReview, Decided: 1},
		{Rule: BuiltinProtectPolicy, Index: BuiltinIndex, Action: HumanApprove, Decided: 1},
	}
	if got := p.Summarize(ds); !slices.Equal(got, want) {
		t.Fatalf("summary =\n%+v\nwant\n%+v", got, want)
	}
}
