package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL+"/", "k3y")
	c.Agent = "claude"
	return c
}

func writeOK(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"v": 1, "ok": true, "data": data})
}

func writeFail(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"v": 1, "ok": false, "error": map[string]string{"code": code, "message": msg}})
}

func TestDeclareSendsAuthAndDecodes(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/intents" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k3y" {
			t.Errorf("auth = %q", got)
		}
		if got := r.Header.Get("X-Wayglass-Agent"); got != "claude" {
			t.Errorf("agent = %q", got)
		}
		var in DeclareIntentInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in.Project != "demo" || in.Goal != "g" || len(in.Paths) != 1 {
			t.Errorf("body = %+v", in)
		}
		writeOK(w, http.StatusCreated, map[string]any{
			"intent":   map[string]any{"id": "i0123456789", "goal": "g", "status": "open", "head_commit": nil},
			"fork":     map[string]any{"name": "demo--i0123456789", "remote": "https://x/git/ns/demo--i0123456789.git", "defaultBranch": "main"},
			"token":    map[string]any{"value": "art_v1_x", "scope": "write", "expires_at": "2026-10-11T00:00:00Z"},
			"overlaps": []any{map[string]any{"intent_id": "iaaaaaaaaaa", "paths": []any{map[string]string{"mine": "web/**", "theirs": "web/login.ts"}}}},
		})
	})
	res, err := c.DeclareIntent(t.Context(), DeclareIntentInput{Project: "demo", Goal: "g", Reason: "r", Paths: []string{"web/**"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Intent.ID != "i0123456789" || res.Fork.DefaultBranch != "main" || res.Token.Value != "art_v1_x" {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Overlaps) != 1 || res.Overlaps[0].Paths[0].Theirs != "web/login.ts" {
		t.Fatalf("overlaps = %+v", res.Overlaps)
	}
	if res.Intent.Head() != "" {
		t.Fatalf("head = %q", res.Intent.Head())
	}
}

func TestErrorEnvelope(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFail(w, http.StatusConflict, "conflict", "project demo already exists")
	})
	_, err := c.CreateProject(t.Context(), "demo", "")
	var e *Error
	if !IsCode(err, CodeConflict) || !errors.As(err, &e) || e.Status != http.StatusConflict {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("message lost: %v", err)
	}
}

func TestNonEnvelopeIsBadResponse(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"html": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "<html>bad gateway</html>")
		},
		"json without v": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"ok":true}`)
		},
		"future version": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"v":2,"ok":true,"data":{}}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := serve(t, h).ListIntents(t.Context(), "demo", "")
			if !IsCode(err, CodeBadResponse) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestQueryParameters(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/v1/intents":
			if q.Get("project") != "demo" || q.Get("status") != "open" {
				t.Errorf("query = %v", q)
			}
			writeOK(w, 200, map[string]any{"intents": []any{map[string]any{"id": "i1"}, map[string]any{"id": "i2", "head_commit": "abc1234"}}})
		case "/v1/why":
			if q.Get("commit") != "abc1234" {
				t.Errorf("query = %v", q)
			}
			writeOK(w, 200, map[string]any{"commit": "abc1234", "intent": map[string]any{"id": "i2"}, "decision": map[string]any{"id": "d1", "action": "auto_merge"}, "approvals": []any{}})
		case "/v1/audit":
			if q.Get("since") != "5" || q.Get("limit") != "10" {
				t.Errorf("query = %v", q)
			}
			writeOK(w, 200, map[string]any{"events": []any{map[string]any{"seq": 6, "kind": "intent.pushed", "data": map[string]any{}}}, "next": 6})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	})
	ctx := t.Context()
	in, err := c.ListIntents(ctx, "demo", "open")
	if err != nil || len(in) != 2 || in[1].Head() != "abc1234" {
		t.Fatalf("list = %+v, %v", in, err)
	}
	w, err := c.Why(ctx, "demo", "abc1234")
	if err != nil || w.Decision.Action != "auto_merge" {
		t.Fatalf("why = %+v, %v", w, err)
	}
	a, err := c.Audit(ctx, "demo", 5, 10)
	if err != nil || a.Next != 6 || a.Events[0].Kind != "intent.pushed" {
		t.Fatalf("audit = %+v, %v", a, err)
	}
}

func TestGetIntentFiltersList(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		writeOK(w, 200, map[string]any{"intents": []any{map[string]any{"id": "i1", "goal": "one"}}})
	})
	got, err := c.GetIntent(t.Context(), "demo", "i1")
	if err != nil || got.Goal != "one" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := c.GetIntent(t.Context(), "demo", "i9"); !IsCode(err, CodeNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestPushAndPropose(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/v1/intents/i1/push":
			if body["commit"] != "abc1234" || len(body["paths"].([]any)) != 2 {
				t.Errorf("push body = %v", body)
			}
			writeOK(w, 200, map[string]any{"intent": map[string]any{"id": "i1"}, "overlaps": []any{}})
		case "/v1/intents/i1/propose":
			if body["action"] != "human_approve" || body["matched_rule"] != "rules[2]" {
				t.Errorf("propose body = %v", body)
			}
			if _, ok := body["approvals_needed"]; ok {
				t.Errorf("approvals_needed must be omitted when zero: %v", body)
			}
			writeOK(w, 201, map[string]any{"intent": map[string]any{"id": "i1", "status": "proposed"}, "decision": map[string]any{"id": "d1", "status": "pending_human", "action": "human_approve"}})
		}
	})
	p, err := c.ReportPush(t.Context(), "i1", PushInput{Project: "demo", Paths: []string{"a", "b"}, Commit: "abc1234"})
	if err != nil || p.Intent.ID != "i1" {
		t.Fatalf("push = %+v, %v", p, err)
	}
	r, err := c.Propose(t.Context(), "i1", ProposeInput{Project: "demo", Tags: map[string]any{}, MatchedRule: "rules[2]", Action: "human_approve", Reason: "Auth"})
	if err != nil || r.Decision.Status != StatusPendingHuman {
		t.Fatalf("propose = %+v, %v", r, err)
	}
}

func TestCheckAuth(t *testing.T) {
	// A Worker with GET /v1/whoami.
	current := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeOK(w, 200, map[string]any{"role": "agent", "agent": "ci-bot", "bound": true, "key_id": "k0123456789"})
	})
	who, err := current.CheckAuth(t.Context())
	if err != nil || who == nil || who.Agent != "ci-bot" || !who.Bound || who.Role != "agent" {
		t.Fatalf("current worker: %+v, %v", who, err)
	}

	// A Worker from before whoami: not_found there, bad_request (authorised, no project) elsewhere.
	older := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/whoami" {
			writeFail(w, 404, "not_found", "no route for /v1/whoami")
			return
		}
		writeFail(w, 400, "bad_request", "project is required")
	})
	if who, err := older.CheckAuth(t.Context()); err != nil || who != nil {
		t.Fatalf("older worker: %+v, %v", who, err)
	}

	bad := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFail(w, 401, "unauthorized", "invalid bearer token")
	})
	if _, err := bad.CheckAuth(t.Context()); !IsCode(err, CodeUnauthorized) {
		t.Fatalf("bad key: %v", err)
	}
}

func TestAgentKeys(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/keys":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["agent"] != "ci-bot" || in["note"] != "nightly" || in["kind"] != KindHuman {
				t.Errorf("body = %v", in)
			}
			writeOK(w, 201, map[string]any{"key": "wgk_abc", "info": map[string]any{"id": "k0123456789", "agent": "ci-bot", "kind": "human"}})
		case "GET /v1/keys":
			if r.URL.Query().Get("all") != "1" {
				t.Errorf("all = %q", r.URL.RawQuery)
			}
			writeOK(w, 200, map[string]any{"keys": []any{map[string]any{"id": "k0123456789", "agent": "ci-bot"}}})
		case "DELETE /v1/keys/k0123456789":
			writeOK(w, 200, map[string]any{"id": "k0123456789", "agent": "ci-bot", "revoked_at": "2026-10-13T00:00:00Z"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	nk, err := c.CreateAgentKey(t.Context(), "ci-bot", "nightly", KindHuman)
	if err != nil || nk.Key != "wgk_abc" || nk.Info.ID != "k0123456789" || nk.Info.Kind != KindHuman || !IsAgentKey(nk.Key) {
		t.Fatalf("create = %+v, %v", nk, err)
	}
	ks, err := c.ListAgentKeys(t.Context(), true)
	if err != nil || len(ks) != 1 || ks[0].Agent != "ci-bot" {
		t.Fatalf("list = %+v, %v", ks, err)
	}
	k, err := c.RevokeAgentKey(t.Context(), "k0123456789")
	if err != nil || k.RevokedAt == nil {
		t.Fatalf("revoke = %+v, %v", k, err)
	}
}

func TestNoURLIsNotConfigured(t *testing.T) {
	_, err := New("", "k").ListIntents(t.Context(), "demo", "")
	if !IsCode(err, CodeNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestCredentialsRoundTripAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv(EnvConfigDir, dir)
	t.Setenv(EnvURL, "")
	t.Setenv(EnvKey, "")
	t.Setenv(EnvAgent, "")
	p, err := SaveCredentials(Credentials{URL: "https://wayglass.example", Key: "s3cret", Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	dst, _ := os.Stat(dir)
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", dst.Mode().Perm())
	}
	c, err := LoadCredentials()
	if err != nil || c.URL != "https://wayglass.example" || c.Key != "s3cret" || c.Agent != "codex" {
		t.Fatalf("load = %+v, %v", c, err)
	}
	t.Setenv(EnvURL, "http://localhost:8787/")
	t.Setenv(EnvKey, "envkey")
	c, _ = LoadCredentials()
	if c.URL != "http://localhost:8787" || c.Key != "envkey" {
		t.Fatalf("env override = %+v", c)
	}
	if b, _ := json.Marshal(c); strings.Contains(string(b), "envkey") {
		t.Fatalf("key leaks into JSON: %s", b)
	}
}

func TestValidateURL(t *testing.T) {
	for in, ok := range map[string]bool{
		"https://wayglass.dev":       true,
		"http://localhost:8787":      true,
		"http://127.0.0.1:8787":      true,
		"http://wayglass.dev":        false,
		"ftp://wayglass.dev":         false,
		"wayglass.dev":               false,
		"https://":                   false,
		"https://w.workers.dev/base": true,
	} {
		if err := ValidateURL(in); (err == nil) != ok {
			t.Errorf("ValidateURL(%q) = %v, want ok=%v", in, err, ok)
		}
	}
}
