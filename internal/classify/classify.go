// Package classify tags a proposed change by area, data class, risk and
// jurisdiction. Tagging is deterministic: the same change and config always
// produce the same tags, and every tag carries the evidence that produced it,
// so a policy decision can always say which rule matched and why.
package classify

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Risk levels, lowest first. A change that crosses no configured threshold is
// RiskLow.
const (
	RiskLow      = "low"
	RiskMedium   = "medium"
	RiskHigh     = "high"
	RiskCritical = "critical"
)

var riskRank = map[string]int{RiskLow: 0, RiskMedium: 1, RiskHigh: 2, RiskCritical: 3}

// RiskLevels lists the valid risk levels, lowest first.
func RiskLevels() []string { return []string{RiskLow, RiskMedium, RiskHigh, RiskCritical} }

// Config is the `classify:` section of .wayglass/policy.yaml.
type Config struct {
	// Area maps an area name to path globs. `**` matches any number of
	// directories, including none.
	Area map[string][]string `yaml:"area" json:"area,omitempty"`
	// DataClass maps a data class name to its detectors.
	DataClass map[string]DataClassRule `yaml:"data_class" json:"data_class,omitempty"`
	// Risk maps a risk level (low, medium, high, critical) to its threshold.
	Risk map[string]RiskRule `yaml:"risk" json:"risk,omitempty"`
}

// DataClassRule detects a data class. A change has the class when any added
// line matches any Content regex, or any changed path matches any Paths glob.
type DataClassRule struct {
	Content []string `yaml:"content" json:"content,omitempty"`
	Paths   []string `yaml:"paths" json:"paths,omitempty"`
}

// RiskRule sets the threshold for a risk level. The level applies when any
// set threshold is exceeded; zero means unset.
type RiskRule struct {
	LinesChangedOver int `yaml:"lines_changed_over" json:"lines_changed_over,omitzero"`
	FilesChangedOver int `yaml:"files_changed_over" json:"files_changed_over,omitzero"`
	// BinaryBytesOver is a threshold on the total size of the binary files
	// in the change (their new side). Binary files carry no lines, so
	// without it a large asset is never more than low risk.
	BinaryBytesOver int `yaml:"binary_bytes_over" json:"binary_bytes_over,omitzero"`
}

// FileChange is one file in a proposed change.
type FileChange struct {
	Path string `json:"path"`
	// Added holds the text of each added line, without the leading '+'.
	Added []string `json:"added,omitempty"`
	// Deleted counts removed lines; their text is never inspected.
	Deleted int `json:"deleted"`
	// Binary says the file's new side is content the detectors cannot read
	// (git's rule: a NUL byte in the first 8,000 bytes), so Added is empty
	// however much changed. A deleted binary file is not Binary: nothing
	// new arrives with it.
	Binary bool `json:"binary,omitzero"`
	// Bytes is the size of the file's new side when the caller knows it
	// (the Worker reads the blob; the CLI asks git). Zero means unknown.
	Bytes int `json:"bytes,omitzero"`
}

// Change is the input to classification.
type Change struct {
	Files []FileChange `json:"files"`
	// Jurisdiction comes from the Artifacts namespace the change lives in
	// (for example "us" or "eu"). It is passed in, never guessed.
	Jurisdiction string `json:"jurisdiction"`
}

// Evidence records why a tag was applied. It names the file and the pattern,
// never the matched text, so a secret that triggers the secrets class is not
// copied into tags, logs or notes.
type Evidence struct {
	Tag     string `json:"tag"`   // area, data_class, risk, jurisdiction or unscanned
	Value   string `json:"value"` // the tag value, e.g. "auth"
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitzero"` // 1-based index into the file's added lines
	Pattern string `json:"pattern,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Tags is the classification of one change.
type Tags struct {
	Areas []string `json:"areas"`
	// AreaSets lists the distinct sets of areas the changed files fall in,
	// one entry per distinct set, sorted; a file in no area adds an empty
	// set. It answers "is every file in one of these areas?" (OnlyIn), which
	// Areas alone cannot: Areas says some file touched an area, not that all
	// files stayed inside it.
	AreaSets     [][]string `json:"area_sets"`
	DataClasses  []string   `json:"data_classes"`
	Risk         string     `json:"risk"`
	Jurisdiction string     `json:"jurisdiction"`
	LinesChanged int        `json:"lines_changed"`
	FilesChanged int        `json:"files_changed"`
	// Unscanned lists the binary files, in change order: the content
	// detectors read none of their bytes, so a data class they would carry
	// as text is missing from DataClasses. Policy routes them by name
	// (binary.allow) because nothing else can vouch for them.
	Unscanned []string `json:"unscanned"`
	// BinaryBytes is the total size of the binary files' new sides, as far
	// as the caller knew it.
	BinaryBytes int        `json:"binary_bytes"`
	Evidence    []Evidence `json:"evidence"`
}

// HasArea reports whether the change touched the named area.
func (t Tags) HasArea(name string) bool { return slices.Contains(t.Areas, name) }

// OnlyIn reports whether every changed file is in at least one of the named
// areas. A change with no files, or tags recorded before AreaSets existed,
// is never "only" anything, so a permissive rule cannot match by accident.
func (t Tags) OnlyIn(areas ...string) bool {
	if len(t.AreaSets) == 0 {
		return false
	}
	for _, set := range t.AreaSets {
		if !slices.ContainsFunc(set, func(a string) bool { return slices.Contains(areas, a) }) {
			return false
		}
	}
	return true
}

// HasDataClass reports whether the change carries the named data class.
func (t Tags) HasDataClass(name string) bool { return slices.Contains(t.DataClasses, name) }

// HasUnscanned reports whether any file in the change escaped the content
// detectors.
func (t Tags) HasUnscanned() bool { return len(t.Unscanned) > 0 }

type areaRule struct {
	name  string
	globs []string
}

type dataClassRule struct {
	name    string
	content []*regexp.Regexp
	paths   []string
}

type riskRule struct {
	level string
	RiskRule
}

// Tagger is a compiled Config. It is safe for concurrent use.
type Tagger struct {
	areas       []areaRule
	dataClasses []dataClassRule
	risks       []riskRule // highest rank first
}

// New validates and compiles a Config.
func New(cfg Config) (*Tagger, error) {
	t := &Tagger{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Area)) {
		globs := cfg.Area[name]
		if len(globs) == 0 {
			return nil, fmt.Errorf("classify.area.%s: needs at least one glob", name)
		}
		for _, g := range globs {
			if !doublestar.ValidatePattern(g) {
				return nil, fmt.Errorf("classify.area.%s: invalid glob %q", name, g)
			}
		}
		t.areas = append(t.areas, areaRule{name: name, globs: slices.Clone(globs)})
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.DataClass)) {
		r := cfg.DataClass[name]
		if len(r.Content) == 0 && len(r.Paths) == 0 {
			return nil, fmt.Errorf("classify.data_class.%s: needs content or paths", name)
		}
		dc := dataClassRule{name: name, paths: slices.Clone(r.Paths)}
		for _, expr := range r.Content {
			re, err := regexp.Compile(expr)
			if err != nil {
				return nil, fmt.Errorf("classify.data_class.%s: bad regex %q: %w", name, expr, err)
			}
			dc.content = append(dc.content, re)
		}
		for _, g := range r.Paths {
			if !doublestar.ValidatePattern(g) {
				return nil, fmt.Errorf("classify.data_class.%s: invalid glob %q", name, g)
			}
		}
		t.dataClasses = append(t.dataClasses, dc)
	}
	for level, r := range cfg.Risk {
		if _, ok := riskRank[level]; !ok {
			return nil, fmt.Errorf("classify.risk.%s: unknown level (want one of %s)", level, strings.Join(RiskLevels(), ", "))
		}
		if level == RiskLow {
			return nil, fmt.Errorf("classify.risk.low: low is the default level and takes no threshold")
		}
		if r.LinesChangedOver < 0 || r.FilesChangedOver < 0 || r.BinaryBytesOver < 0 {
			return nil, fmt.Errorf("classify.risk.%s: thresholds must not be negative", level)
		}
		if r.LinesChangedOver == 0 && r.FilesChangedOver == 0 && r.BinaryBytesOver == 0 {
			return nil, fmt.Errorf("classify.risk.%s: needs lines_changed_over, files_changed_over or binary_bytes_over", level)
		}
		t.risks = append(t.risks, riskRule{level: level, RiskRule: r})
	}
	slices.SortFunc(t.risks, func(a, b riskRule) int { return cmp.Compare(riskRank[b.level], riskRank[a.level]) })
	return t, nil
}

// HasArea reports whether the config defines the named area.
func (t *Tagger) HasArea(name string) bool {
	return slices.ContainsFunc(t.areas, func(a areaRule) bool { return a.name == name })
}

// HasDataClass reports whether the config defines the named data class.
func (t *Tagger) HasDataClass(name string) bool {
	return slices.ContainsFunc(t.dataClasses, func(d dataClassRule) bool { return d.name == name })
}

// Classify tags a change.
func (t *Tagger) Classify(c Change) Tags {
	tags := Tags{
		Areas:        []string{},
		AreaSets:     [][]string{},
		DataClasses:  []string{},
		Jurisdiction: strings.ToLower(strings.TrimSpace(c.Jurisdiction)),
		FilesChanged: len(c.Files),
		Unscanned:    []string{},
		Evidence:     []Evidence{},
	}
	for _, f := range c.Files {
		tags.LinesChanged += len(f.Added) + f.Deleted
		if f.Binary {
			tags.Unscanned = append(tags.Unscanned, f.Path)
			tags.BinaryBytes += f.Bytes
		}
	}

	for _, a := range t.areas {
		if ev, ok := a.match(c.Files); ok {
			tags.Areas = append(tags.Areas, a.name)
			tags.Evidence = append(tags.Evidence, ev)
		}
	}
	seen := map[string]bool{}
	for _, f := range c.Files {
		set := []string{}
		for _, a := range t.areas {
			if _, ok := matchAny(a.globs, f.Path); ok {
				set = append(set, a.name) // t.areas is sorted, so set is too
			}
		}
		if key := strings.Join(set, "\x00"); !seen[key] {
			seen[key] = true
			tags.AreaSets = append(tags.AreaSets, set)
		}
	}
	slices.SortFunc(tags.AreaSets, func(a, b []string) int { return slices.Compare(a, b) })
	for _, d := range t.dataClasses {
		if ev, ok := d.match(c.Files); ok {
			tags.DataClasses = append(tags.DataClasses, d.name)
			tags.Evidence = append(tags.Evidence, ev)
		}
	}

	tags.Risk = RiskLow
	for _, r := range t.risks {
		if r.LinesChangedOver > 0 && tags.LinesChanged > r.LinesChangedOver {
			tags.Risk = r.level
			tags.Evidence = append(tags.Evidence, Evidence{Tag: "risk", Value: r.level,
				Detail: fmt.Sprintf("%d lines changed > %d", tags.LinesChanged, r.LinesChangedOver)})
			break
		}
		if r.FilesChangedOver > 0 && tags.FilesChanged > r.FilesChangedOver {
			tags.Risk = r.level
			tags.Evidence = append(tags.Evidence, Evidence{Tag: "risk", Value: r.level,
				Detail: fmt.Sprintf("%d files changed > %d", tags.FilesChanged, r.FilesChangedOver)})
			break
		}
		if r.BinaryBytesOver > 0 && tags.BinaryBytes > r.BinaryBytesOver {
			tags.Risk = r.level
			tags.Evidence = append(tags.Evidence, Evidence{Tag: "risk", Value: r.level,
				Detail: fmt.Sprintf("%d binary bytes > %d", tags.BinaryBytes, r.BinaryBytesOver)})
			break
		}
	}
	if tags.Jurisdiction != "" {
		tags.Evidence = append(tags.Evidence, Evidence{Tag: "jurisdiction", Value: tags.Jurisdiction,
			Detail: "namespace jurisdiction"})
	}
	for _, f := range c.Files {
		if f.Binary {
			detail := "binary file; the content detectors read none of it"
			if f.Bytes > 0 {
				detail = fmt.Sprintf("binary file of %d bytes; the content detectors read none of it", f.Bytes)
			}
			tags.Evidence = append(tags.Evidence, Evidence{Tag: "unscanned", Value: "binary", Path: f.Path, Detail: detail})
		}
	}
	return tags
}

func (a areaRule) match(files []FileChange) (Evidence, bool) {
	for _, f := range files {
		if g, ok := matchAny(a.globs, f.Path); ok {
			return Evidence{Tag: "area", Value: a.name, Path: f.Path, Pattern: g}, true
		}
	}
	return Evidence{}, false
}

func (d dataClassRule) match(files []FileChange) (Evidence, bool) {
	for _, f := range files {
		if g, ok := matchAny(d.paths, f.Path); ok {
			return Evidence{Tag: "data_class", Value: d.name, Path: f.Path, Pattern: g}, true
		}
		for i, line := range f.Added {
			for _, re := range d.content {
				if re.MatchString(line) {
					return Evidence{Tag: "data_class", Value: d.name, Path: f.Path, Line: i + 1, Pattern: re.String()}, true
				}
			}
		}
	}
	return Evidence{}, false
}

// MatchAny reports the first glob that matches path, normalised with
// NormalizePath, the way areas and data_class.paths are matched.
func MatchAny(globs []string, path string) (string, bool) { return matchAny(globs, path) }

func matchAny(globs []string, path string) (string, bool) {
	path = NormalizePath(path)
	for _, g := range globs {
		if ok, _ := doublestar.Match(g, path); ok {
			return g, true
		}
	}
	return "", false
}

// NormalizePath is the form a changed path takes before any glob sees it:
// a leading "./" dropped and every backslash read as a separator, the same
// way declared paths are normalised for overlap checks. Globs stay
// case-sensitive, so `web/**` does not cover `Web/`; write globs for the
// paths the repo really has.
func NormalizePath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	for {
		rest, ok := strings.CutPrefix(p, "./")
		if !ok {
			return p
		}
		p = rest
	}
}
