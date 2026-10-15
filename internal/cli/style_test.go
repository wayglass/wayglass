package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/notes"
)

func sampleWhy() whyResult {
	return whyResult{
		File: "src/auth/login.ts", Line: 1, Commit: "9111fceadecb0000", Source: "note",
		Note: &notes.Note{
			IntentID: "if95aa11941", Goal: "feat: lock accounts after five failed sign-ins",
			Reason: "Brute-force attempts on login; a lockout limits them",
			Tags:   classify.Tags{Areas: []string{"auth", "docs"}, Risk: "low", Jurisdiction: "us"},
			Rule:   "rules[2]", Action: "human_approve", DecidedBy: "ana",
			DecidedAt: time.Date(2026, 10, 14, 7, 8, 0, 0, time.UTC),
			Approvals: []notes.Approval{
				{By: "ana", Verdict: "approve", Reason: "checked the lockout limit"},
				{By: "bo", Verdict: "reject", Reason: "error path untested"},
			},
		},
	}
}

func TestRenderWhyAlignsLabels(t *testing.T) {
	loc := time.Local
	time.Local = time.FixedZone("PDT", -7*3600)
	t.Cleanup(func() { time.Local = loc })

	var b bytes.Buffer
	if err := renderWhy(&b, styles{}, sampleWhy()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	want := []string{
		"src/auth/login.ts:1 was last changed in 9111fceadecb",
		"Intent:   if95aa11941  feat: lock accounts after five failed sign-ins",
		"Why:      Brute-force attempts on login; a lockout limits them",
		"Tags:     area auth, docs; data class none; risk low; jurisdiction us",
		"Decision: human_approve by rule rules[2], decided by ana at 2026-10-14 00:08 PDT",
		"Approved: ana: checked the lockout limit",
		"Rejected: bo: error path untested",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), strings.Join(want, "\n"))
	}
	col := len("Decision: ")
	for _, l := range lines[1:] {
		if l[col-1] != ' ' || l[col] == ' ' {
			t.Errorf("value not in column %d: %q", col, l)
		}
	}
}

func TestFieldsContinuationAligns(t *testing.T) {
	var b bytes.Buffer
	fields(&b, styles{}, field{"Jurisdiction", "us"}, field{"Paths", "a ~ b\nc ~ d"}, field{"", "more"})
	want := "Jurisdiction: us\nPaths:        a ~ b\n              c ~ d\n              more\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}

// Styles carry colour, but a non-terminal writer must get none: pipes,
// files and agents read plain text.
func TestHumanOutputHasNoANSIOffTerminal(t *testing.T) {
	var b bytes.Buffer
	st := cliStyles(Output{Out: &b})
	if !strings.Contains(st.action("block"), "\x1b[") {
		t.Fatal("styles render no colour at all; the test would prove nothing")
	}
	if err := renderWhy(&b, st, sampleWhy()); err != nil {
		t.Fatal(err)
	}
	rule := "rules[0]"
	wr := sampleWhy()
	wr.Note, wr.Source = nil, "worker"
	wr.Worker = &api.WhyResult{
		Intent:   api.Intent{ID: "i1", Goal: "g", Reason: "r"},
		Decision: api.Decision{ID: "d1", Action: "quarantine", Status: "quarantined", MatchedRule: &rule},
	}
	if err := renderWhy(&b, st, wr); err != nil {
		t.Fatal(err)
	}
	r := proposeResult{IntentID: "i1", Commit: "abcdef1234567", Next: "next",
		Decision: &api.Decision{ID: "d1", Action: "block", Status: "blocked"}}
	if err := renderPropose(&b, st, r); err != nil {
		t.Fatal(err)
	}
	if err := renderLand(&b, st, landResult{IntentID: "i1", Branch: "main", Commit: "abc"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "\x1b") {
		t.Fatalf("ANSI escapes in non-TTY output: %q", b.String())
	}
}
