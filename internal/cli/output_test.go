package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
)

// TestReportExitCodes pins the exit code and envelope code every kind of
// error gets, whatever command produced it. Agents branch on these.
func TestReportExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
		exit int
	}{
		{"usage", UsageError("bad flag"), CodeUsage, ExitUsage},
		{"plain error", errors.New("boom"), CodeError, ExitError},
		{"canceled", context.Canceled, CodeCanceled, ExitError},
		{"worker conflict keeps its code", &api.Error{Status: 409, Code: api.CodeConflict, Message: "busy"}, api.CodeConflict, ExitError},
		{"worker needs_human is exit 4", &api.Error{Status: 403, Code: api.CodeNeedsHuman, Message: "a person decides"}, api.CodeNeedsHuman, ExitNeedsHuman},
		{"wrapped needs_human is exit 4", fmt.Errorf("deciding: %w", &api.Error{Status: 403, Code: api.CodeNeedsHuman, Message: "a person decides"}), api.CodeNeedsHuman, ExitNeedsHuman},
		{"an Error never exits 0", &Error{Code: "x", Message: "built without an exit code"}, "x", ExitError},
		{"blocked keeps exit 3", &Error{Code: "blocked", Message: "policy", Exit: ExitBlocked}, "blocked", ExitBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := report(tc.err, true, &stdout, &stderr); got != tc.exit {
				t.Errorf("json exit = %d, want %d", got, tc.exit)
			}
			env := checkEnvelope(t, stdout.String())
			if env.Error.Code != tc.code {
				t.Errorf("json code = %q, want %q", env.Error.Code, tc.code)
			}
			if stderr.Len() != 0 {
				t.Errorf("--json wrote to stderr: %q", stderr.String())
			}
			stdout.Reset()
			if got := report(tc.err, false, &stdout, &stderr); got != tc.exit {
				t.Errorf("human exit = %d, want %d", got, tc.exit)
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.err.Error()) {
				t.Errorf("human mode: stdout %q, stderr %q", stdout.String(), stderr.String())
			}
		})
	}
}
