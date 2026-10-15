package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/tui"
)

// fakeDirectoryPage is how many projects one fake directory page holds:
// small, so a listing test walks more than one page.
const fakeDirectoryPage = 2

// listProjects mirrors GET /v1/projects in worker/src/browse.ts: any
// logged-in key, rows in directory order, ?q= as a case-insensitive part of
// the name, and a cursor that walks pages (here a plain offset).
func (f *fakeWorker) listProjects(w http.ResponseWriter, r *http.Request) {
	if _, ok := f.identity(r); !ok {
		fail(w, 401, "unauthorized", "invalid bearer token")
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if len(q) > 63 {
		fail(w, 400, "bad_request", "q is longer than a project name")
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.projects))
	for name := range f.projects {
		if q == "" || strings.Contains(name, q) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	rows := []api.ProjectEntry{}
	for _, name := range names[min(offset, len(names)):min(offset+fakeDirectoryPage, len(names))] {
		p := f.projects[name]
		row := api.ProjectEntry{
			Name: name, Jurisdiction: p.info.Jurisdiction, TrunkRepo: p.info.TrunkRepo, TrunkRemote: p.info.TrunkRemote,
			DefaultBranch: "main", CreatedAt: p.info.CreatedAt, Residency: "in_place", Counts: &api.ProjectCounts{},
		}
		agents := map[string]bool{}
		for _, it := range p.intents {
			agents[it.Agent] = true
			row.Counts.Intents++
			switch it.Status {
			case "open", "proposed":
				row.Counts.Open++
			case "landed":
				row.Counts.Landed++
			}
			if it.LastPushAt != nil && (row.LastPushAt == nil || *it.LastPushAt > *row.LastPushAt) {
				row.LastPushAt = it.LastPushAt
			}
		}
		row.Counts.Agents = len(agents)
		for _, d := range p.decisions {
			if d.Status == "pending_human" || d.Status == "quarantined" {
				row.Counts.PendingHuman++
			}
		}
		if n := len(p.audit); n > 0 {
			at := p.audit[n-1].At
			row.LastActivity = &at
		}
		rows = append(rows, row)
	}
	var next *string
	if offset+fakeDirectoryPage < len(names) {
		s := strconv.Itoa(offset + fakeDirectoryPage)
		next = &s
	}
	var qOut *string
	if q != "" {
		qOut = &q
	}
	ok(w, 200, api.ProjectsPage{Projects: rows, Q: qOut, Next: next})
}

func TestProjectsListsAcrossPagesWithSearch(t *testing.T) {
	e := newFlow(t)
	a := e.repo
	e.do(a, ExitOK, "init", "--project", "demo-test-1")
	decl := e.do(a, ExitOK, "intent", "declare", "--goal", "harden login", "--reason", "brute force seen", "--path", "web/**")
	id := get(decl, "intent", "id").(string)
	for _, name := range []string{"demo-test-2", "eu-demo-test-3"} {
		dir := filepath.Join(t.TempDir(), name)
		e.git("", "init", "--quiet", "--initial-branch=main", dir)
		e.write(dir, "README.md", "# "+name+"\n")
		e.git(dir, "add", "-A")
		e.git(dir, "commit", "--quiet", "-m", "initial")
		args := []string{"init", "--project", name}
		if strings.HasPrefix(name, "eu-") {
			args = append(args, "--jurisdiction", "eu")
		}
		e.do(dir, ExitOK, args...)
	}
	e.worker.mu.Lock()
	p := e.worker.projects["demo-test-1"]
	p.decisions = append(p.decisions, &api.Decision{ID: "d01", IntentID: id, Action: "human_approve", Status: "pending_human", Reason: "auth needs a person"})
	e.worker.mu.Unlock()

	got := e.do(a, ExitOK, "projects")
	rows := get(got, "projects").([]any)
	if len(rows) != 3 || get(got, "count") != float64(3) || get(got, "q") != "" {
		t.Fatalf("projects = %v", got)
	}
	first := rows[0].(map[string]any)
	if first["name"] != "demo-test-1" || first["clone"] != "wayglass clone demo-test-1" || first["default_branch"] != "main" || first["residency"] != "in_place" {
		t.Fatalf("first row = %v", first)
	}
	if first["trunk_remote"] == "" || first["last_activity"] == nil || first["created_at"] == "" {
		t.Fatalf("first row lacks browser fields: %v", first)
	}
	if c := first["counts"].(map[string]any); c["open"] != float64(1) || c["pending_human"] != float64(1) || c["agents"] != float64(1) || c["landed"] != float64(0) {
		t.Fatalf("counts = %v", c)
	}
	if third := rows[2].(map[string]any); third["name"] != "eu-demo-test-3" || third["jurisdiction"] != "eu" {
		t.Fatalf("third row = %v", third)
	}

	found := e.do(a, ExitOK, "projects", "--search", "TEST-2")
	if rows := get(found, "projects").([]any); len(rows) != 1 || rows[0].(map[string]any)["name"] != "demo-test-2" || get(found, "q") != "TEST-2" {
		t.Fatalf("search = %v", found)
	}
	alias := e.do(a, ExitOK, "project", "list", "--search", "eu-")
	if rows := get(alias, "projects").([]any); len(rows) != 1 || rows[0].(map[string]any)["name"] != "eu-demo-test-3" {
		t.Fatalf("project list = %v", alias)
	}
	if none := e.do(a, ExitOK, "projects", "--search", "nothing-here"); len(get(none, "projects").([]any)) != 0 {
		t.Fatalf("no match = %v", none)
	}
	if got := e.do(a, ExitUsage, "projects", "--search", strings.Repeat("a", 64)); get(got, "error", "code") != CodeUsage {
		t.Fatalf("long search = %v", got)
	}

	// The table people read: a header, one line per project, the needs-you
	// mark on the project with a change waiting, and how to open one.
	t.Chdir(a)
	exit, stdout, stderr := run(t, "projects")
	if exit != ExitOK || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", exit, stderr)
	}
	for _, want := range []string{"PROJECT", "NEEDS YOU", "demo-test-1", "▲ 1", "eu-demo-test-3", "EU", "3 project(s)", "wayglass clone <name>"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "art_") {
		t.Fatal("a listing must never print a token")
	}
}

func TestProjectsEmptyAndRefused(t *testing.T) {
	e := newFlow(t)
	t.Chdir(e.repo)
	exit, stdout, _ := run(t, "projects")
	if exit != ExitOK || !strings.Contains(stdout, "No projects") {
		t.Fatalf("empty listing: exit=%d %q", exit, stdout)
	}
	t.Setenv("WAYGLASS_KEY", "not-a-key")
	if got := e.do(e.repo, ExitError, "projects"); get(got, "error", "code") != api.CodeUnauthorized {
		t.Fatalf("bad key = %v", got)
	}
}

func TestSinceText(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		ago  time.Duration
		want string
	}{
		{0, "just now"}, {5 * time.Minute, "5m ago"}, {3 * time.Hour, "3h ago"}, {2 * 24 * time.Hour, "2d ago"}, {30 * 24 * time.Hour, "2026-09-14"},
	}
	for _, tc := range tests {
		if got := sinceText(now.Add(-tc.ago), now); got != tc.want {
			t.Errorf("sinceText(-%s) = %q, want %q", tc.ago, got, tc.want)
		}
	}
	if got := sinceText(time.Time{}, now); got != "never" {
		t.Errorf("zero time = %q", got)
	}
}

func TestLiveSourceListsAndSwitchesProjects(t *testing.T) {
	e := newFlow(t)
	e.do(e.repo, ExitOK, "init", "--project", "demo-test-1")
	e.do(e.repo, ExitOK, "intent", "declare", "--goal", "g", "--reason", "try the flow", "--path", "docs/**")
	c := api.New(os.Getenv("WAYGLASS_URL"), e.worker.key)
	src := &liveSource{c: c, project: "demo-test-1", jurisdiction: "us"}
	items, err := src.Projects(t.Context())
	if err != nil || len(items) != 1 || items[0].Name != "demo-test-1" || items[0].Open != 1 || items[0].LastActivity.IsZero() {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	next := src.Switch(tui.ProjectItem{Name: "demo-test-2", Jurisdiction: "eu"})
	if next.Project() != "demo-test-2" || next.(*liveSource).jurisdiction != "eu" || next.(*liveSource).c != c {
		t.Fatalf("switched source = %+v", next)
	}
	if _, ok := next.(tui.ProjectSwitcher); !ok {
		t.Fatal("the switched source must still offer the picker")
	}
}
