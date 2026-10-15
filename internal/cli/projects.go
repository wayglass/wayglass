package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
)

// ProjectRow is one project in `wayglass projects --json`: the directory
// row plus the command that checks it out.
type ProjectRow struct {
	api.ProjectEntry
	// Clone is the command that checks the trunk out with a short-lived
	// read token (`wayglass clone <name>`).
	Clone string `json:"clone"`
}

// ProjectsData is the --json payload of `wayglass projects`.
type ProjectsData struct {
	Projects []ProjectRow `json:"projects"`
	// Q is the search the Worker applied, "" for none.
	Q string `json:"q"`
	// Count is len(Projects), for a quick read.
	Count int `json:"count"`
}

// projectsLong explains the listing once; `projects` and `project list` share it.
const projectsLong = "Lists every project this key can read on the Worker, across its Artifacts\n" +
	"namespaces (US, EU): jurisdiction, intents open, changes waiting for a person,\n" +
	"and the last activity. Any logged-in key may list; the rows hold the same\n" +
	"metadata the intents and audit routes return, and no token. The Worker\n" +
	"matches --search as a case-insensitive part of the name.\n\n" +
	"In --json each row carries its trunk remote, default branch, created_at,\n" +
	"last_activity, counts (intents, open, agents, pending_human, landed) and the\n" +
	"clone command. A project whose state is not where its jurisdiction says is\n" +
	"listed with residency: residency_pending (see project migrate-residency)."

func newProjectsCmd(a *app) *cobra.Command {
	var search string
	cmd := &cobra.Command{
		Use:   "projects",
		Short: "List the projects this key can read, with a search",
		Long:  projectsLong,
		Example: "  wayglass projects\n  wayglass projects --search eu-\n" +
			"  wayglass projects --json | jq -r '.data.projects[].clone'",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProjects(a, cmd, search)
		},
	}
	cmd.Flags().StringVarP(&search, "search", "s", "", "show projects whose name contains this (matched by the Worker, case-insensitive)")
	return cmd
}

// newProjectListCmd is `wayglass project list`, the same listing under the
// project group so it is found next to delete and migrate-residency.
func newProjectListCmd(a *app) *cobra.Command {
	var search string
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List the projects this key can read (same as 'wayglass projects')",
		Long:    projectsLong,
		Example: "  wayglass project list\n  wayglass project list --search demo --json",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runProjects(a, cmd, search)
		},
	}
	cmd.Flags().StringVarP(&search, "search", "s", "", "show projects whose name contains this (matched by the Worker, case-insensitive)")
	return cmd
}

func runProjects(a *app, cmd *cobra.Command, search string) error {
	search = strings.TrimSpace(search)
	if len(search) > 63 {
		return UsageError("--search is longer than a project name (63 characters)")
	}
	o := a.output(cmd)
	c, err := a.client()
	if err != nil {
		return err
	}
	entries, err := c.AllProjects(cmd.Context(), search)
	if err != nil {
		return err
	}
	data := ProjectsData{Projects: make([]ProjectRow, 0, len(entries)), Q: search, Count: len(entries)}
	for _, e := range entries {
		data.Projects = append(data.Projects, ProjectRow{ProjectEntry: e, Clone: api.CloneCommand(e.Name)})
	}
	return o.Emit(data, func(out io.Writer) error {
		return renderProjects(out, cliStyles(o), data, time.Now())
	})
}

// renderProjects prints the table people read: name, jurisdiction, open
// intents, changes waiting for a person, last activity.
func renderProjects(out io.Writer, st styles, data ProjectsData, now time.Time) error {
	if len(data.Projects) == 0 {
		msg := "No projects on this Worker yet; 'wayglass init' creates one."
		if data.Q != "" {
			msg = fmt.Sprintf("No project name contains %q.", data.Q)
		}
		_, err := lipgloss.Fprintln(out, st.muted.Render(msg))
		return err
	}
	nameW := len("PROJECT")
	for _, p := range data.Projects {
		nameW = max(nameW, len(p.Name))
	}
	_, _ = lipgloss.Fprintf(out, "%s\n", st.muted.Render(fmt.Sprintf("%-*s  %-4s  %5s  %9s  %s", nameW, "PROJECT", "JUR", "OPEN", "NEEDS YOU", "LAST ACTIVITY")))
	for _, p := range data.Projects {
		if p.Counts == nil {
			// Listed, not served: the admin must move its state first.
			_, _ = lipgloss.Fprintf(out, "%-*s  %-4s  %s\n", nameW, p.Name, strings.ToUpper(p.Jurisdiction), st.attention.Render(p.Residency))
			continue
		}
		needs := fmt.Sprintf("%9d", p.Counts.PendingHuman)
		if p.Counts.PendingHuman > 0 {
			needs = st.attention.Render(fmt.Sprintf("%9s", fmt.Sprintf("▲ %d", p.Counts.PendingHuman)))
		}
		last := "never"
		if p.LastActivity != nil {
			last = sinceText(parseWorkerTime(*p.LastActivity), now)
		}
		_, _ = lipgloss.Fprintf(out, "%-*s  %-4s  %5d  %s  %s\n", nameW, p.Name, strings.ToUpper(p.Jurisdiction), p.Counts.Open, needs, st.muted.Render(last))
	}
	_, err := lipgloss.Fprintln(out, st.muted.Render(fmt.Sprintf("%d project(s). Check one out with 'wayglass clone <name>'; open it with 'wayglass watch --pick'.", len(data.Projects))))
	return err
}

// sinceText is a short "3m ago" for a table cell; "never" for the zero time.
func sinceText(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Local().Format("2006-01-02")
}
