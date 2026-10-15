package cli

import (
	"cmp"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wayglass/wayglass/internal/api"
)

// identity of a request on the fake Worker: the bound agent for agent keys,
// "" for the admin key.
type fakeIdentity struct {
	admin bool
	key   *api.AgentKey
}

// authenticate mirrors worker/src/auth.ts: the admin key, or a live agent key.
func (f *fakeWorker) authenticate(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := f.identity(r); ok {
		return true
	}
	fail(w, 401, "unauthorized", "invalid bearer token")
	return false
}

// kind mirrors Identity.kind in worker/src/auth.ts: the admin key is a person.
func (id fakeIdentity) kind() string {
	if id.admin || (id.key != nil && id.key.Kind == api.KindHuman) {
		return api.KindHuman
	}
	return api.KindAgent
}

func (f *fakeWorker) identity(r *http.Request) (fakeIdentity, bool) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return fakeIdentity{}, false
	}
	if bearer == f.key {
		return fakeIdentity{admin: true}, true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if k := f.agentKeys[bearer]; k != nil && k.RevokedAt == nil {
		return fakeIdentity{key: k}, true
	}
	return fakeIdentity{}, false
}

func (f *fakeWorker) whoami(w http.ResponseWriter, r *http.Request) {
	if f.noWhoami {
		fail(w, 404, "not_found", "no route for /v1/whoami")
		return
	}
	id, _ := f.identity(r)
	if id.admin {
		ok(w, 200, map[string]any{"role": "admin", "agent": r.Header.Get("X-Wayglass-Agent"), "bound": false, "key_id": nil, "kind": api.KindHuman})
		return
	}
	ok(w, 200, map[string]any{"role": "agent", "agent": id.key.Agent, "bound": true, "key_id": id.key.ID, "kind": id.kind(), "source": cmp.Or(id.key.Source, "key"), "expires_at": id.key.ExpiresAt})
}

func (f *fakeWorker) adminOnly(w http.ResponseWriter, r *http.Request) bool {
	if id, _ := f.identity(r); !id.admin {
		fail(w, 403, "forbidden", "only the admin key can manage keys")
		return false
	}
	return true
}

func (f *fakeWorker) createKey(w http.ResponseWriter, r *http.Request) {
	if !f.adminOnly(w, r) {
		return
	}
	body := decode(r)
	agent, _ := body["agent"].(string)
	note, _ := body["note"].(string)
	kind, _ := body["kind"].(string)
	if kind == "" {
		kind = api.KindAgent
	}
	if kind != api.KindAgent && kind != api.KindHuman {
		fail(w, 400, "bad_request", "kind must be human or agent")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	key := fmt.Sprintf("wgk_%064x", f.seq)
	k := &api.AgentKey{ID: f.nextID("k"), Agent: agent, Kind: kind, Note: note, CreatedBy: r.Header.Get("X-Wayglass-Agent"), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	f.agentKeys[key] = k
	ok(w, 201, map[string]any{"key": key, "info": k})
}

func (f *fakeWorker) listKeys(w http.ResponseWriter, r *http.Request) {
	if !f.adminOnly(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := []*api.AgentKey{}
	for _, k := range f.agentKeys {
		if k.RevokedAt == nil || r.URL.Query().Get("all") == "1" {
			keys = append(keys, k)
		}
	}
	ok(w, 200, map[string]any{"keys": keys})
}

func (f *fakeWorker) revokeKey(w http.ResponseWriter, r *http.Request) {
	if !f.adminOnly(w, r) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.agentKeys {
		if k.ID == r.PathValue("id") {
			at := time.Now().UTC().Format(time.RFC3339)
			k.RevokedAt = &at
			ok(w, 200, k)
			return
		}
	}
	fail(w, 404, "not_found", "no key "+r.PathValue("id"))
}

func (f *fakeWorker) trunkReadToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[r.PathValue("name")]
	if p == nil {
		fail(w, 404, "not_found", "no project "+r.PathValue("name"))
		return
	}
	tok := f.mint()
	tok.Scope = "read"
	f.trunkReadTokens++
	ok(w, 201, map[string]any{"trunk": map[string]any{"name": p.info.TrunkRepo, "remote": p.info.TrunkRemote}, "token": tok})
}

// landRefusal mirrors landRefusal in worker/src/project.ts: only the intent's
// author (a bound key's agent, or the name the admin key gives) or the admin
// key may land, finish or abort a land. Call it without holding f.mu.
func (f *fakeWorker) landRefusal(r *http.Request) func(it *api.Intent) string {
	id, _ := f.identity(r)
	caller := r.Header.Get("X-Wayglass-Agent")
	if id.key != nil {
		caller = id.key.Agent
	}
	return func(it *api.Intent) string {
		if id.admin || caller == it.Agent {
			return ""
		}
		return "only " + it.Agent + " (the author of intent " + it.ID + ") or the admin key may land it; " + caller + " may not"
	}
}
