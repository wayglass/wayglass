// Package intent models a declared unit of agent work: its goal, reason and
// expected paths, and the Artifacts fork it runs in. Overlap calculation
// between open intents lands here.
package intent

import "time"

// Status is where an intent is in its life.
type Status string

// Intent statuses.
const (
	StatusOpen      Status = "open"
	StatusProposed  Status = "proposed"
	StatusLanded    Status = "landed"
	StatusRejected  Status = "rejected"
	StatusAbandoned Status = "abandoned"
)

// Intent is declared before any code is written.
type Intent struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Goal      string    `json:"goal"`
	Reason    string    `json:"reason"`
	Paths     []string  `json:"paths"`             // declared path globs
	Touched   []string  `json:"touched,omitempty"` // actual changed files reported on push
	Agent     string    `json:"agent"`
	Repo      string    `json:"repo"` // Artifacts fork, <project>--<intent-id>
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}
