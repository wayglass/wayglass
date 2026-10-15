package cli

import (
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
)

// agentReviewIntent declares, pushes and proposes a change the example
// policy routes to agent_review (a us-project PII change), returning the
// intent id.
func agentReviewIntent(t *testing.T, e *flowEnv) string {
	t.Helper()
	e.do(e.repo, ExitOK, "init", "--project", "demo")
	id := get(e.do(e.repo, ExitOK, "intent", "declare", "--goal", "Profile page", "--reason", "Users asked", "--path", "web/profile.ts"), "intent", "id").(string)
	e.write(e.repo, "web/profile.ts", "save({ date_of_birth: dob })\n")
	e.do(e.repo, ExitOK, "push", "-m", "feat: profile")
	pr := e.do(e.repo, ExitOK, "propose")
	if get(pr, "decision", "action") != "agent_review" || get(pr, "decision", "status") != api.StatusPendingAgent {
		t.Fatalf("propose = %v", pr)
	}
	return id
}

func TestAIReviewApprovesAndLandsWithTheAIOnTheNote(t *testing.T) {
	e := newFlow(t)
	id := agentReviewIntent(t, e)
	e.worker.aiVerdict = "approve"
	r := e.do(e.repo, ExitOK, "ai-review", id)
	if get(r, "review", "verdict") != "approve" || get(r, "review", "source") != "model" || get(r, "review", "model") != "@cf/meta/llama-4-scout-17b-16e-instruct" {
		t.Fatalf("ai-review = %v", r)
	}
	if get(r, "decision", "status") != api.StatusApproved || get(r, "decision", "decided_by") != fakeAIReviewer {
		t.Fatalf("decision = %v", get(r, "decision"))
	}
	aps := get(r, "approvals").([]any)
	if len(aps) != 1 || get(aps[0].(map[string]any), "identity_source") != "ai" || get(aps[0].(map[string]any), "family") != "meta" {
		t.Fatalf("approvals = %v", aps)
	}
	if next, _ := get(r, "next").(string); !strings.Contains(next, "land") {
		t.Fatalf("next = %q", next)
	}
	// The why travels with the commit: the note names the AI as the decider.
	land := e.do(e.repo, ExitOK, "land")
	if get(land, "note_written") != true {
		t.Fatalf("land = %v", land)
	}
	why := e.do(e.repo, ExitOK, "why", "web/profile.ts:1")
	if get(why, "note", "decided_by") != fakeAIReviewer || get(why, "note", "identity_source") != "ai" {
		t.Fatalf("why = %v", why)
	}
}

func TestAIReviewEscalatesToAPerson(t *testing.T) {
	e := newFlow(t)
	id := agentReviewIntent(t, e)
	e.worker.aiVerdict = "escalate"
	r := e.do(e.repo, ExitOK, "ai-review", id)
	if get(r, "review", "verdict") != "escalate" || get(r, "decision", "status") != api.StatusPendingHuman || get(r, "decision", "action") != "human_approve" {
		t.Fatalf("ai-review = %v", r)
	}
	if c := get(r, "review", "concerns").([]any); len(c) != 1 {
		t.Fatalf("concerns = %v", c)
	}
	// What a person then reads as the decision's reason says why it reached them.
	if reason, _ := get(r, "decision", "reason").(string); !strings.HasPrefix(reason, "Escalated by AI reviewer: ") {
		t.Fatalf("decision reason = %q", reason)
	}
	if next, _ := get(r, "next").(string); !strings.Contains(next, "person") {
		t.Fatalf("next = %q", next)
	}
	// From here only a person decides; the AI cannot be asked again.
	again := e.do(e.repo, ExitNeedsHuman, "ai-review", id)
	if get(again, "error", "code") != api.CodeNeedsHuman {
		t.Fatalf("second ai-review = %v", again)
	}
	q := e.do(e.repo, ExitOK, "review")
	if items := get(q, "queue").([]any); len(items) != 1 || get(items[0].(map[string]any), "intent", "id") != id {
		t.Fatalf("review queue = %v", q)
	}
	if e.worker.aiReviews != 1 {
		t.Fatalf("model calls = %d, want 1", e.worker.aiReviews)
	}
}

func TestAIReviewRefusals(t *testing.T) {
	t.Run("reviewer off", func(t *testing.T) {
		e := newFlow(t)
		id := agentReviewIntent(t, e)
		r := e.do(e.repo, ExitError, "ai-review", id)
		if get(r, "error", "code") != api.CodeAIUnavailable || !strings.Contains(get(r, "error", "message").(string), "review --for agent") {
			t.Fatalf("ai-review = %v", r)
		}
		// Still waiting for a reviewer agent.
		if q := get(e.do(e.repo, ExitOK, "review", "--for", "agent"), "queue").([]any); len(q) != 1 {
			t.Fatalf("agent queue = %v", q)
		}
	})
	t.Run("routed to a person", func(t *testing.T) {
		e := newFlow(t)
		e.do(e.repo, ExitOK, "init", "--project", "demo")
		id := get(e.do(e.repo, ExitOK, "intent", "declare", "--goal", "Login", "--reason", "needed", "--path", "web/login.ts"), "intent", "id").(string)
		e.write(e.repo, "web/login.ts", "export function login() { return true; }\n")
		e.do(e.repo, ExitOK, "push", "-m", "feat: login")
		e.do(e.repo, ExitNeedsHuman, "propose")
		e.worker.aiVerdict = "approve"
		r := e.do(e.repo, ExitNeedsHuman, "ai-review", id)
		if get(r, "error", "code") != api.CodeNeedsHuman || e.worker.aiReviews != 0 {
			t.Fatalf("ai-review = %v (model calls %d)", r, e.worker.aiReviews)
		}
	})
	t.Run("nothing proposed", func(t *testing.T) {
		e := newFlow(t)
		e.do(e.repo, ExitOK, "init", "--project", "demo")
		id := get(e.do(e.repo, ExitOK, "intent", "declare", "--goal", "x", "--reason", "y", "--path", "docs/**"), "intent", "id").(string)
		e.worker.aiVerdict = "approve"
		r := e.do(e.repo, ExitError, "ai-review", id)
		if get(r, "error", "code") != "conflict" {
			t.Fatalf("ai-review = %v", r)
		}
	})
}
