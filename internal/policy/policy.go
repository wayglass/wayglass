// Package policy loads .wayglass/policy.yaml and decides what happens to a
// classified change. Rules are checked in order and the first enforcing rule
// that matches wins; if none matches, the default applies. A rule marked
// `mode: shadow` never decides anything: every decision records the shadow
// rules that matched and whether the first of them would have changed the
// action, so a new rule can be tried on real changes before it is enforced.
package policy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"gopkg.in/yaml.v3"

	"github.com/wayglass/wayglass/internal/classify"
)

// Version is the only policy file version this build understands.
const Version = 1

// Action is what policy does with a change.
type Action string

// The actions, from least to most restrictive.
const (
	AutoMerge    Action = "auto_merge"
	AgentReview  Action = "agent_review"
	HumanApprove Action = "human_approve"
	MultiApprove Action = "multi_approve"
	Quarantine   Action = "quarantine"
	Block        Action = "block"
)

// Actions lists every valid action, least restrictive first.
func Actions() []Action {
	return []Action{AutoMerge, AgentReview, HumanApprove, MultiApprove, Quarantine, Block}
}

// Valid reports whether a is a known action.
func (a Action) Valid() bool { return slices.Contains(Actions(), a) }

// NeedsHuman reports whether a person must act before the change can land.
func (a Action) NeedsHuman() bool {
	return a == HumanApprove || a == MultiApprove || a == Quarantine
}

// Lands reports whether the change may land without a person.
func (a Action) Lands() bool { return a == AutoMerge || a == AgentReview }

// DefaultMultiApprovers is used when a multi_approve rule omits approvers.
const DefaultMultiApprovers = 2

// Values is a YAML field that accepts either one string or a list of strings.
// Within one field, any listed value matches (OR).
type Values []string

// UnmarshalYAML accepts `x` or `[x, y]`.
func (v *Values) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*v = Values{s}
		return nil
	case yaml.SequenceNode:
		var ss []string
		if err := n.Decode(&ss); err != nil {
			return err
		}
		*v = ss
		return nil
	default:
		return fmt.Errorf("line %d: want a string or a list of strings", n.Line)
	}
}

// When is a rule condition. Every set field must match (AND).
type When struct {
	// Area matches when any changed file is in any listed area.
	Area Values `yaml:"area" json:"area,omitempty"`
	// OnlyArea matches when every changed file is in at least one listed
	// area. Use it for permissive rules: `only_area: docs` auto-merges a
	// docs change, while `area: docs` would also match docs plus code.
	OnlyArea     Values `yaml:"only_area" json:"only_area,omitempty"`
	DataClass    Values `yaml:"data_class" json:"data_class,omitempty"`
	Risk         Values `yaml:"risk" json:"risk,omitempty"`
	Jurisdiction Values `yaml:"jurisdiction" json:"jurisdiction,omitempty"`
}

func (w When) empty() bool {
	return len(w.Area) == 0 && len(w.OnlyArea) == 0 && len(w.DataClass) == 0 && len(w.Risk) == 0 && len(w.Jurisdiction) == 0
}

// Matches reports whether the tags satisfy every set field of w.
func (w When) Matches(t classify.Tags) bool {
	if len(w.Area) > 0 && !slices.ContainsFunc(w.Area, t.HasArea) {
		return false
	}
	if len(w.OnlyArea) > 0 && !t.OnlyIn(w.OnlyArea...) {
		return false
	}
	if len(w.DataClass) > 0 && !slices.ContainsFunc(w.DataClass, t.HasDataClass) {
		return false
	}
	if len(w.Risk) > 0 && !slices.Contains(w.Risk, t.Risk) {
		return false
	}
	if len(w.Jurisdiction) > 0 && !slices.ContainsFunc(w.Jurisdiction, func(j string) bool {
		return strings.EqualFold(j, t.Jurisdiction)
	}) {
		return false
	}
	return true
}

// Mode says whether a rule decides or is only logged.
type Mode string

// The rule modes.
const (
	// Enforce rules decide: the first enforcing rule that matches wins.
	Enforce Mode = "enforce"
	// Shadow rules never decide. A matching shadow rule is recorded on the
	// decision with the action it would have taken.
	Shadow Mode = "shadow"
)

// ReviewerKind says who reviews an agent_review change.
type ReviewerKind string

// The reviewer kinds.
const (
	// ReviewerAny (the default): a reviewer agent, or the Worker's AI
	// reviewer when asked (wayglass ai-review) or when the Worker runs it on
	// every propose (AI_REVIEW=auto).
	ReviewerAny ReviewerKind = "any"
	// ReviewerAI: the Worker's AI reviewer runs on propose whenever it is
	// on at all (AI_REVIEW=manual or auto); AI_REVIEW=off still wins.
	ReviewerAI ReviewerKind = "ai"
	// ReviewerAgent: a reviewer agent decides; the AI reviewer refuses.
	ReviewerAgent ReviewerKind = "agent"
)

// Reviewer constrains who may approve a change a rule routed to review. In
// YAML it is either a mapping (`reviewer: { different_family: true, kind: ai }`)
// or, for the kind alone, a bare word (`reviewer: ai`).
type Reviewer struct {
	// DifferentFamily requires the approver's model family (claude, openai,
	// gemini, meta, other, or human for a person) to differ from the
	// author's, so a model never signs off on its own family's blind spots.
	// Families are declared by each agent (WAYGLASS_AGENT_FAMILY); the
	// Worker enforces the check at approve time.
	DifferentFamily bool `yaml:"different_family" json:"different_family,omitzero"`
	// Kind is who reviews an agent_review change: any (the default), ai or
	// agent. Only agent_review takes a kind.
	Kind ReviewerKind `yaml:"kind" json:"kind,omitempty"`
}

// UnmarshalYAML accepts `reviewer: ai` as well as the mapping form. Unknown
// keys are refused here because a node decoded on its own does not inherit
// the decoder's KnownFields setting.
func (r *Reviewer) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*r = Reviewer{Kind: ReviewerKind(s)}
		return nil
	case yaml.MappingNode:
		*r = Reviewer{}
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if seen[k.Value] {
				return fmt.Errorf("line %d: key %q already defined in reviewer", k.Line, k.Value)
			}
			seen[k.Value] = true
			var err error
			switch k.Value {
			case "different_family":
				err = v.Decode(&r.DifferentFamily)
			case "kind":
				err = v.Decode(&r.Kind)
			default:
				return fmt.Errorf("line %d: reviewer has no field %q (want different_family or kind)", k.Line, k.Value)
			}
			if err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("line %d: reviewer must be a word (any, ai, agent) or a mapping", n.Line)
	}
}

// AI tunes the Worker's AI reviewer for one rule. The Worker reads it from
// the decision; nothing here changes how the rule matches.
type AI struct {
	// MinConfidence is the self-reported confidence below which the AI's
	// approval is not trusted and the change goes to a person (0 keeps the
	// Worker's default, 0.7). agent_review only.
	MinConfidence float64 `yaml:"min_confidence" json:"min_confidence,omitzero"`
	// Model pins the reviewer model: a Workers AI id (`@cf/meta/...`) or a
	// gateway id (`anthropic/claude-...`, `openai/gpt-...`). The Worker uses
	// it only when its family differs from the author's and its provider is
	// enabled there; otherwise it picks as usual.
	Model string `yaml:"model" json:"model,omitempty"`
	// Advise asks the AI for advice (what the change does against its goal,
	// risks, questions) on a change routed to a person, written on propose.
	// Advice never decides. human_approve, multi_approve and quarantine only.
	Advise bool `yaml:"advise" json:"advise,omitzero"`
}

func (a AI) empty() bool { return a.MinConfidence == 0 && a.Model == "" && !a.Advise }

// Rule maps a condition to an action.
type Rule struct {
	Name      string `yaml:"name" json:"name,omitempty"`
	When      When   `yaml:"when" json:"when"`
	Action    Action `yaml:"action" json:"action"`
	Approvers int    `yaml:"approvers" json:"approvers,omitzero"`
	// Reviewer, if set, constrains who may approve (agent_review,
	// human_approve, multi_approve or quarantine only).
	Reviewer *Reviewer `yaml:"reviewer" json:"reviewer,omitempty"`
	// AI, if set, tunes the AI reviewer (agent_review) or asks for AI advice
	// (changes routed to a person).
	AI     *AI    `yaml:"ai" json:"ai,omitempty"`
	Reason string `yaml:"reason" json:"reason"`
	// Mode is enforce (the default) or shadow.
	Mode Mode `yaml:"mode" json:"mode"`
	// LegacyShadow is the older spelling `shadow: true`, kept so policy files
	// already on a trunk stay valid. New is normalised into Mode.
	LegacyShadow bool `yaml:"shadow" json:"-"`
}

// IsShadow reports whether r is logged instead of enforced.
func (r Rule) IsShadow() bool { return r.Mode == Shadow }

// Default is the action taken when no enforcing rule matches.
type Default struct {
	Action    Action    `yaml:"action" json:"action"`
	Approvers int       `yaml:"approvers" json:"approvers,omitzero"`
	Reviewer  *Reviewer `yaml:"reviewer" json:"reviewer,omitempty"`
	AI        *AI       `yaml:"ai" json:"ai,omitempty"`
	Reason    string    `yaml:"reason" json:"reason"`
}

// File is a parsed .wayglass/policy.yaml.
type File struct {
	Version  int             `yaml:"version" json:"version"`
	Classify classify.Config `yaml:"classify" json:"classify"`
	// Binary configures the built-in guard for files the detectors cannot
	// read. New fills in DefaultBinaryAllow when the file sets none.
	Binary  *Binary  `yaml:"binary" json:"binary"`
	Rules   []Rule   `yaml:"rules" json:"rules"`
	Default *Default `yaml:"default" json:"default"`
}

// Policy is a validated policy file with its compiled tagger.
type Policy struct {
	File   File
	Tagger *classify.Tagger
}

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("invalid policy")

// LoadFile reads and validates a policy file.
func LoadFile(path string) (*Policy, error) {
	b, err := os.ReadFile(path) //nolint:gosec // reading the policy file the user points at is the point
	if err != nil {
		return nil, err
	}
	p, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Parse decodes and validates policy YAML. Unknown keys are errors, so a typo
// such as `whne:` cannot silently turn a rule into a catch-all.
func Parse(b []byte) (*Policy, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: empty file", ErrInvalid)
		}
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	// Only the first YAML document is decoded; refuse a second one rather
	// than silently ignore rules written after a `---`.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: more than one YAML document; keep one policy per file", ErrInvalid)
	}
	return New(f)
}

// New validates f and compiles its classifier.
func New(f File) (*Policy, error) {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
	}
	if f.Version != Version {
		return nil, invalid("version is %d, want %d", f.Version, Version)
	}
	tagger, err := classify.New(f.Classify)
	if err != nil {
		return nil, invalid("%v", err)
	}
	if f.Default == nil {
		f.Default = &Default{Action: HumanApprove, Reason: "No rule matched; a person decides."}
	}
	if !f.Default.Action.Valid() {
		return nil, invalid("default: unknown action %q", f.Default.Action)
	}
	if strings.TrimSpace(f.Default.Reason) == "" {
		return nil, invalid("default: reason is required")
	}
	if f.Default.Approvers, err = approvers(f.Default.Action, f.Default.Approvers); err != nil {
		return nil, invalid("default: %v", err)
	}
	if err := checkReviewer(f.Default.Action, f.Default.Reviewer); err != nil {
		return nil, invalid("default: %v", err)
	}
	if f.Binary == nil {
		f.Binary = &Binary{}
	}
	if f.Binary.Allow == nil {
		f.Binary.Allow = slices.Clone(DefaultBinaryAllow)
	}
	for _, g := range f.Binary.Allow {
		if !doublestar.ValidatePattern(g) {
			return nil, invalid("binary.allow: invalid glob %q", g)
		}
	}
	if err := checkAI(f.Default.Action, f.Default.AI); err != nil {
		return nil, invalid("default: %v", err)
	}

	names := map[string]bool{}
	for i := range f.Rules {
		r := &f.Rules[i]
		where := fmt.Sprintf("rules[%d]", i)
		if r.Name != "" {
			if names[r.Name] {
				return nil, invalid("%s: duplicate rule name %q", where, r.Name)
			}
			names[r.Name] = true
			where = fmt.Sprintf("%s (%s)", where, r.Name)
		}
		if r.When.empty() {
			return nil, invalid("%s: when is empty; use default for a catch-all", where)
		}
		if !r.Action.Valid() {
			return nil, invalid("%s: unknown action %q", where, r.Action)
		}
		if strings.TrimSpace(r.Reason) == "" {
			return nil, invalid("%s: reason is required; every decision carries its why", where)
		}
		if r.Approvers, err = approvers(r.Action, r.Approvers); err != nil {
			return nil, invalid("%s: %v", where, err)
		}
		if err := checkReviewer(r.Action, r.Reviewer); err != nil {
			return nil, invalid("%s: %v", where, err)
		}
		if err := checkAI(r.Action, r.AI); err != nil {
			return nil, invalid("%s: %v", where, err)
		}
		switch r.Mode {
		case "":
			r.Mode = Enforce
			if r.LegacyShadow {
				r.Mode = Shadow
			}
		case Enforce:
			if r.LegacyShadow {
				return nil, invalid("%s: mode: enforce contradicts shadow: true; keep only mode", where)
			}
		case Shadow:
		default:
			return nil, invalid("%s: unknown mode %q (want enforce or shadow)", where, r.Mode)
		}
		r.LegacyShadow = false
		for _, a := range r.When.Area {
			if !tagger.HasArea(a) {
				return nil, invalid("%s: area %q is not defined under classify.area", where, a)
			}
		}
		for _, a := range r.When.OnlyArea {
			if !tagger.HasArea(a) {
				return nil, invalid("%s: only_area %q is not defined under classify.area", where, a)
			}
		}
		for _, d := range r.When.DataClass {
			if !tagger.HasDataClass(d) {
				return nil, invalid("%s: data_class %q is not defined under classify.data_class", where, d)
			}
		}
		for _, lvl := range r.When.Risk {
			if !slices.Contains(classify.RiskLevels(), lvl) {
				return nil, invalid("%s: unknown risk %q (want one of %s)", where, lvl, strings.Join(classify.RiskLevels(), ", "))
			}
		}
		for j, v := range r.When.Jurisdiction {
			r.When.Jurisdiction[j] = strings.ToLower(strings.TrimSpace(v))
		}
	}
	return &Policy{File: f, Tagger: tagger}, nil
}

// checkReviewer refuses a reviewer constraint on an action nobody approves,
// an empty one, which would read as a constraint but do nothing, and a kind
// on anything but agent_review, where only a person decides anyway.
func checkReviewer(a Action, r *Reviewer) error {
	switch {
	case r == nil:
		return nil
	case a == AutoMerge || a == Block:
		return fmt.Errorf("reviewer does not apply to %s: nobody approves it", a)
	case !r.DifferentFamily && r.Kind == "":
		return errors.New("reviewer is empty; set different_family: true, a kind (any, ai, agent) or remove it")
	}
	switch r.Kind {
	case "", ReviewerAny:
	case ReviewerAI, ReviewerAgent:
		if a != AgentReview {
			return fmt.Errorf("reviewer: %s applies to %s only; %s is decided by a person", r.Kind, AgentReview, a)
		}
	default:
		return fmt.Errorf("unknown reviewer kind %q (want any, ai or agent)", r.Kind)
	}
	return nil
}

// checkAI refuses AI settings that cannot take effect: on an action nobody
// reviews, a confidence floor outside (0, 1] or on a change a person
// decides, advice on a change the AI reviews itself, a model id that is not
// one. The Worker never guesses what an invalid setting meant.
func checkAI(a Action, ai *AI) error {
	switch {
	case ai == nil:
		return nil
	case a == AutoMerge || a == Block:
		return fmt.Errorf("ai does not apply to %s: nobody reviews it", a)
	case ai.empty():
		return errors.New("ai is empty; set min_confidence, model or advise: true, or remove it")
	}
	if ai.MinConfidence != 0 {
		if a != AgentReview {
			return fmt.Errorf("ai.min_confidence applies to %s only; %s is decided by a person", AgentReview, a)
		}
		if ai.MinConfidence < 0 || ai.MinConfidence > 1 {
			return fmt.Errorf("ai.min_confidence must be between 0 and 1, got %v", ai.MinConfidence)
		}
	}
	if ai.Advise && a == AgentReview {
		return fmt.Errorf("ai.advise applies to changes routed to a person; %s is reviewed, not advised", AgentReview)
	}
	if ai.Model != "" && !modelID.MatchString(ai.Model) {
		return fmt.Errorf("ai.model %q is not a model id (want @cf/<vendor>/<model> or <vendor>/<model>)", ai.Model)
	}
	return nil
}

// modelID is a Workers AI id (@cf/vendor/model) or an AI Gateway id (vendor/model).
var modelID = regexp.MustCompile(`^(?:@cf/)?[A-Za-z0-9._-]+/[A-Za-z0-9._/-]+$`)

func approvers(a Action, n int) (int, error) {
	switch {
	case a != MultiApprove && n != 0:
		return 0, fmt.Errorf("approvers only applies to %s", MultiApprove)
	case a == MultiApprove && n == 0:
		return DefaultMultiApprovers, nil
	case a == MultiApprove && n < 2:
		return 0, fmt.Errorf("%s needs approvers >= 2, got %d", MultiApprove, n)
	}
	return n, nil
}

// Match identifies the rule behind a decision.
type Match struct {
	// Index is the rule's position in rules; -1 means the default.
	Index     int       `json:"index"`
	Name      string    `json:"name,omitempty"`
	Action    Action    `json:"action"`
	Approvers int       `json:"approvers,omitzero"`
	Reviewer  *Reviewer `json:"reviewer,omitempty"`
	AI        *AI       `json:"ai,omitempty"`
	Reason    string    `json:"reason"`
}

// ID is a stable human label: the rule name, "rules[i]", "default", or a
// built-in guard's name.
func (m Match) ID() string {
	switch {
	case m.Index == BuiltinIndex:
		return m.Name
	case m.Index < 0:
		return "default"
	case m.Name != "":
		return m.Name
	default:
		return fmt.Sprintf("rules[%d]", m.Index)
	}
}

// Decision is the outcome of evaluating one change.
type Decision struct {
	Action    Action `json:"action"`
	Approvers int    `json:"approvers,omitzero"`
	// Reviewer constrains who may approve; nil when anyone other than the
	// author may.
	Reviewer *Reviewer `json:"reviewer,omitempty"`
	// AI tunes the AI reviewer or asks for AI advice; nil when the rule
	// says nothing about it.
	AI      *AI           `json:"ai,omitempty"`
	Reason  string        `json:"reason"`
	Rule    Match         `json:"rule"`
	Default bool          `json:"default,omitzero"`
	Tags    classify.Tags `json:"tags"`
	// ShadowMatches lists every shadow rule that matched, in file order. It
	// is an empty list, never null, when none did.
	ShadowMatches []ShadowMatch `json:"shadow_matches"`
	// WouldDiffer is true when enforcing the first shadow rule that comes
	// before the deciding rule would have changed the action.
	WouldDiffer bool `json:"would_differ"`
	// Overrode is the rule a built-in guard raised; nil when none did.
	Overrode *Match `json:"overrode,omitempty"`

	// floor is the highest built-in floor applied so far, so WouldDiffer
	// judges the shadow rule under the same guards.
	floor Action
}

// ShadowMatch is a shadow rule that matched a change.
type ShadowMatch struct {
	// Rule is the rule's label (its name or rules[i]).
	Rule      string `json:"rule"`
	Index     int    `json:"index"`
	Action    Action `json:"action"`
	Approvers int    `json:"approvers,omitzero"`
	Reason    string `json:"reason"`
	// Preempts is true when the rule comes before the rule that decided, so
	// enforcing it would have made it the deciding rule. A shadow rule after
	// the deciding rule would never be reached even if enforced.
	Preempts bool `json:"preempts"`
}

// FirstPreempting returns the shadow match that would have decided had it
// been enforced, if any.
func (d Decision) FirstPreempting() (ShadowMatch, bool) {
	for _, s := range d.ShadowMatches {
		if s.Preempts {
			return s, true
		}
	}
	return ShadowMatch{}, false
}

// Evaluate classifies a change and decides on it, then applies the built-in
// guards that no policy file can turn off (see builtin.go).
func (p *Policy) Evaluate(c classify.Change) Decision {
	d := p.Decide(p.Tagger.Classify(c))
	if touchesPolicy(c) {
		d = d.atLeast(HumanApprove, BuiltinProtectPolicy, protectReason)
	}
	return d.guardUnscanned(c, p.File.Binary.Allow)
}

// Decide applies the rules to already classified tags. Enforcing rules
// decide exactly as if no shadow rule existed; every matching shadow rule is
// recorded, including those after the deciding rule.
func (p *Policy) Decide(t classify.Tags) Decision {
	d := Decision{Tags: t, ShadowMatches: []ShadowMatch{}}
	decided := false
	for i, r := range p.File.Rules {
		if !r.When.Matches(t) {
			continue
		}
		m := Match{Index: i, Name: r.Name, Action: r.Action, Approvers: r.Approvers, Reviewer: r.Reviewer, AI: r.AI, Reason: r.Reason}
		if r.IsShadow() {
			d.ShadowMatches = append(d.ShadowMatches, ShadowMatch{
				Rule: m.ID(), Index: i, Action: r.Action, Approvers: r.Approvers, Reason: r.Reason, Preempts: !decided,
			})
			continue
		}
		if !decided {
			d.Action, d.Approvers, d.Reviewer, d.AI, d.Reason, d.Rule = r.Action, r.Approvers, r.Reviewer, r.AI, r.Reason, m
			decided = true
		}
	}
	if !decided {
		def := p.File.Default
		d.Action, d.Approvers, d.Reviewer, d.AI, d.Reason, d.Default = def.Action, def.Approvers, def.Reviewer, def.AI, def.Reason, true
		d.Rule = Match{Index: -1, Action: def.Action, Approvers: def.Approvers, Reviewer: def.Reviewer, AI: def.AI, Reason: def.Reason}
	}
	d.WouldDiffer = d.wouldDiffer("")
	return d
}

// wouldDiffer compares the decision with what the first preempting shadow
// rule would have produced, under the same built-in floor (none when floor
// is empty): a guard that raises the real decision would raise the
// hypothetical one too.
func (d Decision) wouldDiffer(floor Action) bool {
	s, ok := d.FirstPreempting()
	if !ok {
		return false
	}
	alt, altN := s.Action, s.Approvers
	if floor != "" && alt.rank() < floor.rank() {
		alt, altN = floor, 0
	}
	return alt != d.Action || (alt == MultiApprove && altN != d.Approvers)
}
