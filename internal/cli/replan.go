package cli

import (
	"context"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/intent"
)

// replanSide is what intent.SuggestReplan needs from a Worker intent.
func replanSide(i api.Intent) intent.Side {
	return intent.Side{ID: i.ID, Status: i.Status, CreatedAt: parseWorkerTime(i.CreatedAt), Declared: i.DeclaredPaths, Actual: i.ActualPaths}
}

// overlapOut is one overlap in push --json: the Worker's report plus a
// suggested re-plan computed here from both intents' paths and statuses.
// replan is absent only when the other intent could not be read.
type overlapOut struct {
	api.Overlap
	Replan *intent.Replan `json:"replan,omitempty"`
}

// withReplans adds a suggested re-plan to each overlap the Worker reported
// for mine. It reads the project's intents once, and only when there is an
// overlap; if that read fails, the overlaps go out without a re-plan.
func withReplans(ctx context.Context, c *api.Client, project string, mine api.Intent, overlaps []api.Overlap) ([]overlapOut, error) {
	out := make([]overlapOut, len(overlaps))
	for i, ov := range overlaps {
		out[i].Overlap = ov
	}
	if len(overlaps) == 0 {
		return out, nil
	}
	all, err := c.ListIntents(ctx, project, "")
	if err != nil {
		return out, err
	}
	byID := map[string]api.Intent{}
	for _, it := range all {
		byID[it.ID] = it
	}
	for i, ov := range overlaps {
		other, ok := byID[ov.IntentID]
		if !ok {
			continue
		}
		if r, ok := intent.SuggestReplan(replanSide(mine), replanSide(other)); ok {
			out[i].Replan = &r
		}
	}
	return out, nil
}
