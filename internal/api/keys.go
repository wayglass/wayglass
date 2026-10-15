package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// AgentKeyPrefix starts every per-agent key; anything else is the admin key.
const AgentKeyPrefix = "wgk_"

// WhoAmI is who the Worker thinks the caller is.
type WhoAmI struct {
	// Role is "admin" (the Worker's admin key) or "agent" (a per-agent key).
	Role string `json:"role"`
	// Agent is the name the audit log will record: bound to the key for
	// agent keys, self-reported (X-Wayglass-Agent) for the admin key.
	Agent string `json:"agent"`
	// Bound is true when Agent comes from the key and cannot be changed.
	Bound bool    `json:"bound"`
	KeyID *string `json:"key_id"`
	// Kind is "human" (the admin key or a key minted with --human) or
	// "agent". Only a person may decide a change policy routed to a person.
	// Empty from Workers that predate kinds.
	Kind string `json:"kind,omitempty"`
	// Source is "admin" (the admin key), "key" (minted by the admin) or
	// "access" (a person signed in through Cloudflare Access). Empty from
	// older Workers.
	Source string `json:"source,omitempty"`
	// ExpiresAt is when the key stops working (RFC 3339); nil for keys that
	// never expire.
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// SourceAccess marks a human key minted for a Cloudflare Access sign-in.
const SourceAccess = "access"

// Key kinds (worker/src/keys.ts).
const (
	KindHuman = "human"
	KindAgent = "agent"
)

// WhoAmI asks the Worker who this key is (GET /v1/whoami).
func (c *Client) WhoAmI(ctx context.Context) (*WhoAmI, error) {
	var out WhoAmI
	if err := c.do(ctx, http.MethodGet, "/v1/whoami", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AgentKey is a per-agent key as listed by the admin. It never carries the key.
type AgentKey struct {
	ID         string  `json:"id"`
	Agent      string  `json:"agent"`
	Kind       string  `json:"kind,omitempty"`
	Source     string  `json:"source,omitempty"`
	ExpiresAt  *string `json:"expires_at,omitempty"`
	Note       string  `json:"note"`
	CreatedBy  string  `json:"created_by"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	RevokedAt  *string `json:"revoked_at"`
}

// NewAgentKey is the result of minting a key: the plaintext appears only here.
type NewAgentKey struct {
	Key  string   `json:"key"`
	Info AgentKey `json:"info"`
}

// CreateAgentKey mints a key bound to agent (admin key only). kind is
// KindHuman for a person's key, or KindAgent ("" means agent).
func (c *Client) CreateAgentKey(ctx context.Context, agent, note, kind string) (*NewAgentKey, error) {
	var out NewAgentKey
	body := map[string]string{"agent": agent}
	if note != "" {
		body["note"] = note
	}
	if kind != "" {
		body["kind"] = kind
	}
	if err := c.do(ctx, http.MethodPost, "/v1/keys", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAgentKeys lists keys (admin key only); all includes revoked ones.
func (c *Client) ListAgentKeys(ctx context.Context, all bool) ([]AgentKey, error) {
	var q url.Values
	if all {
		q = url.Values{"all": {"1"}}
	}
	var out struct {
		Keys []AgentKey `json:"keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/keys", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Keys, nil
}

// RevokeAgentKey revokes a key by id (admin key only).
func (c *Client) RevokeAgentKey(ctx context.Context, id string) (*AgentKey, error) {
	var out AgentKey
	if err := c.do(ctx, http.MethodDelete, "/v1/keys/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// IsAgentKey reports whether key is a per-agent key (by its prefix only).
func IsAgentKey(key string) bool { return strings.HasPrefix(key, AgentKeyPrefix) }

// TrunkReadToken is a short read-only token for a project's trunk.
type TrunkReadToken struct {
	Trunk struct {
		Name   string `json:"name"`
		Remote string `json:"remote"`
	} `json:"trunk"`
	Token Token `json:"token"`
}

// TrunkReadToken asks for a read-only trunk token (about 60 s). Never store it.
func (c *Client) TrunkReadToken(ctx context.Context, project string) (*TrunkReadToken, error) {
	var out TrunkReadToken
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+url.PathEscape(project)+"/trunk/read-token", nil, map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
