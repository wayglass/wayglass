package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// DeletablePrefixes are the only project names the Worker will delete
// (worker/src/index.ts, DELETABLE_PREFIXES): throwaway test projects. The
// Worker enforces the rule; the CLI checks it first only to fail fast.
var DeletablePrefixes = []string{"demo-test-", "eu-demo-test-"}

// IsDeletableProject reports whether name is a throwaway test project.
func IsDeletableProject(name string) bool {
	for _, p := range DeletablePrefixes {
		if strings.HasPrefix(name, p) && len(name) > len(p) {
			return true
		}
	}
	return false
}

// ProjectDeletion is what DELETE /v1/projects/:name removed.
type ProjectDeletion struct {
	Project string `json:"project"`
	// Existed is false when the Worker had no state for the project (already deleted).
	Existed              bool     `json:"existed"`
	Actor                string   `json:"actor,omitempty"`
	Jurisdiction         string   `json:"jurisdiction,omitempty"`
	Namespace            string   `json:"namespace,omitempty"`
	ReposDeleted         []string `json:"repos_deleted"`
	ReposAlreadyGone     []string `json:"repos_already_gone"`
	TokensRevoked        int      `json:"tokens_revoked"`
	SubscriptionsRemoved int      `json:"subscriptions_removed"`
	SubscriptionsLeft    []string `json:"subscriptions_left,omitempty"`
	Counts               *struct {
		Intents     int `json:"intents"`
		Decisions   int `json:"decisions"`
		AuditEvents int `json:"audit_events"`
		Sessions    int `json:"sessions"`
	} `json:"counts,omitempty"`
	StateWiped bool   `json:"state_wiped"`
	At         string `json:"at,omitempty"`
}

// DeleteProject deletes a throwaway project's repos and Worker state (admin
// key only). confirm must repeat the name.
func (c *Client) DeleteProject(ctx context.Context, name, confirm string) (*ProjectDeletion, error) {
	var out ProjectDeletion
	if err := c.do(ctx, http.MethodDelete, "/v1/projects/"+url.PathEscape(name), nil, map[string]string{"confirm": confirm}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
