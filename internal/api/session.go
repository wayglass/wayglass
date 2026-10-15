package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// SessionRef is a parsed session URL:
// <worker>/v1/projects/<project>/sessions/<id>.
type SessionRef struct {
	Worker  string // scheme://host[:port][/prefix], no trailing slash
	Project string
	ID      string
}

// URL rebuilds the canonical session URL.
func (r SessionRef) URL() string {
	return r.Worker + "/v1/projects/" + r.Project + "/sessions/" + r.ID
}

var (
	sessionPath = regexp.MustCompile(`^(.*)/v1/projects/([a-z0-9][a-z0-9._-]{0,62})/sessions/(s[0-9a-f]{10})/?$`)
	sessionID   = regexp.MustCompile(`^s[0-9a-f]{10}$`)
)

// ParseSessionURL parses a URL printed by `wayglass session share`.
func ParseSessionURL(raw string) (SessionRef, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return SessionRef{}, fmt.Errorf("%q is not a session URL (want <worker>/v1/projects/<project>/sessions/<id>)", raw)
	}
	m := sessionPath.FindStringSubmatch(u.Path)
	if m == nil || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return SessionRef{}, fmt.Errorf("%q is not a session URL (want <worker>/v1/projects/<project>/sessions/<id>)", raw)
	}
	worker := u.Scheme + "://" + u.Host + m[1]
	if err := ValidateURL(worker); err != nil {
		return SessionRef{}, err
	}
	return SessionRef{Worker: worker, Project: m[2], ID: m[3]}, nil
}

// SameWorker reports whether a session URL points at this client's Worker.
// The CLI refuses to follow a session URL to any other host, so a pasted
// link can never make it send the Wayglass key somewhere else.
func (c *Client) SameWorker(ref SessionRef) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimRight(s, "/")) }
	return norm(c.BaseURL) == norm(ref.Worker)
}

// ShareSessionInput is the body of POST /v1/intents/:id/sessions.
type ShareSessionInput struct {
	Project string `json:"project"`
	Notes   string `json:"notes,omitempty"`
	// Commit, if set, must be the fork head; the Worker refuses otherwise.
	Commit string `json:"commit,omitempty"`
}

// ShareSession freezes an intent's context and returns its session URL.
func (c *Client) ShareSession(ctx context.Context, intentID string, in ShareSessionInput) (*ShareResult, error) {
	var out ShareResult
	if err := c.do(ctx, http.MethodPost, "/v1/intents/"+url.PathEscape(intentID)+"/sessions", nil, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func sessionPathFor(project, id string) (string, error) {
	if !sessionID.MatchString(id) {
		return "", &Error{Code: CodeBadRequest, Message: fmt.Sprintf("invalid session id %q", id)}
	}
	return "/v1/projects/" + url.PathEscape(project) + "/sessions/" + id, nil
}

// GetSession reads a shared session.
func (c *Client) GetSession(ctx context.Context, project, id string) (*SessionView, error) {
	p, err := sessionPathFor(project, id)
	if err != nil {
		return nil, err
	}
	var out SessionView
	if err := c.do(ctx, http.MethodGet, p, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ForkSession continues a shared session as a new intent owned by c.Agent,
// with its own fork and a short-lived write token for that fork only.
func (c *Client) ForkSession(ctx context.Context, project, id string) (*ForkSessionResult, error) {
	p, err := sessionPathFor(project, id)
	if err != nil {
		return nil, err
	}
	var out ForkSessionResult
	body := map[string]string{}
	if c.Family != "" {
		body["family"] = c.Family // whoever continues the work is its author
	}
	if err := c.do(ctx, http.MethodPost, p+"/fork", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
