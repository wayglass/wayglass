package api

import (
	"strings"
	"testing"
)

func TestIdentity(t *testing.T) {
	def := DefaultAgent()
	if def == "" || strings.ContainsAny(def, " \t\n") {
		t.Fatalf("DefaultAgent() = %q", def)
	}
	tests := []struct {
		name  string
		agent string
		want  string
	}{
		{"stored or WAYGLASS_AGENT name wins", "agent-a", "agent-a"},
		{"space around the name is not a name", "  agent-b ", "agent-b"},
		{"no name falls back to user@host", "", def},
		{"blank falls back too", "   ", def},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Credentials{Agent: tt.agent}).Identity(); got != tt.want {
				t.Errorf("Identity() = %q, want %q", got, tt.want)
			}
		})
	}
}
