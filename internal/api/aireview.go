package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// CodeAIUnavailable: the Worker's AI reviewer is off (AI_REVIEW=off) or the
// Worker has no Workers AI binding; agent_review keeps waiting for a reviewer
// agent.
const CodeAIUnavailable = "ai_unavailable"

// SourceAI marks a verdict or escalation recorded by the Workers AI reviewer.
const SourceAI = "ai"

// AIReviewerPrefix starts the AI reviewer's name on the record:
// "workers-ai:<model>".
const AIReviewerPrefix = "workers-ai:"

// IsAIReviewer reports whether a decider name is the AI reviewer's.
func IsAIReviewer(by string) bool { return AIModel(by) != "" }

// AIModel returns the model behind an AI reviewer's name, or "" for anyone else.
func AIModel(by string) string {
	for _, prefix := range []string{"workers-ai:", "anthropic:", "openai:"} {
		if model, ok := strings.CutPrefix(by, prefix); ok {
			return model
		}
	}
	return ""
}

// AIVerdict is what the Workers AI reviewer answered, or what a guard
// decided in its place (worker/src/aireview.ts).
type AIVerdict struct {
	Verdict  string   `json:"verdict"` // approve or escalate
	Reason   string   `json:"reason"`
	Concerns []string `json:"concerns"`
	// Source is "model" when the model answered and passed every check, or
	// "guard" when a check escalated without or against the model (a diff
	// too large, an answer off the schema, a model error, low confidence).
	Source string `json:"source"`
	// Reviewer is who decided, on the record: "workers-ai:@cf/meta/..." or
	// "anthropic:claude-sonnet-5.5". Empty from Workers that predate it.
	Reviewer string `json:"reviewer,omitempty"`
	// Provider is workers-ai, anthropic or openai; Family the model vendor's
	// family (meta, claude, openai), which the cross-family rule sees.
	Provider   string   `json:"provider,omitempty"`
	Model      string   `json:"model"`
	Family     string   `json:"family,omitempty"`
	Gateway    *string  `json:"gateway"`
	LatencyMS  int      `json:"latency_ms"`
	Tokens     *Tokens  `json:"tokens"`
	Confidence *float64 `json:"confidence"`
	// FallbackFrom names the provider that failed before this one answered.
	FallbackFrom *AIFallback `json:"fallback_from,omitempty"`
	// Note says why a policy-pinned model was not used, if it was not.
	Note string `json:"note,omitempty"`
}

// AIFallback is a provider call that failed before another provider answered.
type AIFallback struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Error    string `json:"error"`
}

// ReviewerName is the AI reviewer's identity, with a fallback for older Workers.
func (v AIVerdict) ReviewerName() string {
	if v.Reviewer != "" {
		return v.Reviewer
	}
	return "workers-ai:" + v.Model
}

// Tokens counts what one model call read and wrote.
type Tokens struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
}

// AIReviewResult is POST /v1/intents/:id/ai-review's answer.
type AIReviewResult struct {
	IntentID  string     `json:"intent_id"`
	Decision  Decision   `json:"decision"`
	Approvals []Approval `json:"approvals"`
	Review    AIVerdict  `json:"review"`
}

// AIReviewIntent asks the Worker's AI reviewer to decide the intent's latest
// agent_review decision. The model can only approve it or hand it to a
// person; the Worker refuses everything else the way it refuses an agent key.
func (c *Client) AIReviewIntent(ctx context.Context, project, intentID string) (*AIReviewResult, error) {
	var out AIReviewResult
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/ai-review", nil, map[string]string{"project": project}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
