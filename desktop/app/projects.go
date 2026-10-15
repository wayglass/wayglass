package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/wayglass/wayglass/internal/api"
)

// ProjectView is one project in the window's directory: what a person
// needs to pick one, and the command that checks it out.
type ProjectView struct {
	Name         string `json:"name"`
	Jurisdiction string `json:"jurisdiction"`
	Open         int    `json:"open"`
	PendingHuman int    `json:"pending_human"`
	Landed       int    `json:"landed"`
	Agents       int    `json:"agents"`
	// LastActivity is the newest audit event, RFC 3339; "" if none.
	LastActivity string `json:"last_activity"`
	// Served is false for a project whose state is not where its
	// jurisdiction says; Residency then says why (residency_pending,
	// project_moved) and the project cannot be opened.
	Served    bool   `json:"served"`
	Residency string `json:"residency"`
	// Clone is `wayglass clone <name>`: a checkout through a short-lived
	// read token, the only way the window hands out a repo.
	Clone string `json:"clone"`
	// Current is the project the window shows; Listed, one in its list.
	Current bool `json:"current"`
	Listed  bool `json:"listed"`
}

// ProjectsView is the reply to Projects.
type ProjectsView struct {
	Projects []ProjectView `json:"projects"`
	Q        string        `json:"q"`
	Count    int           `json:"count"`
}

// Projects lists every project the login can read on the Worker, in
// directory order, filtered by q (a case-insensitive part of the name,
// matched by the Worker). Any logged-in key may list.
func (a *App) Projects(q string) (ProjectsView, error) {
	q = strings.TrimSpace(q)
	if len(q) > 63 {
		return ProjectsView{Projects: []ProjectView{}}, errors.New("the search is longer than a project name (63 characters)")
	}
	c := a.clientOrNil()
	if c == nil {
		return ProjectsView{Projects: []ProjectView{}}, errors.New(a.Session().Problem)
	}
	ctx, cancel := context.WithTimeout(a.context(), requestTimeout)
	defer cancel()
	entries, err := c.AllProjects(ctx, q)
	if err != nil {
		return ProjectsView{Projects: []ProjectView{}}, fmt.Errorf("listing projects: %s", describe(err))
	}
	sess := a.Session()
	out := ProjectsView{Projects: make([]ProjectView, 0, len(entries)), Q: q, Count: len(entries)}
	for _, e := range entries {
		out.Projects = append(out.Projects, projectView(e, sess))
	}
	return out, nil
}

func projectView(e api.ProjectEntry, sess Session) ProjectView {
	v := ProjectView{
		Name: e.Name, Jurisdiction: e.Jurisdiction, Clone: api.CloneCommand(e.Name),
		Served: e.Counts != nil, Residency: e.Residency,
		Current: e.Name == sess.Current, Listed: slices.Contains(sess.Projects, e.Name),
	}
	if e.LastActivity != nil {
		if t, err := time.Parse(time.RFC3339Nano, *e.LastActivity); err == nil {
			v.LastActivity = t.UTC().Format(time.RFC3339)
		}
	}
	if e.Counts != nil {
		v.Open, v.PendingHuman, v.Landed, v.Agents = e.Counts.Open, e.Counts.PendingHuman, e.Counts.Landed, e.Counts.Agents
	}
	return v
}

// CopyCloneCommand puts `wayglass clone <name>` on the clipboard and returns
// it. The name is checked here as well: the page may be fed by the Worker.
func (a *App) CopyCloneCommand(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !validProject(name) {
		return "", fmt.Errorf("%q is not a project name", name)
	}
	cmd := api.CloneCommand(name)
	if a.native == nil {
		return cmd, errors.New("no window to copy into")
	}
	if err := a.native.CopyText(cmd); err != nil {
		return cmd, fmt.Errorf("clipboard: %w", err)
	}
	return cmd, nil
}
