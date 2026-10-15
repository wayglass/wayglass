package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
)

// deleteProject mirrors DELETE /v1/projects/:name in worker/src/index.ts:
// admin only, throwaway names only, confirm must repeat the name, repos
// already gone are fine, and a second delete answers existed: false.
func (f *fakeWorker) deleteProject(w http.ResponseWriter, r *http.Request) {
	if id, _ := f.identity(r); !id.admin {
		fail(w, 403, "forbidden", "only the admin key can delete projects")
		return
	}
	name := r.PathValue("name")
	if !api.IsDeletableProject(name) {
		fail(w, 403, "forbidden", "only throwaway projects can be deleted")
		return
	}
	if body := decode(r); body["confirm"] != name {
		fail(w, 400, "bad_request", "confirm must repeat the project name")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[name]
	if p == nil {
		ok(w, 200, api.ProjectDeletion{Project: name, ReposDeleted: []string{}, ReposAlreadyGone: []string{}})
		return
	}
	d := api.ProjectDeletion{Project: name, Existed: true, Namespace: "wayglass", Jurisdiction: p.info.Jurisdiction, StateWiped: true, ReposDeleted: []string{}, ReposAlreadyGone: []string{}}
	remotes := []string{p.info.TrunkRemote}
	for _, it := range p.intents {
		remotes = append(remotes, it.ForkRemote)
	}
	for _, remote := range remotes {
		if _, err := os.Stat(remote); err != nil {
			d.ReposAlreadyGone = append(d.ReposAlreadyGone, filepath.Base(remote))
			continue
		}
		if err := os.RemoveAll(remote); err != nil {
			fail(w, 502, "upstream_error", err.Error())
			return
		}
		d.ReposDeleted = append(d.ReposDeleted, filepath.Base(remote))
	}
	delete(f.projects, name)
	ok(w, 200, d)
}

// migrateResidency mirrors POST /v1/projects/:name/migrate-residency
// (worker/src/residency.ts) as far as the CLI can see: admin only, a project
// in the main jurisdiction has nothing to move, an eu project is moved once
// and reported migrated: false afterwards.
func (f *fakeWorker) migrateResidency(w http.ResponseWriter, r *http.Request) {
	if id, _ := f.identity(r); !id.admin {
		fail(w, 403, "forbidden", "only the admin key can migrate a project's residency")
		return
	}
	name := r.PathValue("name")
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.projects[name]
	if p == nil || p.info.Jurisdiction != "eu" {
		fail(w, 503, api.CodeResidencyPending, "project "+name+" has no pinned jurisdiction; nothing to migrate")
		return
	}
	if f.migrated == nil {
		f.migrated = map[string]bool{}
	}
	m := api.ResidencyMigration{Project: name, Jurisdiction: "eu", Migrated: !f.migrated[name], SourceTombstoned: true, Digest: strings.Repeat("ab", 32),
		Counts: map[string]int{"meta": 5, "intents": len(p.intents), "audit": len(p.audit)}}
	if m.Migrated {
		p.log("project.residency_migrated", "admin", "", map[string]any{"from": "unpinned", "to": "eu"})
		m.Counts["audit"]++
	}
	f.migrated[name] = true
	ok(w, 200, m)
}

func TestProjectMigrateResidency(t *testing.T) {
	e := newFlow(t)
	a := e.repo
	e.do(a, ExitOK, "init", "--project", "eu-demo-test-1", "--jurisdiction", "eu")

	got := e.do(a, ExitOK, "project", "migrate-residency", "eu-demo-test-1")
	if get(got, "migrated") != true || get(got, "jurisdiction") != "eu" || get(got, "source_tombstoned") != true {
		t.Fatalf("migrate = %v", got)
	}
	again := e.do(a, ExitOK, "project", "migrate-residency", "eu-demo-test-1")
	if get(again, "migrated") != false {
		t.Fatalf("second migrate = %v", again)
	}
	if got := e.do(a, ExitError, "project", "migrate-residency", "no-such-project"); get(got, "error", "code") != api.CodeResidencyPending {
		t.Fatalf("unknown project = %v", got)
	}
	if got := e.do(a, ExitUsage, "project", "migrate-residency"); get(got, "error", "code") != CodeUsage {
		t.Fatalf("no name = %v", got)
	}
}

func TestProjectDelete(t *testing.T) {
	e := newFlow(t)
	a := e.repo
	e.do(a, ExitOK, "init", "--project", "demo-test-1")
	decl := e.do(a, ExitOK, "intent", "declare", "--goal", "g", "--reason", "try the flow", "--path", "docs/**")
	fork := get(decl, "fork", "remote").(string)
	if _, err := os.Stat(fork); err != nil {
		t.Fatalf("fork repo missing before delete: %v", err)
	}

	tests := []struct {
		name string
		args []string
		exit int
		code string
	}{
		{"not throwaway", []string{"project", "delete", "demo-1", "--confirm", "demo-1"}, ExitUsage, CodeUsage},
		{"no confirm without a terminal", []string{"project", "delete", "demo-test-1"}, ExitUsage, CodeUsage},
		{"confirm mismatch", []string{"project", "delete", "demo-test-1", "--confirm", "demo-test-2"}, ExitUsage, CodeUsage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.do(a, tc.exit, tc.args...); get(got, "error", "code") != tc.code {
				t.Fatalf("%v = %v", tc.args, got)
			}
		})
	}
	if _, err := os.Stat(fork); err != nil {
		t.Fatalf("a refused delete removed the fork: %v", err)
	}

	got := e.do(a, ExitOK, "project", "delete", "demo-test-1", "--confirm", "demo-test-1")
	if get(got, "existed") != true || get(got, "state_wiped") != true || len(get(got, "repos_deleted").([]any)) != 2 {
		t.Fatalf("delete = %v", got)
	}
	if _, err := os.Stat(fork); !os.IsNotExist(err) {
		t.Fatalf("fork repo still there: %v", err)
	}
	if got := e.do(a, ExitError, "intent", "list"); get(got, "error", "code") != api.CodeNotFound {
		t.Fatalf("intent list after delete = %v", got)
	}
	again := e.do(a, ExitOK, "project", "delete", "demo-test-1", "--confirm", "demo-test-1")
	if get(again, "existed") != false || len(get(again, "repos_deleted").([]any)) != 0 {
		t.Fatalf("second delete = %v", again)
	}
}
