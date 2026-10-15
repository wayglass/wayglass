package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/classify"
)

func loadExample(t *testing.T) *Policy {
	t.Helper()
	p, err := LoadFile("../../examples/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func many(n int) []string { return slices.Repeat([]string{"x"}, n) }

func file(path string, added ...string) classify.FileChange {
	return classify.FileChange{Path: path, Added: added}
}

// TestExamplePolicy walks the golden path decisions against examples/policy.yaml.
func TestExamplePolicy(t *testing.T) {
	p := loadExample(t)
	token := "art_v1_" + strings.Repeat("a1", 20)
	tests := []struct {
		name      string
		change    classify.Change
		action    Action
		rule      string
		approvers int
	}{
		{
			name:   "docs change auto-merges",
			change: classify.Change{Jurisdiction: "us", Files: []classify.FileChange{file("docs/usage.md", "new line")}},
			action: AutoMerge, rule: "rules[4]",
		},
		{
			name:   "large docs change is not low risk and falls to default",
			change: classify.Change{Files: []classify.FileChange{{Path: "docs/big.md", Added: many(401)}}},
			action: HumanApprove, rule: "default",
		},
		{
			name: "pii in the eu namespace is quarantined",
			change: classify.Change{Jurisdiction: "eu", Files: []classify.FileChange{
				file("web/profile.ts", "save({ date_of_birth: dob })"),
			}},
			action: Quarantine, rule: "rules[3]",
		},
		{
			name: "pii in the us namespace is only agent reviewed",
			change: classify.Change{Jurisdiction: "us", Files: []classify.FileChange{
				file("web/profile.ts", "save({ date_of_birth: dob })"),
			}},
			action: AgentReview, rule: "rules[5]",
		},
		{
			name:   "auth change routes to a human",
			change: classify.Change{Jurisdiction: "us", Files: []classify.FileChange{file("web/login.ts", "if (ok) {}")}},
			action: HumanApprove, rule: "rules[2]",
		},
		{
			name: "auth rule beats eu pii quarantine because it comes first",
			change: classify.Change{Jurisdiction: "eu", Files: []classify.FileChange{
				file("internal/auth/user.go", "u.SSN = ssn"),
			}},
			action: HumanApprove, rule: "rules[2]",
		},
		{
			name:   "infra needs two approvers",
			change: classify.Change{Files: []classify.FileChange{file(".github/workflows/ci.yml", "on: push")}},
			action: MultiApprove, rule: "rules[1]", approvers: 2,
		},
		{
			name: "secrets block even inside docs",
			change: classify.Change{Files: []classify.FileChange{
				file("docs/setup.md", "export TOKEN="+token),
			}},
			action: Block, rule: "rules[0]",
		},
		{
			name:   "api key mention blocks",
			change: classify.Change{Files: []classify.FileChange{file("worker/src/env.ts", "const apiKey = env.X")}},
			action: Block, rule: "rules[0]",
		},
		{
			name:   "ordinary low risk code gets agent review",
			change: classify.Change{Files: []classify.FileChange{file("internal/intent/intent.go", "package intent")}},
			action: AgentReview, rule: "rules[5]",
		},
		{
			name:   "high risk code falls to default",
			change: classify.Change{Files: []classify.FileChange{{Path: "internal/x.go", Added: many(500)}}},
			action: HumanApprove, rule: "default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.Evaluate(tt.change)
			if d.Action != tt.action {
				t.Errorf("action = %s, want %s (tags %+v)", d.Action, tt.action, d.Tags)
			}
			if got := d.Rule.ID(); got != tt.rule {
				t.Errorf("rule = %s, want %s", got, tt.rule)
			}
			if d.Approvers != tt.approvers {
				t.Errorf("approvers = %d, want %d", d.Approvers, tt.approvers)
			}
			if d.Reason == "" {
				t.Error("decision has no reason")
			}
			if d.Default != (tt.rule == "default") {
				t.Errorf("default = %v", d.Default)
			}
		})
	}
}

func TestWhenMatching(t *testing.T) {
	tags := classify.Tags{
		Areas:        []string{"auth", "web"},
		AreaSets:     [][]string{{"auth", "web"}, {"web"}},
		DataClasses:  []string{"pii"},
		Risk:         classify.RiskLow,
		Jurisdiction: "eu",
	}
	tests := []struct {
		name string
		when When
		want bool
	}{
		{"single area hit", When{Area: Values{"web"}}, true},
		{"single area miss", When{Area: Values{"docs"}}, false},
		{"any of listed areas", When{Area: Values{"docs", "auth"}}, true},
		{"and: area and data class both hit", When{Area: Values{"web"}, DataClass: Values{"pii"}}, true},
		{"and: one field misses", When{Area: Values{"web"}, DataClass: Values{"secrets"}}, false},
		{"risk hit", When{Risk: Values{"low"}}, true},
		{"risk miss", When{Risk: Values{"high"}}, false},
		{"jurisdiction is case-insensitive", When{Jurisdiction: Values{"EU"}}, true},
		{"jurisdiction miss", When{Jurisdiction: Values{"us"}}, false},
		{"all four fields", When{Area: Values{"auth"}, DataClass: Values{"pii"}, Risk: Values{"low"}, Jurisdiction: Values{"eu"}}, true},
		{"only_area: every file in web", When{OnlyArea: Values{"web"}}, true},
		{"only_area: one file outside auth", When{OnlyArea: Values{"auth"}}, false},
		{"only_area and risk", When{OnlyArea: Values{"web", "docs"}, Risk: Values{"low"}}, true},
		{"only_area miss", When{OnlyArea: Values{"docs"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.when.Matches(tags); got != tt.want {
				t.Fatalf("Matches = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShadowRules(t *testing.T) {
	src := `
version: 1
classify:
  area:
    docs: ["docs/**"]
    web: ["web/**"]
    auth: ["**/auth/**"]
    policy: [".wayglass/**"]
  data_class:
    pii: { content: ["(?i)date_of_birth"] }
rules:
  - { name: strict-docs, when: { area: docs }, action: human_approve, reason: "Trying stricter docs review.", mode: shadow }
  - { name: block-web, when: { area: web }, action: block, reason: "Trial web freeze.", shadow: true }
  - { name: pii-quarantine, when: { data_class: pii }, action: quarantine, reason: "Trial PII quarantine.", mode: shadow }
  - { name: docs, when: { area: docs }, action: auto_merge, reason: "Routine docs change.", mode: enforce }
  - { name: same-as-auth, when: { area: auth }, action: human_approve, reason: "Agrees with auth.", mode: shadow }
  - { name: auth, when: { area: auth }, action: human_approve, reason: "Auth is a boundary." }
  - { name: late-shadow, when: { area: docs }, action: block, reason: "After the deciding rule.", mode: shadow }
  - { name: policy-lax, when: { area: policy }, action: auto_merge, reason: "Would wave policy edits through.", mode: shadow }
default: { action: agent_review, reason: "Fallback." }
`
	p, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	type sm struct {
		rule     string
		action   Action
		preempts bool
	}
	tests := []struct {
		name        string
		files       []classify.FileChange
		action      Action
		rule        string
		shadow      []sm
		wouldDiffer bool
	}{
		{
			name:   "shadow before the deciding rule would differ; shadow after is logged but does not preempt",
			files:  []classify.FileChange{file("docs/a.md")},
			action: AutoMerge, rule: "docs",
			shadow:      []sm{{"strict-docs", HumanApprove, true}, {"late-shadow", Block, false}},
			wouldDiffer: true,
		},
		{
			name:   "legacy shadow: true before the default would differ",
			files:  []classify.FileChange{file("web/x.html")},
			action: AgentReview, rule: "default",
			shadow:      []sm{{"block-web", Block, true}},
			wouldDiffer: true,
		},
		{
			name:   "only the first preempting shadow rule decides would_differ",
			files:  []classify.FileChange{file("web/x.html", "date_of_birth")},
			action: AgentReview, rule: "default",
			shadow:      []sm{{"block-web", Block, true}, {"pii-quarantine", Quarantine, true}},
			wouldDiffer: true,
		},
		{
			name:   "shadow rule that agrees with the enforced rule does not differ",
			files:  []classify.FileChange{file("internal/auth/a.go")},
			action: HumanApprove, rule: "auth",
			shadow: []sm{{"same-as-auth", HumanApprove, true}},
		},
		{
			name:   "no shadow match",
			files:  []classify.FileChange{file("src/x.go")},
			action: AgentReview, rule: "default",
		},
		{
			name:   "a built-in guard floors the hypothetical too: lax shadow on a policy edit does not differ",
			files:  []classify.FileChange{file(".wayglass/policy.yaml")},
			action: HumanApprove, rule: BuiltinProtectPolicy,
			shadow: []sm{{"policy-lax", AutoMerge, true}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.Evaluate(classify.Change{Files: tt.files})
			if d.Action != tt.action || d.Rule.ID() != tt.rule {
				t.Fatalf("decision = %s by %s, want %s by %s", d.Action, d.Rule.ID(), tt.action, tt.rule)
			}
			if d.ShadowMatches == nil {
				t.Fatal("shadow_matches must be an empty list, not nil, for stable JSON")
			}
			got := make([]sm, 0, len(d.ShadowMatches))
			for _, s := range d.ShadowMatches {
				if s.Reason == "" {
					t.Errorf("shadow match %s has no reason", s.Rule)
				}
				got = append(got, sm{s.Rule, s.Action, s.Preempts})
			}
			if want := append([]sm{}, tt.shadow...); !slices.Equal(got, want) {
				t.Fatalf("shadow_matches = %+v, want %+v", got, want)
			}
			if d.WouldDiffer != tt.wouldDiffer {
				t.Fatalf("would_differ = %v, want %v", d.WouldDiffer, tt.wouldDiffer)
			}
		})
	}
}

// Enforcing rules must decide exactly as they would with every shadow rule
// deleted: shadow mode can never change an outcome.
func TestShadowRulesNeverDecide(t *testing.T) {
	base := `
version: 1
classify:
  area: { docs: ["docs/**"], web: ["web/**"] }
rules:
%s  - { when: { area: docs }, action: auto_merge, reason: "Docs." }
default: { action: human_approve, reason: "Fallback." }
`
	shadows := "  - { when: { area: docs }, action: block, reason: \"s1\", mode: shadow }\n" +
		"  - { when: { area: web }, action: auto_merge, reason: \"s2\", mode: shadow }\n"
	with, err := Parse(fmt.Appendf(nil, base, shadows))
	if err != nil {
		t.Fatal(err)
	}
	without, err := Parse(fmt.Appendf(nil, base, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"docs/a.md", "web/a.html", "x.go", ".wayglass/policy.yaml"} {
		c := classify.Change{Files: []classify.FileChange{file(path)}}
		a, b := with.Evaluate(c), without.Evaluate(c)
		if a.Action != b.Action || a.Reason != b.Reason || a.Approvers != b.Approvers {
			t.Errorf("%s: with shadows %s (%s), without %s (%s)", path, a.Action, a.Rule.ID(), b.Action, b.Rule.ID())
		}
	}
}

func TestParseMode(t *testing.T) {
	const head = "version: 1\nclassify:\n  area: { docs: [\"docs/**\"] }\nrules:\n  - { when: { area: docs }, action: block, reason: r"
	tests := []struct {
		name    string
		rule    string
		want    Mode
		wantErr string
	}{
		{"default is enforce", " }", Enforce, ""},
		{"explicit enforce", ", mode: enforce }", Enforce, ""},
		{"explicit shadow", ", mode: shadow }", Shadow, ""},
		{"legacy shadow: true", ", shadow: true }", Shadow, ""},
		{"legacy shadow: false", ", shadow: false }", Enforce, ""},
		{"shadow with legacy flag agrees", ", mode: shadow, shadow: true }", Shadow, ""},
		{"enforce contradicts legacy flag", ", mode: enforce, shadow: true }", "", "contradicts"},
		{"unknown mode", ", mode: dry-run }", "", `unknown mode "dry-run"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Parse([]byte(head + tt.rule + "\n"))
			if tt.wantErr != "" {
				if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := p.File.Rules[0].Mode; got != tt.want {
				t.Fatalf("mode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseValuesScalarOrList(t *testing.T) {
	p, err := Parse([]byte(`
version: 1
classify:
  area: { docs: ["docs/**"], web: ["web/**"] }
rules:
  - { when: { area: [docs, web], jurisdiction: EU }, action: auto_merge, reason: "r" }
`))
	if err != nil {
		t.Fatal(err)
	}
	w := p.File.Rules[0].When
	if !slices.Equal(w.Area, Values{"docs", "web"}) || !slices.Equal(w.Jurisdiction, Values{"eu"}) {
		t.Fatalf("when = %+v", w)
	}
	if p.File.Default.Action != HumanApprove {
		t.Fatalf("missing default should fall back to human_approve, got %s", p.File.Default.Action)
	}
}

func TestParseRejects(t *testing.T) {
	const head = "version: 1\nclassify:\n  area: { docs: [\"docs/**\"] }\n  data_class: { pii: { content: [\"ssn\"] } }\n"
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"empty", "", "empty file"},
		{"wrong version", "version: 2\n", "version is 2"},
		{"unknown key", head + "rules:\n  - { whne: { area: docs }, action: block, reason: r }\n", "whne"},
		{"empty when", head + "rules:\n  - { when: {}, action: block, reason: r }\n", "when is empty"},
		{"unknown action", head + "rules:\n  - { when: { area: docs }, action: merge, reason: r }\n", "unknown action"},
		{"missing reason", head + "rules:\n  - { when: { area: docs }, action: block }\n", "reason is required"},
		{"undefined area", head + "rules:\n  - { when: { area: doc }, action: block, reason: r }\n", `area "doc"`},
		{"undefined data class", head + "rules:\n  - { when: { data_class: phi }, action: block, reason: r }\n", `data_class "phi"`},
		{"unknown risk", head + "rules:\n  - { when: { risk: extreme }, action: block, reason: r }\n", `unknown risk "extreme"`},
		{"approvers on wrong action", head + "rules:\n  - { when: { area: docs }, action: block, approvers: 2, reason: r }\n", "approvers only applies"},
		{"too few approvers", head + "rules:\n  - { when: { area: docs }, action: multi_approve, approvers: 1, reason: r }\n", "approvers >= 2"},
		{"duplicate name", head + "rules:\n  - { name: a, when: { area: docs }, action: block, reason: r }\n  - { name: a, when: { area: docs }, action: block, reason: r }\n", "duplicate rule name"},
		{"bad default action", head + "default: { action: nope, reason: r }\n", "default: unknown action"},
		{"default without reason", head + "default: { action: block }\n", "default: reason is required"},
		{"reviewer on auto_merge", head + "rules:\n  - { when: { area: docs }, action: auto_merge, reviewer: { different_family: true }, reason: r }\n", "nobody approves it"},
		{"reviewer on block default", head + "default: { action: block, reviewer: { different_family: true }, reason: r }\n", "default: reviewer does not apply"},
		{"empty reviewer", head + "rules:\n  - { when: { area: docs }, action: agent_review, reviewer: {}, reason: r }\n", "reviewer is empty"},
		{"unknown reviewer key", head + "rules:\n  - { when: { area: docs }, action: agent_review, reviewer: { other_family: true }, reason: r }\n", "other_family"},
		{"duplicate reviewer key", head + "rules:\n  - { when: { area: docs }, action: agent_review, reviewer: { kind: ai, kind: agent }, reason: r }\n", "already defined"},
		{"unknown reviewer kind", head + "rules:\n  - { when: { area: docs }, action: agent_review, reviewer: llm, reason: r }\n", `unknown reviewer kind "llm"`},
		{"reviewer kind on human_approve", head + "rules:\n  - { when: { area: docs }, action: human_approve, reviewer: ai, reason: r }\n", "applies to agent_review only"},
		{"reviewer as a list", head + "rules:\n  - { when: { area: docs }, action: agent_review, reviewer: [ai], reason: r }\n", "a word (any, ai, agent) or a mapping"},
		{"ai on auto_merge", head + "rules:\n  - { when: { area: docs }, action: auto_merge, ai: { advise: true }, reason: r }\n", "ai does not apply to auto_merge"},
		{"ai on block default", head + "default: { action: block, ai: { min_confidence: 0.9 }, reason: r }\n", "default: ai does not apply"},
		{"empty ai", head + "rules:\n  - { when: { area: docs }, action: agent_review, ai: {}, reason: r }\n", "ai is empty"},
		{"unknown ai key", head + "rules:\n  - { when: { area: docs }, action: agent_review, ai: { confidence: 0.9 }, reason: r }\n", "confidence"},
		{"min_confidence over 1", head + "rules:\n  - { when: { area: docs }, action: agent_review, ai: { min_confidence: 1.5 }, reason: r }\n", "between 0 and 1"},
		{"min_confidence on human_approve", head + "rules:\n  - { when: { area: docs }, action: human_approve, ai: { min_confidence: 0.9 }, reason: r }\n", "min_confidence applies to agent_review only"},
		{"advise on agent_review", head + "rules:\n  - { when: { area: docs }, action: agent_review, ai: { advise: true }, reason: r }\n", "advise applies to changes routed to a person"},
		{"ai.model that is not an id", head + "rules:\n  - { when: { area: docs }, action: agent_review, ai: { model: \"llama scout\" }, reason: r }\n", "not a model id"},
		{"bad classify regex", "version: 1\nclassify:\n  data_class: { x: { content: [\"(\"] } }\n", "bad regex"},
		// Shapes an agent could write to slip a rule past the parser.
		{"version as a string", "version: \"1\"\n", "cannot unmarshal"},
		{"missing version", "classify:\n  area: { docs: [\"docs/**\"] }\nrules: []\n", "version is 0"},
		{"duplicate top-level key", head + "default: { action: block, reason: r }\ndefault: { action: auto_merge, reason: r }\n", "already defined"},
		{"duplicate key inside a rule", head + "rules:\n  - { when: { area: docs }, action: block, action: auto_merge, reason: r }\n", "already defined"},
		{"second YAML document", head + "rules: []\n---\nrules:\n  - { when: { area: docs }, action: auto_merge, reason: r }\n", "more than one YAML document"},
		{"rules as a map", head + "rules:\n  docs: { when: { area: docs }, action: block, reason: r }\n", "cannot unmarshal"},
		{"capitalised action", head + "rules:\n  - { when: { area: docs }, action: Block, reason: r }\n", `unknown action "Block"`},
		{"capitalised mode", head + "rules:\n  - { when: { area: docs }, action: block, reason: r, mode: Shadow }\n", "unknown mode"},
		{"null area is no condition", head + "rules:\n  - { when: { area: ~ }, action: block, reason: r }\n", "when is empty"},
		{"when as a list", head + "rules:\n  - { when: [docs], action: block, reason: r }\n", "cannot unmarshal"},
		{"whitespace-only reason", head + "rules:\n  - { when: { area: docs }, action: block, reason: \"  \" }\n", "reason is required"},
		{"negative approvers", head + "rules:\n  - { when: { area: docs }, action: multi_approve, approvers: -2, reason: r }\n", "approvers >= 2"},
		{"unknown top-level key", head + "overrides: []\n", "overrides"},
		{"unknown classify key", "version: 1\nclassify:\n  areas: { docs: [\"docs/**\"] }\n", "areas"},
		{"unknown risk threshold", "version: 1\nclassify:\n  risk: { high: { lines_over: 1 } }\n", "lines_over"},
		{"unknown when key", head + "rules:\n  - { when: { path: docs }, action: block, reason: r }\n", "path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error does not wrap ErrInvalid: %v", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

// Both spellings of reviewer parse, the kind and the ai block ride on the
// decision, and a built-in floor drops them with the rest of the routing.
func TestParseReviewerAndAI(t *testing.T) {
	const src = `
version: 1
classify:
  area: { tests: ["**/*_test.go"], docs: ["docs/**"], web: ["web/**"] }
rules:
  - { when: { only_area: tests }, action: agent_review, reviewer: ai, ai: { min_confidence: 0.85, model: "anthropic/claude-sonnet-5.5" }, reason: r }
  - { when: { only_area: docs }, action: agent_review, reviewer: { kind: agent, different_family: true }, reason: r }
  - { when: { area: web }, action: multi_approve, ai: { advise: true }, reason: r }
  - { when: { area: web }, action: agent_review, reviewer: any, reason: r }
default: { action: human_approve, ai: { advise: true, model: "@cf/meta/llama-4-scout-17b-16e-instruct" }, reason: r }
`
	p, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	rules := p.File.Rules
	if got := rules[0].Reviewer; got == nil || got.Kind != ReviewerAI || got.DifferentFamily {
		t.Fatalf("rules[0].reviewer = %+v", got)
	}
	if got := rules[0].AI; got == nil || got.MinConfidence != 0.85 || got.Model != "anthropic/claude-sonnet-5.5" || got.Advise {
		t.Fatalf("rules[0].ai = %+v", got)
	}
	if got := rules[1].Reviewer; got == nil || got.Kind != ReviewerAgent || !got.DifferentFamily {
		t.Fatalf("rules[1].reviewer = %+v", got)
	}
	if got := rules[3].Reviewer; got == nil || got.Kind != ReviewerAny {
		t.Fatalf("rules[3].reviewer = %+v", got)
	}
	if got := p.File.Default.AI; got == nil || !got.Advise || got.Model == "" {
		t.Fatalf("default.ai = %+v", got)
	}

	file := func(path string) classify.Change { return classify.Change{Files: []classify.FileChange{{Path: path}}} }
	if d := p.Evaluate(file("a_test.go")); d.AI == nil || d.AI.MinConfidence != 0.85 || d.Reviewer.Kind != ReviewerAI {
		t.Fatalf("tests decision = %+v", d)
	}
	if d := p.Evaluate(file("web/a.ts")); d.Action != MultiApprove || d.AI == nil || !d.AI.Advise || d.Reviewer != nil {
		t.Fatalf("web decision = %+v", d)
	}
	if d := p.Evaluate(file("other.go")); !d.Default || d.AI == nil || !d.AI.Advise {
		t.Fatalf("default decision = %+v", d)
	}
	if d := p.Evaluate(file(".wayglass/a_test.go")); d.Rule.ID() != BuiltinProtectPolicy || d.AI != nil || d.Reviewer != nil {
		t.Fatalf("guarded decision keeps the rule's AI settings: %+v", d)
	}
}

func TestMultiApproveDefaultsToTwo(t *testing.T) {
	p, err := Parse([]byte("version: 1\nclassify:\n  area: { infra: [\".github/**\"] }\nrules:\n  - { when: { area: infra }, action: multi_approve, reason: r }\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := p.Evaluate(classify.Change{Files: []classify.FileChange{{Path: ".github/x.yml"}}})
	if d.Action != MultiApprove || d.Approvers != DefaultMultiApprovers {
		t.Fatalf("decision = %s with %d approvers", d.Action, d.Approvers)
	}
}

func TestActionProperties(t *testing.T) {
	tests := []struct {
		a                 Action
		lands, needsHuman bool
	}{
		{AutoMerge, true, false},
		{AgentReview, true, false},
		{HumanApprove, false, true},
		{MultiApprove, false, true},
		{Quarantine, false, true},
		{Block, false, false},
	}
	for _, tt := range tests {
		if !tt.a.Valid() || tt.a.Lands() != tt.lands || tt.a.NeedsHuman() != tt.needsHuman {
			t.Errorf("%s: valid=%v lands=%v needsHuman=%v", tt.a, tt.a.Valid(), tt.a.Lands(), tt.a.NeedsHuman())
		}
	}
	if Action("merge").Valid() {
		t.Error("unknown action reported valid")
	}
}

func TestOnlyAreaRules(t *testing.T) {
	p, err := Parse([]byte(`
version: 1
classify:
  area:
    docs: ["docs/**", "**/*.md"]
    tests: ["**/*_test.go"]
    app: ["internal/**"]
rules:
  - { name: docs-only, when: { only_area: docs }, action: auto_merge, reason: "Docs only." }
  - { name: tests-only, when: { only_area: tests }, action: agent_review, reason: "Tests only." }
default: { action: human_approve, reason: "A person decides." }
`))
	if err != nil {
		t.Fatal(err)
	}
	change := func(paths ...string) classify.Change {
		c := classify.Change{Jurisdiction: "us"}
		for _, p := range paths {
			c.Files = append(c.Files, classify.FileChange{Path: p, Added: []string{"x"}})
		}
		return c
	}
	tests := []struct {
		name  string
		paths []string
		want  Action
		rule  string
	}{
		{"docs only auto-merges", []string{"docs/a.md", "README.md"}, AutoMerge, "docs-only"},
		{"docs plus code goes to a person", []string{"docs/a.md", "internal/shop/cart.go"}, HumanApprove, "default"},
		{"tests only gets an agent review", []string{"internal/shop/cart_test.go"}, AgentReview, "tests-only"},
		{"tests plus code goes to a person", []string{"internal/shop/cart_test.go", "internal/shop/cart.go"}, HumanApprove, "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := p.Evaluate(change(tt.paths...))
			if d.Action != tt.want || d.Rule.ID() != tt.rule {
				t.Fatalf("got %s by %s, want %s by %s", d.Action, d.Rule.ID(), tt.want, tt.rule)
			}
		})
	}
	if _, err := Parse([]byte(`
version: 1
classify: { area: { docs: ["docs/**"] } }
rules: [{ when: { only_area: web }, action: auto_merge, reason: "r" }]
`)); err == nil || !strings.Contains(err.Error(), `only_area "web" is not defined`) {
		t.Fatalf("undefined only_area: err = %v", err)
	}
}

// The protect-policy guard must see every spelling of a path under
// .wayglass/, and a built-in floor must never lower a stricter decision.
func TestBuiltinGuards(t *testing.T) {
	const lax = `
version: 1
classify:
  area: { all: ["**"] }
  data_class: { secrets: { content: ["TOKEN="] } }
rules:
  - { when: { data_class: secrets }, action: block, reason: "Secrets never land." }
  - { when: { area: all }, action: agent_review, reviewer: { different_family: true }, reason: "An agent checks everything." }
default: { action: auto_merge, reason: "Everything merges." }
`
	tests := []struct {
		name     string
		req      WireRequest
		action   Action
		rule     string
		overrode string
	}{
		{name: "plain", req: WireRequest{Changes: []WireChange{{Path: "a.go"}}}, action: AgentReview, rule: "rules[1]"},
		{name: "policy file", req: WireRequest{Changes: []WireChange{{Path: ".wayglass/policy.yaml"}}}, action: HumanApprove, rule: BuiltinProtectPolicy, overrode: "rules[1]"},
		{name: "dot-slash prefix", req: WireRequest{Changes: []WireChange{{Path: "./.wayglass/policy.yaml"}}}, action: HumanApprove, rule: BuiltinProtectPolicy, overrode: "rules[1]"},
		{name: "backslash separator", req: WireRequest{Changes: []WireChange{{Path: `.wayglass\policy.yaml`}}}, action: HumanApprove, rule: BuiltinProtectPolicy, overrode: "rules[1]"},
		{name: "the directory itself (a symlink)", req: WireRequest{Changes: []WireChange{{Path: ".wayglass"}}}, action: HumanApprove, rule: BuiltinProtectPolicy, overrode: "rules[1]"},
		{name: "any file under it", req: WireRequest{Changes: []WireChange{{Path: ".wayglass/x/y.txt"}}}, action: HumanApprove, rule: BuiltinProtectPolicy, overrode: "rules[1]"},
		{name: "a sibling with the same prefix is not the config dir", req: WireRequest{Changes: []WireChange{{Path: ".wayglass2/x"}}}, action: AgentReview, rule: "rules[1]"},
		{name: "a nested dir of the same name is not the config dir", req: WireRequest{Changes: []WireChange{{Path: "sub/.wayglass/policy.yaml"}}}, action: AgentReview, rule: "rules[1]"},
		{name: "a block stays a block when the policy is touched", req: WireRequest{Changes: []WireChange{{Path: ".wayglass/policy.yaml", AddedLines: []string{"TOKEN=x"}}}}, action: Block, rule: "rules[0]"},
		{name: "a block stays a block when the diff is truncated", req: WireRequest{Changes: []WireChange{{Path: "a.go", AddedLines: []string{"TOKEN=x"}}}, Truncated: true}, action: Block, rule: "rules[0]"},
		{name: "a truncated lax change goes to a person", req: WireRequest{Changes: []WireChange{{Path: "a.go"}}, Truncated: true}, action: HumanApprove, rule: BuiltinTruncated, overrode: "rules[1]"},
		{name: "no policy and truncated is one human_approve", req: WireRequest{PolicyYAML: "   \n", Changes: []WireChange{{Path: "a.go"}}, Truncated: true}, action: HumanApprove, rule: BuiltinNoPolicy},
		// Binary files: the detectors read none of them, so only the default
		// allowlist (no binary: key in lax) lets one through.
		{name: "an allowlisted binary is routed by the rules", req: WireRequest{Changes: []WireChange{{Path: "web/logo.png", Binary: true, Bytes: 100}}}, action: AgentReview, rule: "rules[1]"},
		{name: "a binary outside the allowlist goes to a person", req: WireRequest{Changes: []WireChange{{Path: "tools/run.exe", Binary: true}}}, action: HumanApprove, rule: BuiltinUnscannedBinary, overrode: "rules[1]"},
		{name: "a NUL byte hides a text file from the detectors", req: WireRequest{Changes: []WireChange{{Path: "notes.txt", Binary: true, Bytes: 80}}}, action: HumanApprove, rule: BuiltinUnscannedBinary, overrode: "rules[1]"},
		{name: "a key store is not an asset", req: WireRequest{Changes: []WireChange{{Path: "certs/client.p12", Binary: true}}}, action: HumanApprove, rule: BuiltinUnscannedBinary, overrode: "rules[1]"},
		{name: "a file with no extension is not an asset", req: WireRequest{Changes: []WireChange{{Path: "bin/wayglass", Binary: true}}}, action: HumanApprove, rule: BuiltinUnscannedBinary, overrode: "rules[1]"},
		{name: "a block stays a block with an unscanned binary", req: WireRequest{Changes: []WireChange{{Path: "a.go", AddedLines: []string{"TOKEN=x"}}, {Path: "blob.bin", Binary: true}}}, action: Block, rule: "rules[0]"},
		{name: "a deleted binary is not unscanned", req: WireRequest{Changes: []WireChange{{Path: "old.bin"}}}, action: AgentReview, rule: "rules[1]"},
		{name: "no policy and an unscanned binary is one human_approve", req: WireRequest{PolicyYAML: "   \n", Changes: []WireChange{{Path: "blob.bin", Binary: true}}}, action: HumanApprove, rule: BuiltinNoPolicy},
		{name: "an invalid policy and an unscanned binary is one human_approve", req: WireRequest{PolicyYAML: "version: 2\n", Changes: []WireChange{{Path: "blob.bin", Binary: true}}}, action: HumanApprove, rule: BuiltinInvalidPolicy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.req
			if req.PolicyYAML == "" {
				req.PolicyYAML = lax
			}
			got := EvaluateWire(req)
			if got.Error != nil {
				t.Fatalf("error: %+v", got.Error)
			}
			if got.Action != tt.action || got.MatchedRule != tt.rule {
				t.Fatalf("decision = %s by %s, want %s by %s", got.Action, got.MatchedRule, tt.action, tt.rule)
			}
			var overrode string
			if got.Overrode != nil {
				overrode = got.Overrode.ID
			}
			if overrode != tt.overrode {
				t.Fatalf("overrode = %q, want %q", overrode, tt.overrode)
			}
			if wantReviewer := tt.rule == "rules[1]"; (got.Reviewer != nil) != wantReviewer {
				t.Fatalf("reviewer = %+v; a built-in guard replaces the rule's reviewer constraint", got.Reviewer)
			}
		})
	}
}

// binary.allow replaces the default list when set; an empty list allows
// nothing, and a bad glob fails the file closed like any other mistake.
func TestBinaryAllow(t *testing.T) {
	const head = "version: 1\nclassify:\n  area: { all: [\"**\"] }\nrules:\n  - { name: all, when: { area: all }, action: auto_merge, reason: \"Everything merges.\" }\n"
	tests := []struct {
		name   string
		src    string
		allow  []string
		png    Action // decision for assets/logo.png
		pngIn  Action // decision for web/assets/logo.png
		bin    Action // decision for tools/run.bin
		errHas string
	}{
		{name: "absent means the default list", src: head, allow: DefaultBinaryAllow, png: AutoMerge, pngIn: AutoMerge, bin: HumanApprove},
		{name: "an empty section means the default list", src: head + "binary: {}\n", allow: DefaultBinaryAllow, png: AutoMerge, pngIn: AutoMerge, bin: HumanApprove},
		{name: "allow: [] lets no binary through", src: head + "binary: { allow: [] }\n", allow: []string{}, png: HumanApprove, pngIn: HumanApprove, bin: HumanApprove},
		{name: "a list replaces the default", src: head + "binary: { allow: [\"web/assets/**/*.png\"] }\n", allow: []string{"web/assets/**/*.png"}, png: HumanApprove, pngIn: AutoMerge, bin: HumanApprove},
		{name: "a bad glob is refused", src: head + "binary: { allow: [\"[\"] }\n", errHas: "binary.allow: invalid glob"},
		{name: "an unknown key is refused", src: head + "binary: { deny: [\"*.bin\"] }\n", errHas: "deny"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Parse([]byte(tt.src))
			if tt.errHas != "" {
				if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tt.errHas) {
					t.Fatalf("err = %v, want %q", err, tt.errHas)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(p.File.Binary.Allow, tt.allow) {
				t.Fatalf("allow = %v, want %v", p.File.Binary.Allow, tt.allow)
			}
			for path, want := range map[string]Action{"assets/logo.png": tt.png, "web/assets/logo.png": tt.pngIn, "tools/run.bin": tt.bin} {
				d := p.Evaluate(classify.Change{Files: []classify.FileChange{{Path: path, Binary: true, Bytes: 10}}})
				if d.Action != want {
					t.Errorf("%s: %s by %s, want %s", path, d.Action, d.Rule.ID(), want)
				}
				if want == HumanApprove && (d.Rule.ID() != BuiltinUnscannedBinary || d.Overrode == nil || d.Overrode.ID() != "all" || !strings.Contains(d.Reason, path)) {
					t.Errorf("%s: rule %s overrode %v reason %q", path, d.Rule.ID(), d.Overrode, d.Reason)
				}
				if !slices.Equal(d.Tags.Unscanned, []string{path}) {
					t.Errorf("%s: unscanned = %v", path, d.Tags.Unscanned)
				}
			}
		})
	}
}

// The floor's reason names the files a person has to look at, and stops
// listing after three so a bulk asset drop does not flood the record.
func TestUnscannedReasonListsFiles(t *testing.T) {
	p, err := Parse([]byte("version: 1\nbinary: { allow: [] }\ndefault: { action: auto_merge, reason: \"Everything merges.\" }\n"))
	if err != nil {
		t.Fatal(err)
	}
	var files []classify.FileChange
	for _, n := range []string{"a.bin", "b.bin", "c.bin", "d.bin", "e.bin"} {
		files = append(files, classify.FileChange{Path: n, Binary: true})
	}
	d := p.Evaluate(classify.Change{Files: files[:1]})
	if d.Action != HumanApprove || !strings.HasPrefix(d.Reason, "a.bin is a binary file outside binary.allow") {
		t.Fatalf("one file: %s %q", d.Action, d.Reason)
	}
	d = p.Evaluate(classify.Change{Files: files})
	if !strings.Contains(d.Reason, "5 binary files") || !strings.Contains(d.Reason, "a.bin, b.bin, c.bin and 2 more") {
		t.Fatalf("five files: %q", d.Reason)
	}
	// A shadow rule that would have agent-reviewed does not "differ": the
	// floor would raise it too.
	p, err = Parse([]byte("version: 1\nclassify:\n  area: { all: [\"**\"] }\nbinary: { allow: [] }\nrules:\n  - { name: trial, when: { area: all }, action: agent_review, reason: r, mode: shadow }\ndefault: { action: auto_merge, reason: r }\n"))
	if err != nil {
		t.Fatal(err)
	}
	d = p.Evaluate(classify.Change{Files: files[:1]})
	if d.Rule.ID() != BuiltinUnscannedBinary || d.WouldDiffer || len(d.ShadowMatches) != 1 {
		t.Fatalf("shadow under the floor: %s would_differ=%v shadow=%v", d.Rule.ID(), d.WouldDiffer, d.ShadowMatches)
	}
}
