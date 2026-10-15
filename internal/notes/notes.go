// Package notes reads and writes the why of a landed commit under
// refs/notes/wayglass. Git access goes through internal/gitx.
package notes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wayglass/wayglass/internal/classify"
	"github.com/wayglass/wayglass/internal/gitx"
)

// Ref is the git notes ref Wayglass owns.
const Ref = "refs/notes/wayglass"

// Refspec pushes or fetches the notes ref. Notes are not transferred by a
// plain push or fetch, so callers add this explicitly.
const Refspec = "+" + Ref + ":" + Ref

// Version is the note body version.
const Version = 1

// Note is the JSON body stored on a landed commit.
type Note struct {
	V          int           `json:"v"`
	IntentID   string        `json:"intent_id"`
	Goal       string        `json:"goal"`
	Reason     string        `json:"reason"`
	Tags       classify.Tags `json:"tags"`
	Rule       string        `json:"rule"`
	Action     string        `json:"action"`
	DecisionID string        `json:"decision_id,omitempty"`
	Status     string        `json:"status,omitempty"`
	DecidedBy  string        `json:"decided_by"`
	DecidedAt  time.Time     `json:"decided_at"`
	// IdentitySource is how the decider signed in ("access" for Cloudflare
	// Access, "key", "admin"); empty in notes written before Oct 14.
	IdentitySource string `json:"identity_source,omitempty"`
	// Approvals are the verdicts of the people or agents policy routed the
	// change to, with their reasons. Empty when policy decided alone.
	Approvals []Approval `json:"approvals,omitempty"`
}

// Approval is one verdict on the decision and the reason given for it.
type Approval struct {
	By string `json:"by"`
	// ClaimedBy is the name the admin key reported with its verdict; the
	// verdict itself counts as By ("admin").
	ClaimedBy string `json:"claimed_by,omitempty"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason"`
	At        string `json:"at,omitempty"`
	// IdentitySource is how this approver signed in; see Note.IdentitySource.
	IdentitySource string `json:"identity_source,omitempty"`
}

// ErrNoNote means the commit carries no Wayglass note.
var ErrNoNote = errors.New("no wayglass note on this commit")

// Write attaches n to commit, replacing any earlier Wayglass note on it.
func Write(ctx context.Context, g gitx.Git, commit string, n Note) error {
	if n.V == 0 {
		n.V = Version
	}
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	_, err = g.RunInput(ctx, strings.NewReader(string(b)+"\n"), "notes", "--ref="+Ref, "add", "--force", "--file=-", commit)
	return err
}

// Read returns the Wayglass note on commit, or ErrNoNote.
func Read(ctx context.Context, g gitx.Git, commit string) (Note, error) {
	var n Note
	out, err := g.Run(ctx, "notes", "--ref="+Ref, "show", commit)
	if err != nil {
		var ge *gitx.Error
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "no note found") {
			return n, ErrNoNote
		}
		return n, err
	}
	if err := json.Unmarshal([]byte(out), &n); err != nil {
		return n, fmt.Errorf("note on %s is not Wayglass JSON: %w", commit, err)
	}
	return n, nil
}

// Exists reports whether the local notes ref exists.
func Exists(ctx context.Context, g gitx.Git) (bool, error) {
	_, ok, err := g.Resolve(ctx, Ref)
	if err != nil {
		// The notes ref points at a commit, so Resolve works; any other
		// failure is real.
		return false, err
	}
	return ok, nil
}

// Push sends the notes ref to remote.
func Push(ctx context.Context, g gitx.Git, remote string) error {
	return g.Push(ctx, remote, Refspec)
}

// Fetch brings the remote's notes ref into the local one.
func Fetch(ctx context.Context, g gitx.Git, remote string) error {
	return g.Fetch(ctx, remote, Refspec)
}
