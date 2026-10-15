package cli

import (
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
)

// humanRoutedIntent declares, pushes and proposes an auth change the example
// policy routes to a person (human_approve), returning the intent id.
func humanRoutedIntent(t *testing.T, e *flowEnv) string {
	t.Helper()
	e.do(e.repo, ExitOK, "init", "--project", "demo")
	id := get(e.do(e.repo, ExitOK, "intent", "declare", "--goal", "Login page", "--reason", "Users asked", "--path", "web/login.ts"), "intent", "id").(string)
	e.write(e.repo, "web/login.ts", "export function login() { return true; }\n")
	e.do(e.repo, ExitOK, "push", "-m", "feat: login")
	pr := e.do(e.repo, ExitNeedsHuman, "propose")
	if get(pr, "decision", "status") != api.StatusPendingHuman {
		t.Fatalf("propose = %v", pr)
	}
	return id
}

func TestAIAdviceWritesAdviceAndDecidesNothing(t *testing.T) {
	e := newFlow(t)
	id := humanRoutedIntent(t, e)
	e.worker.aiVerdict = "approve" // the fake's "AI is on"; advice has no verdict
	r := e.do(e.repo, ExitOK, "ai-advice", id)
	if get(r, "advice", "matches_goal") != "partly" || get(r, "advice", "source") != "model" || get(r, "advice", "reviewer") != fakeAIReviewer {
		t.Fatalf("ai-advice = %v", r)
	}
	if risks := get(r, "advice", "risks").([]any); len(risks) != 1 || !strings.Contains(risks[0].(string), "bypass") {
		t.Fatalf("risks = %v", risks)
	}
	// The decision is exactly where it was: waiting for a person, no decider.
	if get(r, "decision", "status") != api.StatusPendingHuman || get(r, "decision", "decided_by") != nil {
		t.Fatalf("decision = %v", get(r, "decision"))
	}
	if get(r, "decision", "detail", "ai_advice", "by") != fakeAIReviewer {
		t.Fatalf("detail.ai_advice = %v", get(r, "decision", "detail", "ai_advice"))
	}
	if next, _ := get(r, "next").(string); !strings.Contains(next, "unchanged") {
		t.Fatalf("next = %q", next)
	}
	if u := get(r, "untrusted").([]any); len(u) != 3 {
		t.Fatalf("untrusted = %v", u)
	}
	// Still in the person's queue; the person decides.
	q := e.do(e.repo, ExitOK, "review")
	if items := get(q, "queue").([]any); len(items) != 1 || get(items[0].(map[string]any), "intent", "id") != id {
		t.Fatalf("review queue = %v", q)
	}
	if e.worker.aiReviews != 1 {
		t.Fatalf("model calls = %d, want 1", e.worker.aiReviews)
	}
}

func TestAIAdviceRefusals(t *testing.T) {
	t.Run("ai off", func(t *testing.T) {
		e := newFlow(t)
		id := humanRoutedIntent(t, e)
		r := e.do(e.repo, ExitError, "ai-advice", id)
		if get(r, "error", "code") != api.CodeAIUnavailable || !strings.Contains(get(r, "error", "message").(string), "wayglass diff") {
			t.Fatalf("ai-advice = %v", r)
		}
	})
	t.Run("agent_review is reviewed, not advised", func(t *testing.T) {
		e := newFlow(t)
		id := agentReviewIntent(t, e)
		e.worker.aiVerdict = "approve"
		r := e.do(e.repo, ExitError, "ai-advice", id)
		if get(r, "error", "code") != "conflict" || !strings.Contains(get(r, "error", "message").(string), "reviewed, not advised") || e.worker.aiReviews != 0 {
			t.Fatalf("ai-advice = %v (model calls %d)", r, e.worker.aiReviews)
		}
	})
}
