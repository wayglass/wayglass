package policy

import "github.com/wayglass/wayglass/internal/classify"

// RuleHit is one rule that matched a change, whether it is enforced or in
// shadow mode, and whether it decided.
type RuleHit struct {
	Rule      string `json:"rule"`
	Index     int    `json:"index"`
	Mode      Mode   `json:"mode"`
	Action    Action `json:"action"`
	Approvers int    `json:"approvers,omitzero"`
}

// Hits lists every rule whose condition the tags satisfy, in file order. It
// answers "what would each rule have decided", which Decide alone does not:
// enforcing rules after the first match never show up in a Decision.
func (p *Policy) Hits(t classify.Tags) []RuleHit {
	hits := []RuleHit{}
	for i, r := range p.File.Rules {
		if !r.When.Matches(t) {
			continue
		}
		m := Match{Index: i, Name: r.Name}
		hits = append(hits, RuleHit{Rule: m.ID(), Index: i, Mode: r.Mode, Action: r.Action, Approvers: r.Approvers})
	}
	return hits
}

// RuleStats summarises one rule over a replay of many changes.
type RuleStats struct {
	Rule string `json:"rule"`
	// Index is the rule's position; -1 is the default, -2 a built-in guard.
	Index  int    `json:"index"`
	Mode   Mode   `json:"mode,omitempty"`
	Action Action `json:"action"`
	// Matched counts changes whose tags satisfied the rule.
	Matched int `json:"matched"`
	// Decided counts changes the rule decided. For a shadow rule it counts
	// changes it would have decided had it been enforced (it was the first
	// preempting shadow match).
	Decided int `json:"decided"`
	// Differed counts, for a shadow rule, the changes where enforcing it
	// would have changed the action.
	Differed int `json:"differed"`
}

// Summarize tallies decisions made by p, one row per rule in file order,
// then the default, then any built-in guard that decided something.
func (p *Policy) Summarize(ds []Decision) []RuleStats {
	rows := make([]RuleStats, 0, len(p.File.Rules)+1)
	byID := map[string]int{}
	add := func(s RuleStats) {
		byID[s.Rule] = len(rows)
		rows = append(rows, s)
	}
	for i, r := range p.File.Rules {
		add(RuleStats{Rule: Match{Index: i, Name: r.Name}.ID(), Index: i, Mode: r.Mode, Action: r.Action})
	}
	add(RuleStats{Rule: "default", Index: -1, Action: p.File.Default.Action})
	for _, d := range ds {
		for _, h := range p.Hits(d.Tags) {
			rows[byID[h.Rule]].Matched++
		}
		id := d.Rule.ID()
		if _, ok := byID[id]; !ok {
			add(RuleStats{Rule: id, Index: d.Rule.Index, Action: d.Rule.Action})
		}
		rows[byID[id]].Decided++
		if s, ok := d.FirstPreempting(); ok {
			rows[byID[s.Rule]].Decided++
			if d.WouldDiffer {
				rows[byID[s.Rule]].Differed++
			}
		}
	}
	return rows
}
