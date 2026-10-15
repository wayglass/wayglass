package policy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wayglass/wayglass/internal/classify"
)

// WireVersion is the version of the EvaluateJSON request and response
// shapes. The Worker checks it, so a stale Wasm build fails loudly.
// v2 replaced `shadow` with `shadow_matches` and `would_differ`; v3 added
// `reviewer`; v4 added binary scanning fields and guards; v5 adds reviewer.kind and ai.
const WireVersion = 5

// WireChange is one file in a WireRequest.
type WireChange struct {
	Path         string   `json:"path"`
	AddedLines   []string `json:"added_lines"`
	RemovedCount int      `json:"removed_count"`
	// Binary: the new side cannot be read as text (a deleted binary is not
	// binary; nothing new arrives with it).
	Binary bool `json:"binary,omitzero"`
	// Bytes is the new side's size, when known.
	Bytes int `json:"bytes,omitzero"`
}

// WireRequest is the input to EvaluateJSON.
type WireRequest struct {
	// PolicyYAML is .wayglass/policy.yaml from the trunk at the intent's base
	// commit. Empty means the file does not exist there.
	PolicyYAML   string       `json:"policy_yaml"`
	Changes      []WireChange `json:"changes"`
	Jurisdiction string       `json:"jurisdiction"`
	// Truncated says the caller hit a size cap and Changes is incomplete.
	Truncated bool `json:"truncated,omitzero"`
}

// WireMatch is a rule reference in a WireResponse.
type WireMatch struct {
	ID        string `json:"id"`
	Action    Action `json:"action"`
	Approvers int    `json:"approvers,omitzero"`
	Reason    string `json:"reason"`
}

// WireResponse is the output of EvaluateJSON. Exactly one of Error and the
// decision fields is set.
type WireResponse struct {
	V           int           `json:"v"`
	Error       *WireError    `json:"error,omitempty"`
	Tags        classify.Tags `json:"tags"`
	MatchedRule string        `json:"matched_rule"`
	Action      Action        `json:"action"`
	Approvers   int           `json:"approvers"`
	// Reviewer constrains who may approve; the Worker enforces it.
	Reviewer *Reviewer `json:"reviewer,omitempty"`
	// AI tunes the Worker's AI reviewer or asks it for advice.
	AI      *AI    `json:"ai,omitempty"`
	Reason  string `json:"reason"`
	Default bool   `json:"default"`
	// ShadowMatches is every shadow rule that matched, in file order.
	ShadowMatches []ShadowMatch `json:"shadow_matches"`
	// WouldDiffer: enforcing the first preempting shadow rule would have
	// changed the action.
	WouldDiffer bool       `json:"would_differ"`
	Overrode    *WireMatch `json:"overrode,omitempty"`
	// PolicyError explains a builtin:invalid-policy decision.
	PolicyError string `json:"policy_error,omitempty"`
}

// WireError reports a malformed request. A bad policy file is not an error:
// it is a decision (builtin:invalid-policy) so nothing fails open.
type WireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func wireMatch(m Match) WireMatch {
	return WireMatch{ID: m.ID(), Action: m.Action, Approvers: m.Approvers, Reason: m.Reason}
}

// EvaluateWire is the server-side policy decision: parse the base policy,
// classify the change and decide. It does no I/O, so the same code runs in
// the CLI, in tests and inside the Worker as Wasm.
func EvaluateWire(req WireRequest) WireResponse {
	c := classify.Change{Jurisdiction: req.Jurisdiction, Files: make([]classify.FileChange, 0, len(req.Changes))}
	for _, ch := range req.Changes {
		c.Files = append(c.Files, classify.FileChange{Path: ch.Path, Added: ch.AddedLines, Deleted: ch.RemovedCount, Binary: ch.Binary, Bytes: ch.Bytes})
	}

	var (
		d      Decision
		polErr string
		allow  = DefaultBinaryAllow
	)
	switch {
	case strings.TrimSpace(req.PolicyYAML) == "":
		d = fallback(c, BuiltinNoPolicy, "The base commit has no .wayglass/policy.yaml; a person decides.")
	default:
		p, err := Parse([]byte(req.PolicyYAML))
		if err != nil {
			polErr = err.Error()
			d = fallback(c, BuiltinInvalidPolicy, "The base commit's policy file is invalid; a person decides.")
		} else {
			d = p.Evaluate(c)
			allow = p.File.Binary.Allow
		}
	}
	if touchesPolicy(c) {
		d = d.atLeast(HumanApprove, BuiltinProtectPolicy, protectReason)
	}
	d = d.guardUnscanned(c, allow)
	if req.Truncated {
		d = d.atLeast(HumanApprove, BuiltinTruncated, "The change is too large to classify in full; a person decides.")
	}

	resp := WireResponse{
		V:             WireVersion,
		Tags:          d.Tags,
		MatchedRule:   d.Rule.ID(),
		Action:        d.Action,
		Approvers:     d.Approvers,
		Reviewer:      d.Reviewer,
		AI:            d.AI,
		Reason:        d.Reason,
		Default:       d.Default,
		ShadowMatches: d.ShadowMatches,
		WouldDiffer:   d.WouldDiffer,
		PolicyError:   polErr,
	}
	if d.Overrode != nil {
		m := wireMatch(*d.Overrode)
		resp.Overrode = &m
	}
	return resp
}

// EvaluateJSON wraps EvaluateWire for callers that only speak bytes, such as
// the Wasm entry point. It never returns an error: failures are encoded in
// the response so the caller always gets JSON back.
func EvaluateJSON(in []byte) []byte {
	var req WireRequest
	if err := json.Unmarshal(in, &req); err != nil {
		return mustJSON(WireResponse{V: WireVersion, Error: &WireError{Code: "bad_request", Message: fmt.Sprintf("decode request: %v", err)}})
	}
	return mustJSON(EvaluateWire(req))
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Every field is a plain string, number, bool or slice of them.
		panic(err)
	}
	return b
}
