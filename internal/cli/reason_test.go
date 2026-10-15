package cli

import "testing"

func TestFillerReason(t *testing.T) {
	for in, want := range map[string]bool{
		"LGTM":              true,
		"lgtm!":             true,
		"Looks good to me.": true,
		"ok":                true,
		"+1":                true,
		"👍":                 true,
		"Approved":          true,
		"ship it 🚀":         true,
		"yes, lgtm":         true,
		"no":                true,
		"LGTM: checked the lockout path against the intent": false,
		"Lockout window is fine":                            false,
		"Checked the login flow against the intent":         false,
		"okay to merge after the schema check passed":       false,
	} {
		if got := fillerReason(in); got != want {
			t.Errorf("fillerReason(%q) = %v, want %v", in, got, want)
		}
	}
}
