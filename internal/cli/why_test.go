package cli

import (
	"errors"
	"net/http"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/notes"
)

// TestCheckNote: a git note is believed only when every field why shows
// from it is what the Worker recorded. Anyone who can push notes can write
// one that copies the public record and changes the rest.
func TestCheckNote(t *testing.T) {
	alice := "alice"
	record := func() *api.WhyResult {
		return &api.WhyResult{
			Intent:   api.Intent{ID: "i1", Goal: "limit login attempts", Reason: "brute force"},
			Decision: api.Decision{ID: "d1", Action: "human_approve", DecidedBy: &alice},
			Approvals: []api.Approval{
				{By: "alice", Verdict: "approve", Reason: "checked the lockout path"},
			},
		}
	}
	good := func() notes.Note {
		return notes.Note{
			IntentID: "i1", Goal: "limit login attempts", Reason: "brute force", Action: "human_approve",
			DecisionID: "d1", DecidedBy: "alice",
			Approvals: []notes.Approval{{By: "alice", Verdict: "approve", Reason: "checked the lockout path"}},
		}
	}
	cases := []struct {
		name     string
		note     func(*notes.Note)
		record   func(*api.WhyResult)
		werr     error
		verified string
		source   string
	}{
		{"matches", nil, nil, nil, "worker", "note"},
		{"older note without approvals gains them", func(n *notes.Note) { n.Approvals = nil }, nil, nil, "worker", "note"},
		{"policy decided both sides", func(n *notes.Note) { n.DecidedBy, n.Approvals = "policy", nil },
			func(w *api.WhyResult) { w.Decision.DecidedBy, w.Approvals = nil, nil }, nil, "worker", "note"},
		{"other decision id", func(n *notes.Note) { n.DecisionID = "d2" }, nil, nil, "mismatch", "worker"},
		{"other goal", func(n *notes.Note) { n.Goal = "reviewed by security" }, nil, nil, "mismatch", "worker"},
		{"other decider", func(n *notes.Note) { n.DecidedBy = "mallory" }, nil, nil, "mismatch", "worker"},
		{"other approval reason", func(n *notes.Note) { n.Approvals[0].Reason = "LGTM: rubber stamp" }, nil, nil, "mismatch", "worker"},
		{"other approver", func(n *notes.Note) { n.Approvals[0].By = "mallory" }, nil, nil, "mismatch", "worker"},
		{"other verdict", func(n *notes.Note) { n.Approvals[0].Verdict = "reject" }, nil, nil, "mismatch", "worker"},
		{"extra approval", func(n *notes.Note) {
			n.Approvals = append(n.Approvals, notes.Approval{By: "bob", Verdict: "approve", Reason: "also fine"})
		}, nil, nil, "mismatch", "worker"},
		{"worker has no decision", nil, nil, &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "none"}, "mismatch", "note"},
		{"worker unreachable", nil, nil, errors.New("dial tcp: refused"), "unchecked", "note"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := good()
			if tc.note != nil {
				tc.note(&n)
			}
			wr := record()
			if tc.record != nil {
				tc.record(wr)
			}
			res := whyResult{Source: "note", Note: &n}
			checkNote(&res, wr, tc.werr)
			if res.Verified != tc.verified || res.Source != tc.source {
				t.Fatalf("verified %q source %q, want %q %q", res.Verified, res.Source, tc.verified, tc.source)
			}
			if tc.verified != "worker" && res.Warning == "" {
				t.Error("no warning on an unverified note")
			}
			if tc.verified == "worker" && len(res.Note.Approvals) == 0 && len(wr.Approvals) > 0 {
				t.Error("a verified note without approvals should gain the Worker's")
			}
		})
	}
}
