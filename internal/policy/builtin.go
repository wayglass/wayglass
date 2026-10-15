package policy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wayglass/wayglass/internal/classify"
)

// Built-in guards sit outside the policy file because they protect the policy
// file itself, or cover cases where the file could not be applied. A guard
// only ever raises an action to a floor; it never relaxes one.
const (
	// BuiltinIndex marks a Match produced by a built-in guard.
	BuiltinIndex = -2

	// BuiltinProtectPolicy: a change that touches .wayglass/ could rewrite
	// the rules it is judged by, so a person decides at minimum.
	BuiltinProtectPolicy = "builtin:protect-policy"
	// BuiltinNoPolicy: the base commit has no policy file.
	BuiltinNoPolicy = "builtin:no-policy"
	// BuiltinInvalidPolicy: the base commit's policy file does not parse.
	BuiltinInvalidPolicy = "builtin:invalid-policy"
	// BuiltinTruncated: the change was too large to read in full, so its
	// tags may be incomplete.
	BuiltinTruncated = "builtin:diff-truncated"
	// BuiltinUnscannedBinary: the change adds or replaces a binary file
	// outside binary.allow. The content detectors read none of it, so the
	// data classes may be incomplete and a person decides.
	BuiltinUnscannedBinary = "builtin:unscanned-binary"
)

// Dir is the directory that holds the policy and other Wayglass config.
const Dir = ".wayglass"

const protectReason = "Changes to .wayglass/ edit the rules themselves; a person decides."

// DefaultBinaryAllow is binary.allow when the policy file sets none: the
// assets a web or docs repo ships that a person would not want to review
// one by one. Anything else that the detectors cannot read (an archive, an
// executable, a key store, a file with no extension) goes to a person.
// Trust rests on the name, so a repo that ships no PDFs should drop that
// entry, and a stricter list can pin assets to their directory.
var DefaultBinaryAllow = []string{
	"**/*.png", "**/*.jpg", "**/*.jpeg", "**/*.gif", "**/*.webp", "**/*.ico",
	"**/*.woff", "**/*.woff2",
}

// Binary is the `binary:` section of the policy file.
type Binary struct {
	// Allow lists globs for binary files that may land without a person.
	// Absent means DefaultBinaryAllow; an explicit empty list allows none.
	Allow []string `yaml:"allow" json:"allow"`
}

// unscannedBinaries lists the files whose new side is binary and that no
// allow glob covers, in change order.
func unscannedBinaries(c classify.Change, allow []string) []string {
	var paths []string
	for _, f := range c.Files {
		if !f.Binary {
			continue
		}
		if _, ok := classify.MatchAny(allow, f.Path); !ok {
			paths = append(paths, f.Path)
		}
	}
	return paths
}

// unscannedReason names the files so the person asked to decide knows what
// to open; the list is cut short so a bulk change does not flood the record.
func unscannedReason(paths []string) string {
	const show = 3
	if len(paths) == 1 {
		return fmt.Sprintf("%s is a binary file outside binary.allow; its content cannot be scanned, so a person decides.", paths[0])
	}
	listed := strings.Join(paths[:min(len(paths), show)], ", ")
	if len(paths) > show {
		listed += fmt.Sprintf(" and %d more", len(paths)-show)
	}
	return fmt.Sprintf("%d binary files outside binary.allow (%s); their content cannot be scanned, so a person decides.", len(paths), listed)
}

// guardUnscanned raises d to a person when the change holds a binary file
// outside allow. It never lowers a stricter action.
func (d Decision) guardUnscanned(c classify.Change, allow []string) Decision {
	if paths := unscannedBinaries(c, allow); len(paths) > 0 {
		return d.atLeast(HumanApprove, BuiltinUnscannedBinary, unscannedReason(paths))
	}
	return d
}

// rank orders actions from least to most restrictive.
func (a Action) rank() int { return slices.Index(Actions(), a) }

func touchesPolicy(c classify.Change) bool {
	return slices.ContainsFunc(c.Files, func(f classify.FileChange) bool {
		p := classify.NormalizePath(f.Path)
		return p == Dir || strings.HasPrefix(p, Dir+"/")
	})
}

// atLeast raises d to floor when d is less restrictive, and keeps the rule it
// overrode so the record still shows what the policy file said.
func (d Decision) atLeast(floor Action, name, reason string) Decision {
	if d.Action.rank() < floor.rank() {
		prev := d.Rule
		d.Overrode = &prev
		// The guard's floor replaces the rule's routing, reviewer constraint and AI settings included.
		d.Action, d.Approvers, d.Reviewer, d.AI, d.Reason, d.Default = floor, 0, nil, nil, reason, false
		d.Rule = Match{Index: BuiltinIndex, Name: name, Action: floor, Reason: reason}
	}
	if d.floor == "" || d.floor.rank() < floor.rank() {
		d.floor = floor
	}
	d.WouldDiffer = d.wouldDiffer(d.floor)
	return d
}

// fallback is the decision when no usable policy exists: tag what can be
// tagged without config and send the change to a person.
func fallback(c classify.Change, name, reason string) Decision {
	empty, _ := classify.New(classify.Config{}) // an empty config always compiles
	return Decision{
		Action:        HumanApprove,
		Reason:        reason,
		Rule:          Match{Index: BuiltinIndex, Name: name, Action: HumanApprove, Reason: reason},
		Tags:          empty.Classify(c),
		ShadowMatches: []ShadowMatch{},
	}
}
