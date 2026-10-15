package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
)

func (n *fakeNative) CopyText(text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.copied = append(n.copied, text)
	return nil
}

// projects mirrors GET /v1/projects (worker/src/browse.ts) for the two
// projects the fake knows: the watched one and an EU one with a change
// waiting for a person; ?q= filters by name.
func (f *fakeWorker) projects(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	last := "2026-10-13T12:01:00Z"
	rows := []api.ProjectEntry{
		{Name: f.project, Jurisdiction: "us", TrunkRepo: f.project, TrunkRemote: "https://fake.invalid/git/" + f.project + ".git", DefaultBranch: "main", Residency: "in_place", LastActivity: &last,
			Counts: &api.ProjectCounts{Intents: 4, Open: 3, Agents: 4, PendingHuman: 2, Landed: 1}},
		{Name: "eu-shop", Jurisdiction: "eu", TrunkRepo: "eu-shop", DefaultBranch: "main", Residency: "in_place", Counts: &api.ProjectCounts{Intents: 1, Open: 1, Agents: 1, PendingHuman: 1}},
		{Name: "eu-legacy", Jurisdiction: "eu", Residency: "residency_pending"},
	}
	out := []api.ProjectEntry{}
	for _, row := range rows {
		if q == "" || strings.Contains(row.Name, q) {
			out = append(out, row)
		}
	}
	writeOK(w, api.ProjectsPage{Projects: out})
}

func TestProjectsListsSearchesAndCopiesTheCloneCommand(t *testing.T) {
	f, srv := newFake(t)
	n := &fakeNative{}
	a, _ := newApp(t, srv, "john", n)

	v, err := a.Projects("")
	if err != nil {
		t.Fatal(err)
	}
	if v.Count != 3 || len(v.Projects) != 3 || v.Q != "" {
		t.Fatalf("projects = %+v", v)
	}
	demo := v.Projects[0]
	if !demo.Current || !demo.Listed || demo.Open != 3 || demo.PendingHuman != 2 || demo.Landed != 1 || demo.Agents != 4 || !demo.Served {
		t.Fatalf("demo row = %+v", demo)
	}
	if demo.Clone != "wayglass clone demo" || demo.LastActivity != "2026-10-13T12:01:00Z" {
		t.Fatalf("demo row = %+v", demo)
	}
	if legacy := v.Projects[2]; legacy.Served || legacy.Residency != "residency_pending" || legacy.Current || legacy.Listed {
		t.Fatalf("legacy row = %+v", legacy)
	}
	if strings.Contains(strings.Join([]string{demo.Clone, demo.Name}, " "), "fake.invalid") {
		t.Fatal("the window must not hand out a remote")
	}

	eu, err := a.Projects(" EU-S ")
	if err != nil || len(eu.Projects) != 1 || eu.Projects[0].Name != "eu-shop" || eu.Q != "EU-S" {
		t.Fatalf("search = %+v err=%v", eu, err)
	}
	if _, err := a.Projects(strings.Repeat("a", 64)); err == nil {
		t.Fatal("an over-long search must be refused before any request")
	}
	if f.hit("GET /v1/projects") != 2 {
		t.Fatalf("directory requests = %d, want 2", f.hit("GET /v1/projects"))
	}

	cmd, err := a.CopyCloneCommand("eu-shop")
	if err != nil || cmd != "wayglass clone eu-shop" {
		t.Fatalf("copy = %q, %v", cmd, err)
	}
	n.mu.Lock()
	copied := append([]string(nil), n.copied...)
	n.mu.Unlock()
	if len(copied) != 1 || copied[0] != "wayglass clone eu-shop" {
		t.Fatalf("clipboard = %v", copied)
	}
	if _, err := a.CopyCloneCommand("Not A Name; rm -rf"); err == nil {
		t.Fatal("a bad name must not reach the clipboard")
	}
}

func TestProjectsNeedsALogin(t *testing.T) {
	a := New(Options{Creds: api.Credentials{}})
	v, err := a.Projects("")
	if err == nil || v.Projects == nil {
		t.Fatalf("without a login: %+v, %v", v, err)
	}
}
