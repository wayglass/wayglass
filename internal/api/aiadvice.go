package api

import (
	"context"
	"net/http"
	"net/url"
)

// AIAdvice is what the AI wrote for the person a decision waits on
// (worker/src/aiadvice.ts). It never decides: the decision's status, action
// and decider are exactly as before.
type AIAdvice struct {
	AI          *AIMeta  `json:"ai,omitempty"`
	Summary     string   `json:"summary"`
	MatchesGoal string   `json:"matches_goal"` // yes, partly, no or unclear
	Risks       []string `json:"risks"`
	Questions   []string `json:"questions"`
	// Concerns is the stored copy's name for Warnings (detail.ai_advice).
	Concerns []string `json:"concerns,omitempty"`
	// Warnings are the Worker's, not the model's: text addressed to
	// reviewers in the change, a model that could not run, an answer off
	// the schema. Weigh the advice against them.
	Warnings []string `json:"warnings"`
	// Source is "model" when the model answered, "guard" when the advice is
	// the Worker's because the model did not or could not.
	Source       string      `json:"source"`
	Reviewer     string      `json:"reviewer"`
	Provider     string      `json:"provider"`
	Model        string      `json:"model"`
	Family       string      `json:"family"`
	Gateway      *string     `json:"gateway"`
	LatencyMS    int         `json:"latency_ms"`
	Tokens       *Tokens     `json:"tokens"`
	FallbackFrom *AIFallback `json:"fallback_from,omitempty"`
	Note         string      `json:"note,omitempty"`
	RequestedBy  string      `json:"requested_by,omitempty"`
	// By and At are set on the copy stored in a decision's detail.
	By string `json:"by,omitempty"`
	At string `json:"at,omitempty"`
}

// AIAdviceResult is POST /v1/intents/:id/ai-advice's answer.
type AIAdviceResult struct {
	IntentID string   `json:"intent_id"`
	Decision Decision `json:"decision"`
	Advice   AIAdvice `json:"advice"`
}

// AIAdviseIntent asks the Worker's AI for advice on the intent's latest
// decision, which must be waiting for a person. The advice is stored on the
// decision and audited; nothing about the decision changes.
func (c *Client) AIAdviseIntent(ctx context.Context, project, intentID string) (*AIAdviceResult, error) {
	var out AIAdviceResult
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/ai-advice", nil, map[string]string{"project": project}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
