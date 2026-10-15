// Package api is the typed HTTP client for the Wayglass Worker. The Worker is
// the only holder of Cloudflare credentials; this client carries only a
// Wayglass key (today the Worker's admin key, later a per-agent session
// token) and, transiently, the short-lived repo tokens the Worker returns.
package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// EnvelopeVersion is the only Worker envelope version this client accepts.
const EnvelopeVersion = 1

// maxResponse caps how much of a response body is read.
const maxResponse = 8 << 20

// Worker error codes (worker/src/envelope.ts), plus client-side ones.
const (
	CodeBadRequest       = "bad_request"
	CodeUnauthorized     = "unauthorized"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeConflict         = "conflict"
	// CodeNotAuthor: only the intent's author or the admin key may land it.
	CodeNotAuthor = "not_author"
	// CodeNeedsHuman: policy routed the change to a person; an agent key
	// cannot decide it (only the admin key or a human key can).
	CodeNeedsHuman = "needs_human"
	// CodeNeedsAgent: the policy rule said `reviewer: agent`, so a reviewer
	// agent decides the change and the AI reviewer refuses it.
	CodeNeedsAgent = "needs_agent"
	// CodeSuperseded: the decision was superseded by a restack; the author
	// proposes again and the new decision is reviewed.
	CodeSuperseded = "superseded"
	// CodeDecisionReplaced: the intent was proposed again after the decision
	// named (or shown), so a verdict on it would be about a change nobody
	// reviewed; only an intent's latest decision is decided.
	CodeDecisionReplaced = "decision_replaced"
	// CodePolicyChanged: the trunk's .wayglass/policy.yaml changed after the
	// decision (or the Worker could not tell which policy it was made under),
	// so land refuses before any trunk token; the author merges the trunk
	// into the intent and proposes again.
	CodePolicyChanged = "policy_changed"
	CodeUpstream      = "upstream_error"
	CodeNotConfigured = "not_configured"
	CodeInternal      = "internal"
	// CodeBadResponse means the reply was not a Wayglass envelope (wrong URL,
	// proxy error page, version mismatch).
	CodeBadResponse = "bad_response"
)

// Error is a failure reported by the Worker, or a reply that could not be
// understood. Its text never contains the request's bearer key.
type Error struct {
	Status  int    // HTTP status; 0 when no response arrived
	Code    string // envelope error code
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("worker: %s: %s", e.Code, e.Message)
}

// IsCode reports whether err is an *Error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Client talks to one Worker deployment.
type Client struct {
	BaseURL string
	// Key is sent as "Authorization: Bearer <Key>". Never log it.
	Key string
	// Agent names the caller in the audit log (X-Wayglass-Agent).
	Agent string
	// Family is the caller's declared model family (WAYGLASS_AGENT_FAMILY),
	// sent on declare and on decisions. Self-reported.
	Family string
	HTTP   *http.Client
}

// New returns a client with a sane timeout.
func New(baseURL, key string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Key:     key,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

type envelope struct {
	V     int             `json:"v"`
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// do sends one request and decodes the envelope's data into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	if c.BaseURL == "" {
		return &Error{Code: CodeNotConfigured, Message: "no Worker URL; run 'wayglass login' or set WAYGLASS_URL"}
	}
	u, err := url.Parse(c.BaseURL + path)
	if err != nil {
		return &Error{Code: CodeNotConfigured, Message: fmt.Sprintf("bad Worker URL %q", c.BaseURL)}
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	if c.Agent != "" {
		req.Header.Set("X-Wayglass-Agent", c.Agent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// url.Error includes the URL but never headers, so the key stays out.
		return fmt.Errorf("worker request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("read worker response: %w", err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.V == 0 {
		return &Error{Status: resp.StatusCode, Code: CodeBadResponse,
			Message: fmt.Sprintf("%s %s returned HTTP %d without a Wayglass envelope; is the Worker URL right?", method, path, resp.StatusCode)}
	}
	if env.V != EnvelopeVersion {
		return &Error{Status: resp.StatusCode, Code: CodeBadResponse,
			Message: fmt.Sprintf("worker speaks envelope v%d, this client v%d; update wayglass", env.V, EnvelopeVersion)}
	}
	if !env.OK {
		if env.Error == nil {
			return &Error{Status: resp.StatusCode, Code: CodeBadResponse, Message: "ok:false without an error"}
		}
		return &Error{Status: resp.StatusCode, Code: env.Error.Code, Message: env.Error.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return &Error{Status: resp.StatusCode, Code: CodeBadResponse, Message: fmt.Sprintf("decode %s %s: %v", method, path, err)}
	}
	return nil
}

func projectQuery(project string) url.Values {
	return url.Values{"project": {project}}
}

// CheckAuth verifies the key without touching any project and says who the
// Worker takes the caller to be. Workers from before GET /v1/whoami answer
// not_found; for them it falls back to an authorised request that fails on
// the missing project, and returns a nil WhoAmI.
func (c *Client) CheckAuth(ctx context.Context) (*WhoAmI, error) {
	who, err := c.WhoAmI(ctx)
	if err == nil {
		return who, nil
	}
	if !IsCode(err, CodeNotFound) {
		return nil, err
	}
	err = c.do(ctx, http.MethodGet, "/v1/intents", nil, nil, nil)
	if err == nil || IsCode(err, CodeBadRequest) {
		return nil, nil
	}
	return nil, err
}

// CreateProject creates the project and its trunk repo. The returned token
// is a short-lived trunk write token for the first push.
func (c *Client) CreateProject(ctx context.Context, name, description string) (*CreateProjectResult, error) {
	return c.CreateProjectIn(ctx, name, description, "")
}

// CreateProjectIn creates a project whose repos live in the namespace for
// jurisdiction ("us", "eu"; "" = the Worker's main one). It cannot be changed later.
func (c *Client) CreateProjectIn(ctx context.Context, name, description, jurisdiction string) (*CreateProjectResult, error) {
	return c.CreateProjectWith(ctx, CreateProjectInput{Name: name, Description: description, Jurisdiction: jurisdiction})
}

// CreateProjectInput is the body of POST /v1/projects.
type CreateProjectInput struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Jurisdiction string `json:"jurisdiction,omitempty"` // "" = the Worker's main namespace
	// Public opts the project in to the unauthenticated read view. The
	// Worker's PUBLIC_PROJECTS must also match the name for it to show.
	Public bool `json:"public,omitempty"`
}

// CreateProjectWith creates a project from a full input.
func (c *Client) CreateProjectWith(ctx context.Context, in CreateProjectInput) (*CreateProjectResult, error) {
	var out CreateProjectResult
	body := in
	if err := c.do(ctx, http.MethodPost, "/v1/projects", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeclareIntentInput is the body of POST /v1/intents.
type DeclareIntentInput struct {
	Project string   `json:"project"`
	Goal    string   `json:"goal"`
	Reason  string   `json:"reason"`
	Paths   []string `json:"paths"`
	Agent   string   `json:"agent,omitempty"`
	// Family is the author's model family; the client's Family if empty.
	Family string `json:"family,omitempty"`
	// On stacks the intent on another unlanded intent: the Worker forks the
	// parent's fork and diffs against the parent's head.
	On string `json:"on,omitempty"`
}

// DeclareIntent registers an intent; the Worker forks the trunk for it.
func (c *Client) DeclareIntent(ctx context.Context, in DeclareIntentInput) (*DeclareResult, error) {
	var out DeclareResult
	in.Family = cmp.Or(in.Family, c.Family)
	if err := c.do(ctx, http.MethodPost, "/v1/intents", nil, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListIntents lists a project's intents; status "" means all.
func (c *Client) ListIntents(ctx context.Context, project, status string) ([]Intent, error) {
	q := projectQuery(project)
	if status != "" {
		q.Set("status", status)
	}
	var out struct {
		Intents []Intent `json:"intents"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/intents", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Intents, nil
}

// GetIntent finds one intent. The Worker has no single-intent route yet, so
// this filters the list.
func (c *Client) GetIntent(ctx context.Context, project, id string) (*Intent, error) {
	all, err := c.ListIntents(ctx, project, "")
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, &Error{Status: http.StatusNotFound, Code: CodeNotFound, Message: fmt.Sprintf("intent %s not found in project %s", id, project)}
}

// ReadTokenResult is the reply to POST /v1/intents/:id/read-token.
type ReadTokenResult struct {
	Intent Intent `json:"intent"`
	Fork   struct {
		Repo   string `json:"repo"`
		Remote string `json:"remote"`
	} `json:"fork"`
	Token Token `json:"token"`
}

// ReadToken asks the Worker for a short-lived read-only token for an
// intent's fork, so a reviewer can see the change it decides on.
func (c *Client) ReadToken(ctx context.Context, project, intentID string) (*ReadTokenResult, error) {
	var out ReadTokenResult
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/read-token", nil, map[string]string{"project": project}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ForkToken asks the Worker for a fresh short-lived write token for an
// intent's fork. Not every Worker has this route yet; callers fall back to a
// token the agent kept from `intent declare`.
func (c *Client) ForkToken(ctx context.Context, project, intentID string) (*Token, error) {
	var out struct {
		Token Token `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/token", nil, map[string]string{"project": project}, &out); err != nil {
		return nil, err
	}
	return &out.Token, nil
}

// PushInput is the body of POST /v1/intents/:id/push.
type PushInput struct {
	Project string   `json:"project"`
	Paths   []string `json:"paths"`
	Commit  string   `json:"commit,omitempty"`
}

// ReportPush tells the Worker which files an intent changed and gets back
// the live overlaps.
func (c *Client) ReportPush(ctx context.Context, intentID string, in PushInput) (*PushResult, error) {
	var out PushResult
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/push", nil, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ProposeInput is the body of POST /v1/intents/:id/propose. Tags, rule and
// action are the client's preview; the Worker's decision is authoritative.
type ProposeInput struct {
	Project         string `json:"project"`
	Commit          string `json:"commit,omitempty"`
	Tags            any    `json:"tags"`
	MatchedRule     string `json:"matched_rule,omitempty"`
	Action          string `json:"action"`
	Reason          string `json:"reason"`
	ApprovalsNeeded int    `json:"approvals_needed,omitzero"`
}

// Propose records a proposal and returns the Worker's decision.
func (c *Client) Propose(ctx context.Context, intentID string, in ProposeInput) (*ProposeResult, error) {
	var out ProposeResult
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/propose", nil, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DecideInput is the body of POST /v1/decisions/:id.
type DecideInput struct {
	Project string `json:"project"`
	Verdict string `json:"verdict"` // approve or reject
	By      string `json:"by,omitempty"`
	Reason  string `json:"reason"`
	// Family is the decider's model family; the client's Family if empty.
	Family string `json:"family,omitempty"`
	// DecisionID binds a verdict sent by intent (DecideIntent) to the
	// decision the reviewer saw; the Worker refuses (decision_replaced) when
	// the intent was proposed again since. Commit does the same by fork
	// commit (a prefix will do). Either is enough.
	DecisionID string `json:"decision_id,omitempty"`
	Commit     string `json:"commit,omitempty"`
}

// Decide approves or rejects a pending decision.
func (c *Client) Decide(ctx context.Context, decisionID string, in DecideInput) (*DecideResult, error) {
	var out DecideResult
	in.Family = cmp.Or(in.Family, c.Family)
	if err := c.do(ctx, http.MethodPost, "/v1/decisions/"+url.PathEscape(decisionID), nil, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DecideIntent approves or rejects an intent's latest decision
// (POST /v1/intents/:id/decide), so people need not know decision ids. Set
// in.DecisionID (or in.Commit) to what was reviewed: once an intent has been
// proposed more than once the Worker refuses an unbound verdict.
func (c *Client) DecideIntent(ctx context.Context, intentID string, in DecideInput) (*DecideResult, error) {
	var out DecideResult
	in.Family = cmp.Or(in.Family, c.Family)
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/decide", nil, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Land reserves the trunk for an approved intent and returns a trunk write
// token that lives LAND_TOKEN_TTL_SECONDS (300 s).
func (c *Client) Land(ctx context.Context, project, intentID string) (*LandResult, error) {
	var out LandResult
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/land", nil, map[string]string{"project": project}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AbortLandResult is the reply to POST /v1/intents/:id/land/abort.
type AbortLandResult struct {
	Land         Land `json:"land"`
	TokenRevoked bool `json:"token_revoked"`
}

// AbortLand gives up a land the client could not finish: the Worker revokes
// the trunk token and frees the project's land slot for the next intent.
func (c *Client) AbortLand(ctx context.Context, project, intentID, reason string) (*AbortLandResult, error) {
	var out AbortLandResult
	body := map[string]string{"project": project, "reason": reason}
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/land/abort", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AbandonResult is the reply to POST /v1/intents/:id/abandon.
type AbandonResult struct {
	Intent Intent `json:"intent"`
	// Restacked lists intents that were stacked on this one; they now carry
	// its commits and must propose again before they land.
	Restacked     []string `json:"restacked"`
	TokensRevoked int      `json:"tokens_revoked"`
	ForkKept      bool     `json:"fork_kept"`
}

// AbandonIntent gives up an open or proposed intent. Only its author or the
// admin may; the reason is required and audited. The fork repo is kept.
func (c *Client) AbandonIntent(ctx context.Context, project, intentID, reason string) (*AbandonResult, error) {
	var out AbandonResult
	body := map[string]string{"project": project, "reason": reason}
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/abandon", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RestackResult is the reply to POST /v1/intents/:id/restack.
type RestackResult struct {
	// Intent is the intent after the restack: stacked on the new parent, in
	// a new fork whose base is the parent's head.
	Intent Intent `json:"intent"`
	Fork   Repo   `json:"fork"`
	// Token writes to the new fork only. A secret: never print or store it.
	Token    Token           `json:"token"`
	Previous RestackPrevious `json:"previous"`
	// SupersededDecision is the decision made before the restack, if any; it
	// no longer covers the change, so land refuses it until a new propose.
	SupersededDecision *string   `json:"superseded_decision"`
	Overlaps           []Overlap `json:"overlaps"`
}

// RestackPrevious is where the intent was before a restack. Its old fork is
// kept; its live tokens there were revoked.
type RestackPrevious struct {
	ParentID      *string `json:"parent_id"`
	BaseCommit    *string `json:"base_commit"`
	ForkRepo      string  `json:"fork_repo"`
	ForkRemote    string  `json:"fork_remote"`
	ForkKept      bool    `json:"fork_kept"`
	TokensRevoked int     `json:"tokens_revoked"`
}

// RestackIntent moves an open or proposed intent onto another unlanded
// intent (the overlap re-plan "stack"). Only its author or the admin may;
// the reason is required and audited. The Worker forks the parent's fork;
// the caller merges the intent's commits into it with the returned token.
func (c *Client) RestackIntent(ctx context.Context, project, intentID, on, reason string) (*RestackResult, error) {
	var out RestackResult
	body := map[string]string{"project": project, "on": on, "reason": reason}
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/restack", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Landed closes a land: the Worker revokes the trunk token and checks that
// the trunk now holds exactly the approved change.
func (c *Client) Landed(ctx context.Context, project, intentID, commit string) (*LandedResult, error) {
	var out LandedResult
	body := map[string]string{"project": project, "commit": commit}
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/landed", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Audit returns audit events after seq `since`.
func (c *Client) Audit(ctx context.Context, project string, since, limit int) (*AuditPage, error) {
	q := projectQuery(project)
	q.Set("since", fmt.Sprint(since))
	q.Set("limit", fmt.Sprint(limit))
	var out AuditPage
	if err := c.do(ctx, http.MethodGet, "/v1/audit", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListDecisions returns a project's decisions with their approvals, newest
// last. status filters when not empty.
func (c *Client) ListDecisions(ctx context.Context, project, status string) ([]Decision, error) {
	q := projectQuery(project)
	if status != "" {
		q.Set("status", status)
	}
	var out struct {
		Decisions []Decision `json:"decisions"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/decisions", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Decisions, nil
}

// Why returns the intent and decision recorded for a commit.
func (c *Client) Why(ctx context.Context, project, commit string) (*WhyResult, error) {
	q := projectQuery(project)
	q.Set("commit", commit)
	var out WhyResult
	if err := c.do(ctx, http.MethodGet, "/v1/why", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Cost reads what the project has cost in Artifacts operations, as far as
// the Worker can see: its own binding calls per intent, plus trunk pushes
// seen through Artifacts events. Client git traffic to forks is not in it.
func (c *Client) Cost(ctx context.Context, project string) (*CostSummary, error) {
	var out CostSummary
	if err := c.do(ctx, http.MethodGet, "/v1/cost", projectQuery(project), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
