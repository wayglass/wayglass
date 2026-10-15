package cli

import (
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/policy"
)

const trialPolicy = `version: 1
classify:
  area: { docs: ["docs/**"], web: ["web/**"] }
rules:
  - { name: trial-web, when: { area: web }, action: quarantine, reason: "Trial: web needs review.", mode: shadow }
  - { name: docs, when: { area: docs }, action: auto_merge, reason: "Docs." }
  - { name: web, when: { area: web }, action: agent_review, reason: "Web." }
default: { action: human_approve, reason: "Fallback." }
`

func TestPolicyTestReplaysCommits(t *testing.T) {
	e := newFlow(t) // commit 1: README.md and web/login.ts
	e.write(e.repo, ".wayglass/policy.yaml", trialPolicy)
	e.git(e.repo, "add", "-A")
	e.git(e.repo, "commit", "--quiet", "-m", "chore: add policy")
	e.write(e.repo, "docs/a.md", "a\n")
	e.git(e.repo, "add", "-A")
	e.git(e.repo, "commit", "--quiet", "-m", "docs: a")
	e.write(e.repo, "web/login.ts", "export function login() { return 1 }\n")
	e.git(e.repo, "commit", "--quiet", "-am", "feat: web")
	first := strings.TrimSpace(e.git(e.repo, "rev-list", "--max-parents=0", "HEAD"))

	tests := []struct {
		name     string
		args     []string
		subjects []string
		actions  []string
		differ   float64
		trunc    bool
	}{
		{"default range", nil, []string{"feat: web", "docs: a", "chore: add policy", "initial"},
			[]string{"agent_review", "auto_merge", "human_approve", "agent_review"}, 2, false},
		{"since", []string{"--since", first}, []string{"feat: web", "docs: a", "chore: add policy"},
			[]string{"agent_review", "auto_merge", "human_approve"}, 1, false},
		{"limit", []string{"-n", "1"}, []string{"feat: web"}, []string{"agent_review"}, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := e.do(e.repo, ExitOK, append([]string{"policy", "test"}, tt.args...)...)
			commits := get(d, "commits").([]any)
			var subjects, actions []string
			for _, c := range commits {
				subjects = append(subjects, get(c.(map[string]any), "subject").(string))
				actions = append(actions, get(c.(map[string]any), "decision", "action").(string))
			}
			if strings.Join(subjects, "|") != strings.Join(tt.subjects, "|") || strings.Join(actions, "|") != strings.Join(tt.actions, "|") {
				t.Fatalf("replay = %v %v, want %v %v", subjects, actions, tt.subjects, tt.actions)
			}
			if got := get(d, "summary", "would_differ"); got != tt.differ {
				t.Errorf("would_differ = %v, want %v", got, tt.differ)
			}
			if get(d, "truncated") != tt.trunc {
				t.Errorf("truncated = %v", get(d, "truncated"))
			}
		})
	}

	// The web commit carries the shadow match; the trial rule shows in the summary.
	d := e.do(e.repo, ExitOK, "policy", "test", "-n", "1")
	c := get(d, "commits").([]any)[0].(map[string]any)
	sm := get(c, "decision", "shadow_matches").([]any)
	if len(sm) != 1 || get(sm[0].(map[string]any), "rule") != "trial-web" || get(c, "decision", "would_differ") != true {
		t.Fatalf("shadow = %v", sm)
	}
	rules := get(d, "rules").([]any)
	if r := rules[0].(map[string]any); r["rule"] != "trial-web" || r["mode"] != "shadow" || r["differed"] != float64(1) {
		t.Fatalf("rules[0] = %v", r)
	}

	// Human output: the calm shadow line and the rules table.
	e.t.Chdir(e.repo)
	exit, out, _ := run(t, "policy", "test")
	if exit != ExitOK {
		t.Fatalf("exit %d", exit)
	}
	for _, want := range []string{"shadow: trial-web would have quarantined this", "WOULD DIFFER", "Enforcing the shadow rules would change 2 of 4 commits."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A replay sizes binary files through git and shows the unscanned-binary
// floor, so a policy author sees which assets would stop at a person before
// the rules ever run live.
func TestPolicyTestShowsUnscannedBinaries(t *testing.T) {
	e := newFlow(t)
	e.write(e.repo, "p.yaml", "version: 1\nclassify:\n  area: { docs: [\"docs/**\"] }\n  risk: { high: { binary_bytes_over: 10 } }\nrules:\n  - { name: docs, when: { only_area: docs, risk: low }, action: auto_merge, reason: \"Docs.\" }\ndefault: { action: agent_review, reason: \"Fallback.\" }\n")
	e.git(e.repo, "add", "-A")
	e.git(e.repo, "commit", "--quiet", "-m", "chore: draft policy")
	e.write(e.repo, "docs/logo.png", "\x89PNG\x00\x01")
	e.git(e.repo, "add", "-A")
	e.git(e.repo, "commit", "--quiet", "-m", "docs: small logo")
	e.write(e.repo, "docs/notes.txt", "\x00api_key = hidden behind a NUL byte\n")
	e.git(e.repo, "add", "-A")
	e.git(e.repo, "commit", "--quiet", "-m", "docs: notes")
	e.write(e.repo, "docs/big.png", "\x89PNG\x00"+strings.Repeat("x", 20))
	e.git(e.repo, "add", "-A")
	e.git(e.repo, "commit", "--quiet", "-m", "docs: big logo")
	e.git(e.repo, "rm", "--quiet", "docs/notes.txt")
	e.git(e.repo, "commit", "--quiet", "-m", "docs: drop notes")

	d := e.do(e.repo, ExitOK, "policy", "test", "--file", "p.yaml", "-n", "4")
	commits := get(d, "commits").([]any)
	want := []struct {
		subject, action, rule string
		unscanned             []string
		bytes                 float64
	}{
		{"docs: drop notes", "auto_merge", "docs", nil, 0},
		{"docs: big logo", "agent_review", "default", []string{"docs/big.png"}, 25},
		{"docs: notes", "human_approve", "builtin:unscanned-binary", []string{"docs/notes.txt"}, 36},
		{"docs: small logo", "auto_merge", "docs", []string{"docs/logo.png"}, 6},
	}
	for i, w := range want {
		c := commits[i].(map[string]any)
		var unscanned []string
		for _, u := range get(c, "decision", "tags", "unscanned").([]any) {
			unscanned = append(unscanned, u.(string))
		}
		rule, _ := get(c, "decision", "rule", "name").(string)
		if get(c, "decision", "rule", "index") == float64(-1) {
			rule = "default"
		}
		if get(c, "subject") != w.subject || get(c, "decision", "action") != w.action || rule != w.rule ||
			strings.Join(unscanned, ",") != strings.Join(w.unscanned, ",") || get(c, "decision", "tags", "binary_bytes") != w.bytes {
			t.Fatalf("commit %d = %v, want %+v", i, c["decision"], w)
		}
	}
	rules := get(d, "rules").([]any)
	last := rules[len(rules)-1].(map[string]any)
	if last["rule"] != "builtin:unscanned-binary" || last["decided"] != float64(1) {
		t.Fatalf("rules = %v", rules)
	}
}

func TestPolicyTestErrors(t *testing.T) {
	e := newFlow(t)
	tests := []struct {
		name string
		args []string
		exit int
		code string
	}{
		{"no policy file", nil, ExitError, "policy_invalid"},
		{"bad limit", []string{"-n", "0"}, ExitUsage, "usage"},
		{"unknown revision", []string{"--file", "p.yaml", "--since", "nope"}, ExitError, CodeNotRepo},
	}
	e.write(e.repo, "p.yaml", trialPolicy)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.t.Chdir(e.repo)
			exit, stdout, _ := run(t, append([]string{"policy", "test", "--json"}, tt.args...)...)
			if exit != tt.exit || !strings.Contains(stdout, `"code":"`+tt.code+`"`) {
				t.Fatalf("exit %d, stdout %s", exit, stdout)
			}
		})
	}
}

func TestShadowLine(t *testing.T) {
	quarantine := policy.ShadowMatch{Rule: "rules[4]", Action: policy.Quarantine, Preempts: true}
	late := policy.ShadowMatch{Rule: "late", Action: policy.Block}
	tests := []struct {
		name    string
		matches []policy.ShadowMatch
		differ  bool
		want    string
	}{
		{"none", nil, false, ""},
		{"would differ", []policy.ShadowMatch{quarantine}, true, "shadow: rules[4] would have quarantined this"},
		{"first preempting one is named", []policy.ShadowMatch{late, quarantine}, true, "shadow: rules[4] would have quarantined this"},
		{"multi approve names the count", []policy.ShadowMatch{{Rule: "two", Action: policy.MultiApprove, Approvers: 3, Preempts: true}}, true, "shadow: two would have needed 3 approvers for this"},
		{"agrees", []policy.ShadowMatch{quarantine}, false, "shadow: rules[4] matched; enforcing it would not change this"},
		{"several agree", []policy.ShadowMatch{quarantine, late}, false, "shadow: 2 rules matched; enforcing them would not change this"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shadowLine(tt.matches, tt.differ); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// The Worker's shadow record wins over the local preview in human output.
func TestRenderProposePrefersWorkerShadow(t *testing.T) {
	r := proposeResult{
		IntentID: "i1", Commit: "abcdef1234567",
		Preview: policy.Decision{Action: policy.AutoMerge, ShadowMatches: []policy.ShadowMatch{}},
		Decision: &api.Decision{ID: "d1", Action: "auto_merge", Status: "approved", Detail: api.DecisionDetail{
			ShadowMatches: []policy.ShadowMatch{{Rule: "rules[4]", Action: policy.Quarantine, Preempts: true}}, WouldDiffer: true,
		}},
		Next: "Policy allows this change to land.",
	}
	var b strings.Builder
	if err := renderPropose(&b, styles{}, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "Shadow:       rules[4] would have quarantined this\n") {
		t.Fatalf("output:\n%s", b.String())
	}
}
