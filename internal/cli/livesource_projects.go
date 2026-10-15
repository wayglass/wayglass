package cli

import (
	"context"

	"github.com/wayglass/wayglass/internal/tui"
)

var _ tui.ProjectSwitcher = (*liveSource)(nil)

// Projects lists the projects this key can read (GET /v1/projects), in
// directory order, for the TUI picker.
func (s *liveSource) Projects(ctx context.Context) ([]tui.ProjectItem, error) {
	entries, err := s.c.AllProjects(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]tui.ProjectItem, 0, len(entries))
	for _, e := range entries {
		it := tui.ProjectItem{Name: e.Name, Jurisdiction: e.Jurisdiction}
		if e.LastActivity != nil {
			it.LastActivity = parseWorkerTime(*e.LastActivity)
		}
		if e.Counts == nil {
			// Listed, not served (worker/src/residency.ts): say why.
			it.Residency = e.Residency
		} else {
			it.Open, it.PendingHuman, it.Landed = e.Counts.Open, e.Counts.PendingHuman, e.Counts.Landed
		}
		out = append(out, it)
	}
	return out, nil
}

// Switch points a fresh source at the chosen project with the same client,
// so the key, identity and family carry over; warnings start empty.
func (s *liveSource) Switch(it tui.ProjectItem) tui.DataSource {
	return &liveSource{c: s.c, project: it.Name, jurisdiction: it.Jurisdiction}
}
