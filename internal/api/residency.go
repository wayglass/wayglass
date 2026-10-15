package api

import (
	"context"
	"net/http"
	"net/url"
)

// Residency error codes from the Worker (worker/src/residency.ts): the
// project exists but its state is not yet, or no longer, in the jurisdiction
// it belongs to. Nothing is served from the wrong place; an admin runs
// `wayglass project migrate-residency <name>`.
const (
	CodeResidencyPending = "residency_pending"
	CodeProjectMoved     = "project_moved"
)

// ResidencyMigration is the reply to POST /v1/projects/:name/migrate-residency.
type ResidencyMigration struct {
	Project      string `json:"project"`
	Jurisdiction string `json:"jurisdiction"`
	// Migrated is false when the state was already in place (a rerun).
	Migrated bool `json:"migrated"`
	// Counts is rows per table, as copied and verified on both sides.
	Counts map[string]int `json:"counts"`
	// Digest is the SHA-256 of the copied state, equal on both sides.
	Digest string `json:"digest"`
	// SourceTombstoned is true once the unpinned object refuses every call and holds no state.
	SourceTombstoned bool `json:"source_tombstoned"`
}

// MigrateResidency moves a project's Worker state into the Durable Object
// pinned to its jurisdiction (admin key only). Safe to rerun.
func (c *Client) MigrateResidency(ctx context.Context, name string) (*ResidencyMigration, error) {
	var out ResidencyMigration
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(name)+"/migrate-residency", nil, map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
