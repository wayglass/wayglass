package intent

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Re-plan actions. A re-plan is a suggestion computed on the client from the
// two intents' paths and statuses; nothing about either intent changes until
// an agent or a person acts on it.
const (
	// ReplanLandFirst: First lands, then Then lands after it (land merges the
	// new trunk in; it never rebases).
	ReplanLandFirst = "land_first"
	// ReplanStack: both edited the same files; Then continues from First's
	// head instead of from trunk (wayglass intent restack, see Command).
	ReplanStack = "stack"
	// ReplanSplit: only broad declarations collide; both narrow their
	// declared paths so the work can run in parallel.
	ReplanSplit = "split"
)

// Side is what SuggestReplan needs to know about one intent.
type Side struct {
	ID        string
	Status    string // open, proposed, ...
	CreatedAt time.Time
	Declared  []string
	Actual    []string
}

// Replan is a suggested way to resolve an overlap without a merge conflict.
type Replan struct {
	Action  string   `json:"action"`  // land_first, stack or split
	First   string   `json:"first"`   // intent id that goes first (or keeps its paths)
	Then    string   `json:"then"`    // intent id that waits, lands after or narrows
	Paths   []string `json:"paths"`   // the contested paths
	Summary string   `json:"summary"` // one sentence for people
	// Command is the wayglass command that carries the re-plan out, when
	// one does (stack: intent restack); the reason is left for the agent.
	Command   string `json:"command,omitempty"`
	Suggested bool   `json:"suggested"` // always true: a suggestion, not something done
}

// SuggestReplan picks a re-plan for two overlapping intents. The rules, in
// order:
//
//  1. One side is already proposed: it lands first, the other lands after it.
//  2. Both pushed edits to a contested file: stack the newer on the older.
//  3. Only one side pushed edits to a contested file: it lands first.
//  4. Nothing pushed yet, and both declared the same literal file: the older
//     intent lands first.
//  5. Nothing pushed yet, and only globs collide: split the glob, each side
//     declares the narrower paths it needs.
//
// It returns false when the two intents do not overlap.
func SuggestReplan(a, b Side) (Replan, bool) {
	pairs := Intersect(Paths(a.Declared, a.Actual), Paths(b.Declared, b.Actual))
	if len(pairs) == 0 {
		return Replan{}, false
	}
	contested := contestedPaths(pairs)
	older, newer := a, b
	if b.CreatedAt.Before(a.CreatedAt) || (b.CreatedAt.Equal(a.CreatedAt) && b.ID < a.ID) {
		older, newer = b, a
	}
	r := Replan{Paths: contested, Suggested: true}
	set := func(action string, first, then Side, summary string) Replan {
		r.Action, r.First, r.Then, r.Summary = action, first.ID, then.ID, summary
		return r
	}
	touchedA := touches(a.Actual, contested)
	touchedB := touches(b.Actual, contested)
	what := listPaths(contested)

	switch pa, pb := a.Status == "proposed", b.Status == "proposed"; {
	case pa && pb:
		return set(ReplanLandFirst, older, newer, fmt.Sprintf("land %s before %s; %s lands after it (both proposed, both touch %s)", older.ID, newer.ID, newer.ID, what)), true
	case pa:
		return set(ReplanLandFirst, a, b, fmt.Sprintf("land %s before %s; %s is proposed, %s lands after it", a.ID, b.ID, a.ID, b.ID)), true
	case pb:
		return set(ReplanLandFirst, b, a, fmt.Sprintf("land %s before %s; %s is proposed, %s lands after it", b.ID, a.ID, b.ID, a.ID)), true
	}
	switch {
	case touchedA && touchedB:
		r := set(ReplanStack, older, newer, fmt.Sprintf("stack %s on %s; both edited %s", newer.ID, older.ID, what))
		r.Command = RestackCommand(newer.ID, older.ID)
		r.Summary += " (" + r.Command + ")"
		return r, true
	case touchedA:
		return set(ReplanLandFirst, a, b, fmt.Sprintf("land %s before %s; %s already edited %s", a.ID, b.ID, a.ID, what)), true
	case touchedB:
		return set(ReplanLandFirst, b, a, fmt.Sprintf("land %s before %s; %s already edited %s", b.ID, a.ID, b.ID, what)), true
	}
	if slices.ContainsFunc(pairs, func(p PathPair) bool { return !isGlob(p.Mine) && !isGlob(p.Theirs) }) {
		return set(ReplanLandFirst, older, newer, fmt.Sprintf("land %s before %s; both plan to edit %s", older.ID, newer.ID, what)), true
	}
	glob := broadest(pairs)
	return set(ReplanSplit, older, newer, fmt.Sprintf("split %s; %s and %s each declare the narrower paths they need", glob, older.ID, newer.ID)), true
}

// RestackCommand is the command that stacks then on first. The reason is a
// placeholder the Worker refuses, so an agent has to say why.
func RestackCommand(then, first string) string {
	return fmt.Sprintf(`wayglass intent restack %s --on %s --reason "<why %s builds on %s>"`, then, first, then, first)
}

// contestedPaths names what both sides want: the literal path where one side
// is literal, else both globs.
func contestedPaths(pairs []PathPair) []string {
	var out []string
	for _, p := range pairs {
		switch gm, gt := isGlob(p.Mine), isGlob(p.Theirs); {
		case !gm:
			out = append(out, p.Mine)
		case !gt:
			out = append(out, p.Theirs)
		default:
			out = append(out, p.Mine, p.Theirs)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	// A glob adds nothing when a literal path it covers is already listed.
	return slices.DeleteFunc(out, func(g string) bool {
		return isGlob(g) && slices.ContainsFunc(out, func(p string) bool { return !isGlob(p) && match(Normalize(g), p) })
	})
}

// listPaths names up to two paths and counts the rest, for a summary.
func listPaths(ps []string) string {
	if len(ps) <= 2 {
		return strings.Join(ps, ", ")
	}
	return fmt.Sprintf("%s, %s and %d more", ps[0], ps[1], len(ps)-2)
}

// touches reports whether any actual path falls in the contested set.
func touches(actual, contested []string) bool {
	for _, a := range actual {
		for _, c := range contested {
			if PatternsIntersect(a, c) {
				return true
			}
		}
	}
	return false
}

// broadest returns the glob with the shortest literal prefix, the one most
// worth splitting.
func broadest(pairs []PathPair) string {
	best := ""
	for _, p := range pairs {
		for _, g := range []string{p.Mine, p.Theirs} {
			if isGlob(g) && (best == "" || len(staticPrefix(g)) < len(staticPrefix(best))) {
				best = g
			}
		}
	}
	return best
}
