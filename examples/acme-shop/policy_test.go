// Package acmeshop_test checks that the Acme Shop demo policy routes each
// task brief in tasks/ the way the brief says it will. The changes here are
// the scripted stand-ins from scripts/demo/examples.sh, reduced to the lines
// that matter for classification.
package acmeshop_test

import (
	"slices"
	"testing"

	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/policy"
)

func file(path string, added ...string) classify.FileChange {
	if len(added) == 0 {
		added = []string{"// a line"}
	}
	return classify.FileChange{Path: path, Added: added}
}

func TestAcmePolicyRoutesEachTask(t *testing.T) {
	pol, err := policy.LoadFile("app/.wayglass/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		task         string
		jurisdiction string
		files        []classify.FileChange
		action       policy.Action
		rule         string
		shadow       string // a shadow rule that must preempt, if any
	}{
		{"01-docs", "us", []classify.FileChange{
			file("docs/api.md", "| GET | `/api/products?category=kitchen` | Products in one category. |"),
		}, policy.AutoMerge, "docs-only", ""},
		{"02-auth", "us", []classify.FileChange{
			file("internal/auth/auth.go", "const maxFailures = 5"),
			file("internal/auth/auth_test.go", `if _, err := s.Login("ada@example.com", "wrong"); err == nil {`),
		}, policy.HumanApprove, "auth-boundary", ""},
		{"03-eu-pii", "eu", []classify.FileChange{
			file("internal/billing/customer.go", "\tVATNumber   string `json:\"vat_number,omitempty\"`", "\tDateOfBirth string `json:\"date_of_birth,omitempty\"`"),
		}, policy.Quarantine, "eu-personal-data", ""},
		{"03-eu-pii on a US repo", "us", []classify.FileChange{
			file("internal/billing/customer.go", "\tDateOfBirth string `json:\"date_of_birth,omitempty\"`"),
		}, policy.HumanApprove, "personal-data", ""},
		{"04-infra", "us", []classify.FileChange{
			file("infra/wrangler.jsonc", `      "max_instances": 5`),
			file("docs/architecture.md", "It scales to five instances."),
		}, policy.MultiApprove, "infra-two-people", ""},
		{"05-tests", "us", []classify.FileChange{
			file("internal/catalog/price_test.go", "func TestFormatPriceTable(t *testing.T) {"),
		}, policy.AgentReview, "tests-only", "trial-tests-auto-merge"},
		{"06-secret", "us", []classify.FileChange{
			file("internal/billing/paylane.go", `const paylaneKey = "pl_live_4f9c2a7e1b8d3f6a0c5e9b2d7a4f1c8e"`),
		}, policy.Block, "no-secrets", ""},
		{"07-search", "us", []classify.FileChange{
			file("internal/catalog/catalog.go", "func (c *Catalog) Search(q string) []Product {"),
			file("internal/catalog/search_test.go", "func TestSearch(t *testing.T) {"),
			file("internal/server/server.go", `	q := r.URL.Query().Get("q")`),
		}, policy.AgentReview, "routine-app", ""},
		{"08-category (stacked on 07)", "us", []classify.FileChange{
			file("internal/catalog/catalog.go", "func (c *Catalog) InCategory(cat string) []Product {"),
			file("internal/server/server.go", `	cat := r.URL.Query().Get("category")`),
			file("web/app.js", "const params = new URLSearchParams(location.search);"),
		}, policy.AgentReview, "routine-app", ""},
		// Guards the permissive rules: docs or tests bundled with code do not
		// ride along.
		{"docs plus billing code", "us", []classify.FileChange{
			file("docs/api.md"), file("internal/billing/invoice.go"),
		}, policy.HumanApprove, "billing-money", ""},
		{"docs plus a build file outside every area", "us", []classify.FileChange{
			file("docs/api.md"), file("Makefile"),
		}, policy.HumanApprove, "default", ""},
		// A policy file is in no area, so nothing permissive matches; the
		// built-in guard would raise it to a person anyway.
		{"policy edit", "us", []classify.FileChange{
			file(".wayglass/policy.yaml", "  - { when: { area: docs }, action: auto_merge, reason: x }"),
		}, policy.HumanApprove, "default", ""},
	}
	for _, tt := range tests {
		t.Run(tt.task, func(t *testing.T) {
			d := pol.Evaluate(classify.Change{Files: tt.files, Jurisdiction: tt.jurisdiction})
			if d.Action != tt.action || d.Rule.ID() != tt.rule {
				t.Fatalf("got %s by %s, want %s by %s (tags %+v)", d.Action, d.Rule.ID(), tt.action, tt.rule, d.Tags)
			}
			// Everything a reviewer agent approves needs one from another
			// model family than the author.
			if cross := d.Reviewer != nil && d.Reviewer.DifferentFamily; cross != (d.Action == policy.AgentReview) {
				t.Errorf("reviewer = %+v for %s, want different_family exactly on agent_review", d.Reviewer, d.Action)
			}
			first, ok := d.FirstPreempting()
			switch {
			case tt.shadow == "" && ok:
				t.Errorf("unexpected preempting shadow rule %s", first.Rule)
			case tt.shadow != "" && (!ok || first.Rule != tt.shadow || !d.WouldDiffer):
				t.Errorf("shadow = %+v, would_differ %v; want %s to preempt and differ", d.ShadowMatches, d.WouldDiffer, tt.shadow)
			}
		})
	}
}

func TestAcmePolicyAreas(t *testing.T) {
	pol, err := policy.LoadFile("app/.wayglass/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tags := pol.Tagger.Classify(classify.Change{Files: []classify.FileChange{
		file("internal/auth/auth_test.go"),
	}})
	if want := []string{"app", "auth", "tests"}; !slices.Equal(tags.Areas, want) {
		t.Fatalf("auth test file areas = %v, want %v", tags.Areas, want)
	}
}
