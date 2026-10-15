package policy

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// Vector is one case in testdata/policy-vectors.json. The Worker test suite
// runs the same file through the Wasm build, so both sides must agree.
type vector struct {
	Name    string      `json:"name"`
	Policy  *string     `json:"policy"` // key into policies; null means no policy file
	Request WireRequest `json:"request"`
	Expect  struct {
		Action      Action   `json:"action"`
		MatchedRule string   `json:"matched_rule"`
		Approvers   int      `json:"approvers"`
		Areas       []string `json:"areas"`
		DataClasses []string `json:"data_classes"`
		Risk        string   `json:"risk"`
		Shadow      []string `json:"shadow_matches"`
		WouldDiffer bool     `json:"would_differ"`
		Overrode    string   `json:"overrode"`
		// ReviewerDifferentFamily is the decision's reviewer constraint.
		ReviewerDifferentFamily bool `json:"reviewer_different_family"`
		// Unscanned is tags.unscanned; absent means none.
		Unscanned []string `json:"unscanned"`
		// ReviewerKind is the decision's reviewer kind ("" when none).
		ReviewerKind ReviewerKind `json:"reviewer_kind"`
		// AI is the decision's AI settings (nil when none).
		AI *AI `json:"ai"`
	} `json:"expect"`
}

type vectorFile struct {
	Policies map[string]string `json:"policies"`
	Vectors  []vector          `json:"vectors"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	b, err := os.ReadFile("../../testdata/policy-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var f vectorFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	return f
}

func TestSharedVectors(t *testing.T) {
	f := loadVectors(t)
	for _, v := range f.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			req := v.Request
			if v.Policy != nil {
				src, ok := f.Policies[*v.Policy]
				if !ok {
					t.Fatalf("unknown policy %q", *v.Policy)
				}
				req.PolicyYAML = src
			}
			in, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			// Go through the byte interface, exactly as the Wasm export does.
			var got WireResponse
			if err := json.Unmarshal(EvaluateJSON(in), &got); err != nil {
				t.Fatal(err)
			}
			e := v.Expect
			if got.Error != nil {
				t.Fatalf("error: %+v", got.Error)
			}
			if got.V != WireVersion {
				t.Errorf("v = %d", got.V)
			}
			if got.Action != e.Action || got.MatchedRule != e.MatchedRule || got.Approvers != e.Approvers {
				t.Errorf("decision = %s by %s (%d approvers), want %s by %s (%d)", got.Action, got.MatchedRule, got.Approvers, e.Action, e.MatchedRule, e.Approvers)
			}
			if !slices.Equal(got.Tags.Areas, e.Areas) || !slices.Equal(got.Tags.DataClasses, e.DataClasses) || got.Tags.Risk != e.Risk {
				t.Errorf("tags = %v %v %s, want %v %v %s", got.Tags.Areas, got.Tags.DataClasses, got.Tags.Risk, e.Areas, e.DataClasses, e.Risk)
			}
			var shadow []string
			for _, s := range got.ShadowMatches {
				shadow = append(shadow, s.Rule)
			}
			if !slices.Equal(shadow, e.Shadow) && len(shadow)+len(e.Shadow) > 0 {
				t.Errorf("shadow_matches = %v, want %v", shadow, e.Shadow)
			}
			if got.ShadowMatches == nil {
				t.Error("shadow_matches must be a list, not null")
			}
			if !slices.Equal(got.Tags.Unscanned, e.Unscanned) && len(got.Tags.Unscanned)+len(e.Unscanned) > 0 {
				t.Errorf("unscanned = %v, want %v", got.Tags.Unscanned, e.Unscanned)
			}
			if got.Tags.Unscanned == nil {
				t.Error("unscanned must be a list, not null")
			}
			if got.WouldDiffer != e.WouldDiffer {
				t.Errorf("would_differ = %v, want %v", got.WouldDiffer, e.WouldDiffer)
			}
			var overrode string
			if got.Overrode != nil {
				overrode = got.Overrode.ID
			}
			if overrode != e.Overrode {
				t.Errorf("overrode = %q, want %q", overrode, e.Overrode)
			}
			if df := got.Reviewer != nil && got.Reviewer.DifferentFamily; df != e.ReviewerDifferentFamily {
				t.Errorf("reviewer.different_family = %v, want %v", df, e.ReviewerDifferentFamily)
			}
			var kind ReviewerKind
			if got.Reviewer != nil {
				kind = got.Reviewer.Kind
			}
			if kind != e.ReviewerKind {
				t.Errorf("reviewer.kind = %q, want %q", kind, e.ReviewerKind)
			}
			if (got.AI == nil) != (e.AI == nil) || (got.AI != nil && *got.AI != *e.AI) {
				t.Errorf("ai = %+v, want %+v", got.AI, e.AI)
			}
			if got.Reason == "" {
				t.Error("decision has no reason")
			}
			// Evidence names the pattern and the line, never the text, so a
			// secret that triggered the class is not copied into the record.
			out := EvaluateJSON(in)
			for _, ch := range req.Changes {
				for _, line := range ch.AddedLines {
					if len(line) >= 8 && bytes.Contains(out, []byte(line)) {
						t.Errorf("response echoes an added line: %q", line)
					}
				}
			}
		})
	}
}

// The vectors embed the example policy so the Worker tests need no file
// access; this keeps that copy honest.
func TestVectorsExampleMatchesExamplesFile(t *testing.T) {
	f := loadVectors(t)
	b, err := os.ReadFile("../../examples/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if f.Policies["example"] != string(b) {
		t.Fatal("testdata/policy-vectors.json policies.example differs from examples/policy.yaml")
	}
}

func TestEvaluateJSONBadRequest(t *testing.T) {
	var got WireResponse
	if err := json.Unmarshal(EvaluateJSON([]byte("{not json")), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || got.Error.Code != "bad_request" {
		t.Fatalf("error = %+v", got.Error)
	}
}

func TestInvalidPolicyCarriesItsError(t *testing.T) {
	got := EvaluateWire(WireRequest{PolicyYAML: "version: 2\n", Changes: []WireChange{{Path: "a"}}})
	if got.MatchedRule != BuiltinInvalidPolicy || got.PolicyError == "" {
		t.Fatalf("got %s / %q", got.MatchedRule, got.PolicyError)
	}
}
