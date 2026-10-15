package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// ProjectEntry is one row of GET /v1/projects (worker/src/browse.ts): what
// a repo browser needs without opening the project. Any authenticated key
// gets the same rows; nothing in a row is a secret (clone goes through a
// short-lived read token, see TrunkReadToken).
type ProjectEntry struct {
	Name         string `json:"name"`
	Jurisdiction string `json:"jurisdiction"`
	TrunkRepo    string `json:"trunk_repo,omitempty"`
	TrunkRemote  string `json:"trunk_remote,omitempty"`
	// DefaultBranch is "main" for every trunk Artifacts makes today.
	DefaultBranch string `json:"default_branch,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
	Public        bool   `json:"public"`
	// Residency is "in_place", or residency_pending / project_moved for a
	// project whose state is not where its jurisdiction says (counts are
	// nil then; an admin runs `wayglass project migrate-residency`).
	Residency string `json:"residency,omitempty"`
	// LastActivity is the newest audit event; nil for a project with none.
	LastActivity *string `json:"last_activity"`
	// LastPushAt is the last push an intent reported; nil if none.
	LastPushAt *string        `json:"last_push_at"`
	Counts     *ProjectCounts `json:"counts"`
}

// ProjectCounts summarises a project's intents and decisions.
type ProjectCounts struct {
	Intents int `json:"intents"`
	// Open counts intents open or proposed.
	Open   int `json:"open"`
	Agents int `json:"agents"`
	// PendingHuman counts decisions waiting for a person (pending_human or
	// quarantined) whose intent is still active.
	PendingHuman int `json:"pending_human"`
	Landed       int `json:"landed"`
}

// ProjectsPage is one page of the directory. A page walks one namespace
// page of Artifacts repos, so it can be empty and still have a Next.
type ProjectsPage struct {
	Projects []ProjectEntry `json:"projects"`
	Q        *string        `json:"q"`
	Next     *string        `json:"next"`
}

// maxDirectoryPages bounds AllProjects: 200 pages of 20 repos is far past
// any namespace this tool manages, and keeps a looping cursor from running
// forever.
const maxDirectoryPages = 200

// ListProjects fetches one directory page. q is a case-insensitive substring
// of the name, matched by the Worker; cursor is Next from the previous page.
func (c *Client) ListProjects(ctx context.Context, q, cursor string) (*ProjectsPage, error) {
	query := url.Values{}
	if q != "" {
		query.Set("q", q)
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	var out ProjectsPage
	if err := c.do(ctx, http.MethodGet, "/v1/projects", query, nil, &out); err != nil {
		return nil, err
	}
	if out.Projects == nil {
		out.Projects = []ProjectEntry{}
	}
	return &out, nil
}

// AllProjects walks every directory page and returns the rows in directory
// order. The Worker filters by q, so a search costs the same page walk as
// a full listing, with fewer rows.
func (c *Client) AllProjects(ctx context.Context, q string) ([]ProjectEntry, error) {
	out := []ProjectEntry{}
	cursor := ""
	for n := 0; ; n++ {
		if n == maxDirectoryPages {
			return nil, fmt.Errorf("project directory did not end after %d pages", n)
		}
		page, err := c.ListProjects(ctx, q, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Projects...)
		if page.Next == nil || *page.Next == "" {
			return out, nil
		}
		cursor = *page.Next
	}
}

// CloneCommand is how a person or agent checks a listed project out: the
// CLI mints a short-lived read token for it. The project's git remote is
// never used with a long-lived credential.
func CloneCommand(name string) string { return "wayglass clone " + name }
