// Package audit defines the append-only audit events the Worker stores and
// the CLI and website display.
package audit

import "time"

// Kind names an audit event.
type Kind string

// Event kinds.
const (
	IntentDeclared Kind = "intent.declared"
	IntentPushed   Kind = "intent.pushed"
	OverlapFound   Kind = "overlap.found"
	ChangeProposed Kind = "change.proposed"
	Decided        Kind = "decision.made"
	ShadowDecided  Kind = "decision.shadow"
	ChangeLanded   Kind = "change.landed"
)

// Event is one append-only audit record. Who or what decided, which rule
// matched and why are always recorded for decisions.
type Event struct {
	ID       string    `json:"id"`
	Kind     Kind      `json:"kind"`
	Project  string    `json:"project"`
	IntentID string    `json:"intent_id,omitempty"`
	Actor    string    `json:"actor"` // agent or person
	Action   string    `json:"action,omitempty"`
	Rule     string    `json:"rule,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Commit   string    `json:"commit,omitempty"`
	At       time.Time `json:"at"`
}
