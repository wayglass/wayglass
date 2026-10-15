package api

import "testing"

func TestAIProviderNames(t *testing.T) {
	for _, tt := range []struct{ by, model string }{
		{"workers-ai:@cf/meta/llama", "@cf/meta/llama"},
		{"anthropic:claude-sonnet-5.5", "claude-sonnet-5.5"},
		{"openai:gpt-5.4-mini", "gpt-5.4-mini"},
		{"reviewer-agent", ""}, {"openai:", ""},
	} {
		t.Run(tt.by, func(t *testing.T) {
			if got := AIModel(tt.by); got != tt.model {
				t.Fatalf("model = %q, want %q", got, tt.model)
			}
			if IsAIReviewer(tt.by) != (tt.model != "") {
				t.Fatal("AI identity mismatch")
			}
		})
	}
}
