package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// goldenCases runs every leaf command with --json. TestGoldenCoversEveryCommand
// fails if a command is added without a case here.
var goldenCases = []struct {
	args []string
	ok   bool
	code string // error code when !ok
	exit int
}{
	{[]string{"version"}, true, "", ExitOK},
	{[]string{"init", "--project", "demo"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"login"}, false, CodeUsage, ExitUsage},
	{[]string{"intent", "declare", "--goal", "g", "--reason", "r", "--path", "docs/**"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"intent", "list"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"intent", "show", "i-1"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"intent", "abandon", "i-1", "--reason", "superseded"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"intent", "restack", "i-1", "--on", "i-2", "--reason", "both edit login"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"push"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"sync"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"overlaps"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"propose", "i-1"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"review"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"approve", "i-1", "--reason", "looks right"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"reject", "i-1", "--reason", "wrong area"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"ai-review", "i-1"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"ai-advice", "i-1"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"land", "i-1"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"publish"}, false, CodeNotRepo, ExitError},
	{[]string{"clone", "demo", "golden-clone-target-that-does-not-exist"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"diff", "i-1"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"git-credential", "--project", "demo", "get"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"why", "web/login.ts:42"}, false, CodeNotRepo, ExitError},
	{[]string{"policy", "test"}, false, CodeNotRepo, ExitError},
	{[]string{"log"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"watch"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"session", "share"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"session", "fork", "https://api.wayglass.dev/v1/projects/demo/sessions/s0123456789"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"session", "show"}, false, CodeNotRepo, ExitError},
	{[]string{"gen-docs"}, false, CodeUsage, ExitUsage},
	{[]string{"keys", "create", "--agent", "ci-bot"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"keys", "list"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"keys", "revoke", "k0123456789"}, false, CodeNotLoggedIn, ExitError},
	// doctor reports every check in data, and exits 1 when one failed.
	{[]string{"doctor"}, true, "", ExitError},
	{[]string{"agent", "setup"}, false, CodeNotRepo, ExitError},
	{[]string{"project", "delete", "demo-test-1", "--confirm", "demo-test-1"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"project", "migrate-residency", "eu-demo-1"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"project", "list"}, false, CodeNotLoggedIn, ExitError},
	{[]string{"projects"}, false, CodeNotLoggedIn, ExitError},
}

// usageCases check that bad input is a usage error (exit 2) in the envelope.
var usageCases = [][]string{
	{},
	{"intent"},
	{"session"},
	{"keys"},
	{"keys", "revoke"},
	{"project"},
	{"project", "delete"},
	{"project", "delete", "demo-test-1"},
	{"project", "delete", "demo-1", "--confirm", "demo-1"},
	{"project", "delete", "demo-test-1", "--confirm", "demo-test-2"},
	{"project", "migrate-residency"},
	{"doctor", "extra"},
	{"git-credential"},
	{"git-credential", "get"},
	{"git-credential", "--project", "demo", "fill"},
	{"git-credential", "--project", "Bad--Name", "get"},
	{"git-credential", "--project", "demo", "--intent", "nope", "get"},
	{"clone", "demo", "--intent", "nope"},
	{"agent"},
	{"agent", "setup", "extra"},
	{"policy"},
	{"policy", "test", "extra"},
	{"policy", "test", "--limit", "0"},
	{"nope"},
	{"intent", "nope"},
	{"version", "extra"},
	{"intent", "show"},
	{"intent", "abandon", "i-1"},
	{"intent", "abandon", "i-1", "--reason", "TODO"},
	{"intent", "abandon", "--reason", "superseded"},
	{"intent", "restack", "i-1", "--reason", "both edit login"},
	{"intent", "restack", "i-1", "--on", "i-2"},
	{"intent", "restack", "i-1", "--on", "i-1", "--reason", "both edit login"},
	{"intent", "restack", "i-1", "--on", "i-2", "--reason", "<why>"},
	{"intent", "restack", "i-1", "--on", "i-2", "--reason", "LGTM!"},
	{"intent", "restack", "--on", "i-2", "--reason", "both edit login"},
	{"intent", "declare", "--goal", "g", "--path", "x"},
	{"intent", "declare", "--reason", "r", "--path", "x"},
	{"intent", "declare", "--goal", "g", "--reason", "r"},
	{"approve", "i-1"},
	{"approve", "i-1", "--reason", "checked it", "--family", "mistral"},
	{"intent", "declare", "--goal", "g", "--reason", "r", "--path", "x", "--family", "mistral"},
	{"approve", "i-1", "--reason", "<what you checked>"},
	{"reject", "i-1", "--reason", "TODO"},
	{"reject"},
	{"diff", "i-1", "extra"},
	{"why", "web/login.ts"},
	{"why", "web/login.ts:0"},
	{"session", "fork"},
	{"clone"},
	{"clone", "demo", "dir", "extra"},
	{"clone", "Not--A-Project"},
	{"session", "fork", "https://wayglass.dev/s/abc"},
	{"session", "share", "extra"},
	{"session", "show", "not-a-url"},
	{"log", "--limit", "many"},
	{"review", "--for", "robots"},
	{"watch", "--bogus"},
}

type rawEnvelope struct {
	V     *int             `json:"v"`
	OK    *bool            `json:"ok"`
	Data  *json.RawMessage `json:"data"`
	Error *struct {
		Code    string           `json:"code"`
		Message string           `json:"message"`
		Details *json.RawMessage `json:"details"`
	} `json:"error"`
}

// hermetic runs the test outside any git repo with no login, so commands
// that need the Worker or a checkout fail the same way on every machine.
func hermetic(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("WAYGLASS_CONFIG_DIR", t.TempDir())
	for _, k := range []string{"WAYGLASS_URL", "WAYGLASS_KEY", "WAYGLASS_AGENT", "WAYGLASS_AGENT_FAMILY", EnvProject, EnvIntent, EnvForkToken} {
		t.Setenv(k, "")
	}
	// Stop git from finding a repository above the temp dir.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), BuildInfo{Version: "test", Commit: "abc"}, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// checkEnvelope asserts stdout is exactly one envelope and returns it.
func checkEnvelope(t *testing.T, stdout string) rawEnvelope {
	t.Helper()
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Fatalf("want exactly one JSON line, got %q", stdout)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	var env rawEnvelope
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("stdout is not an envelope: %v: %q", err, stdout)
	}
	if env.V == nil || *env.V != EnvelopeVersion {
		t.Fatalf("v missing or wrong: %q", stdout)
	}
	if env.OK == nil {
		t.Fatalf("ok missing: %q", stdout)
	}
	if *env.OK {
		if env.Data == nil || env.Error != nil {
			t.Fatalf("ok:true needs data and no error: %q", stdout)
		}
	} else {
		if env.Error == nil || env.Data != nil {
			t.Fatalf("ok:false needs error and no data: %q", stdout)
		}
		if env.Error.Code == "" || env.Error.Message == "" {
			t.Fatalf("error needs code and message: %q", stdout)
		}
	}
	return env
}

func TestGoldenJSONEnvelope(t *testing.T) {
	hermetic(t)
	for _, tc := range goldenCases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			exit, stdout, stderr := run(t, append(slices.Clone(tc.args), "--json")...)
			env := checkEnvelope(t, stdout)
			if exit != tc.exit {
				t.Errorf("exit = %d, want %d", exit, tc.exit)
			}
			if *env.OK != tc.ok {
				t.Errorf("ok = %v, want %v", *env.OK, tc.ok)
			}
			if !tc.ok && env.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.code)
			}
			if stderr != "" {
				t.Errorf("--json must keep stderr quiet, got %q", stderr)
			}
			if strings.Contains(stdout, "\x1b[") {
				t.Errorf("--json output contains ANSI escapes: %q", stdout)
			}
		})
	}
}

func TestUsageErrorsInJSON(t *testing.T) {
	hermetic(t)
	for _, args := range usageCases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			// --json first, so flag errors after it still yield JSON.
			exit, stdout, stderr := run(t, append([]string{"--json"}, args...)...)
			env := checkEnvelope(t, stdout)
			if exit != ExitUsage || *env.OK || env.Error.Code != CodeUsage {
				t.Fatalf("exit=%d env=%s, want usage error exit 2", exit, stdout)
			}
			if stderr != "" {
				t.Errorf("stderr = %q", stderr)
			}
		})
	}
}

func TestJSONFlagAfterBadFlagStillJSON(t *testing.T) {
	hermetic(t)
	exit, stdout, _ := run(t, "watch", "--bogus", "--json")
	env := checkEnvelope(t, stdout)
	if exit != ExitUsage || env.Error.Code != CodeUsage {
		t.Fatalf("exit=%d env=%s", exit, stdout)
	}
}

func TestVersionData(t *testing.T) {
	_, stdout, _ := run(t, "version", "--json")
	var env struct {
		Data BuildInfo `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Version != "test" || env.Data.Commit != "abc" || !strings.HasPrefix(env.Data.Go, "go") {
		t.Fatalf("version data = %+v", env.Data)
	}
}

func TestHumanModeWritesErrorsToStderr(t *testing.T) {
	hermetic(t)
	exit, stdout, stderr := run(t, "log")
	if exit != ExitError || stdout != "" {
		t.Fatalf("exit=%d stdout=%q", exit, stdout)
	}
	if !strings.Contains(stderr, "not logged in") {
		t.Fatalf("stderr = %q", stderr)
	}
	if strings.Contains(stderr, "\x1b[") {
		t.Fatalf("non-TTY stderr must be plain, got %q", stderr)
	}
}

func TestHumanUsageExitCode(t *testing.T) {
	hermetic(t)
	exit, _, stderr := run(t, "intent", "show")
	if exit != ExitUsage || !strings.Contains(stderr, "--help") {
		t.Fatalf("exit=%d stderr=%q", exit, stderr)
	}
}

func TestNoArgsShowsHelpForPeople(t *testing.T) {
	exit, stdout, _ := run(t)
	if exit != ExitOK || !strings.Contains(stdout, "Usage:") {
		t.Fatalf("exit=%d stdout=%q", exit, stdout)
	}
}

func TestWithExitDoesNotPrint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := report(WithExit(ExitBlocked), true, &stdout, &stderr); got != ExitBlocked {
		t.Fatalf("exit = %d", got)
	}
	if stdout.Len()+stderr.Len() != 0 {
		t.Fatalf("WithExit printed %q %q", stdout.String(), stderr.String())
	}
}

func TestGoldenCoversEveryCommand(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range goldenCases {
		var words []string
		for _, a := range tc.args {
			if strings.HasPrefix(a, "-") {
				break
			}
			words = append(words, a)
		}
		// Command paths are at most two words deep today.
		for n := 1; n <= min(2, len(words)); n++ {
			covered[strings.Join(words[:n], " ")] = true
		}
	}
	var walk func(c *cobra.Command, prefix string)
	walk = func(c *cobra.Command, prefix string) {
		for _, sub := range c.Commands() {
			if sub.Name() == "help" || sub.Name() == "completion" {
				continue
			}
			p := strings.TrimSpace(prefix + " " + sub.Name())
			if sub.HasSubCommands() {
				walk(sub, p)
				continue
			}
			if !covered[p] {
				t.Errorf("command %q has no golden --json case", p)
			}
		}
	}
	walk(NewRoot(BuildInfo{}), "")
}

func TestParseFileLine(t *testing.T) {
	tests := []struct {
		in   string
		file string
		line int
		ok   bool
	}{
		{"web/login.ts:42", "web/login.ts", 42, true},
		{"c:/x.go:7", "c:/x.go", 7, true},
		{"a.go", "", 0, false},
		{"a.go:", "", 0, false},
		{":3", "", 0, false},
		{"a.go:-1", "", 0, false},
		{"a.go:x", "", 0, false},
	}
	for _, tt := range tests {
		f, l, err := parseFileLine(tt.in)
		if (err == nil) != tt.ok || f != tt.file || l != tt.line {
			t.Errorf("parseFileLine(%q) = %q, %d, %v", tt.in, f, l, err)
		}
	}
}
