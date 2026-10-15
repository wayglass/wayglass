package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
)

// runStdin runs wayglass with stdin, as git runs a credential helper.
func runStdin(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	root := NewRoot(BuildInfo{Version: "test"})
	root.SetArgs(args)
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(t.Context())
	if err == nil {
		return ExitOK, stdout.String(), stderr.String()
	}
	return report(err, false, &stdout, &stderr), stdout.String(), stderr.String()
}

// The helper speaks git's credential protocol against the fake Worker: get
// mints a fresh read token for the trunk or an intent fork and answers on
// stdout; store and erase keep nothing; a request for another host, or a
// bad line, fails on stderr so git stops cleanly.
func TestGitCredentialProtocol(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	s.change("agent-a", s.a, map[string]string{"docs/a.md": "a\n"}, ExitOK)
	f := s.e.worker
	// A failed get answers quit=1 so git stops instead of prompting for a
	// password that cannot exist.
	wantQuit := func(t *testing.T, out string) {
		if out != "quit=1\n" {
			t.Fatalf("stdout = %q, want quit=1", out)
		}
	}
	for _, tc := range []struct {
		name      string
		stdin     string
		args      []string
		env       map[string]string
		wantExit  int
		wantTrunk int // trunk read tokens minted
		wantFork  int // intent read tokens minted
		wantOut   func(t *testing.T, out string)
		wantErr   string
	}{
		{
			name:  "store keeps nothing",
			stdin: "protocol=https\nhost=x\nusername=u\npassword=p\n\n",
			args:  []string{"git-credential", "--project", "demo", "store"},
		},
		{
			name:  "erase keeps nothing",
			stdin: "protocol=https\nhost=x\n\n",
			args:  []string{"git-credential", "--project", "demo", "erase"},
		},
		{
			name:      "get mints a trunk read token",
			stdin:     "\n",
			args:      []string{"git-credential", "--project", "demo", "get"},
			wantTrunk: 1,
			wantOut: func(t *testing.T, out string) {
				tok := f.tokens[len(f.tokens)-1]
				want := "username=" + credentialUsername + "\npassword=" + tok + "\n"
				if !strings.HasPrefix(out, want) || !strings.Contains(out, "password_expiry_utc=") {
					t.Fatalf("stdout = %q, want prefix %q with an expiry", out, want)
				}
			},
		},
		{
			name:     "get --intent mints a fork read token",
			stdin:    "",
			args:     []string{"git-credential", "--project", "demo", "--intent", s.idA, "get"},
			wantFork: 1,
			wantOut: func(t *testing.T, out string) {
				if !strings.Contains(out, "password="+f.tokens[len(f.tokens)-1]+"\n") {
					t.Fatalf("stdout = %q", out)
				}
			},
		},
		{
			name:      "lines git 2.41+ adds are ignored",
			stdin:     "capability[]=authtype\nwwwauth[]=Basic realm=\"Artifacts\"\nusername=wayglass\n\n",
			args:      []string{"git-credential", "--project", "demo", "get"},
			wantTrunk: 1,
			wantOut: func(t *testing.T, out string) {
				if !strings.Contains(out, "password="+f.tokens[len(f.tokens)-1]+"\n") {
					t.Fatalf("stdout = %q", out)
				}
			},
		},
		{
			name:      "parsing stops at the first blank line",
			stdin:     "\nhost=evil.example\n",
			args:      []string{"git-credential", "--project", "demo", "get"},
			wantTrunk: 1,
			wantOut: func(t *testing.T, out string) {
				if !strings.Contains(out, "password="+f.tokens[len(f.tokens)-1]+"\n") {
					t.Fatalf("stdout = %q", out)
				}
			},
		},
		{
			name:      "a request for another host gets no token",
			stdin:     "host=evil.example\n\n", // the fake remotes are local paths: no protocol
			args:      []string{"git-credential", "--project", "demo", "get"},
			wantExit:  ExitError,
			wantTrunk: 1, // minted, then refused; the mint is audited anyway
			wantOut:   wantQuit,
			wantErr:   "evil.example",
		},
		{
			name:     "unknown intent",
			stdin:    "",
			args:     []string{"git-credential", "--project", "demo", "--intent", "i0000000000", "get"},
			wantExit: ExitError,
			wantOut:  wantQuit,
			wantErr:  "not found",
		},
		{
			name:     "unknown project",
			stdin:    "",
			args:     []string{"git-credential", "--project", "nope", "get"},
			wantExit: ExitError,
			wantOut:  wantQuit,
			wantErr:  "nope",
		},
		{
			name:     "a revoked or expired key mints nothing",
			stdin:    "",
			args:     []string{"git-credential", "--project", "demo", "get"},
			env:      map[string]string{"WAYGLASS_KEY": "not-the-key"},
			wantExit: ExitError,
			wantOut:  wantQuit,
			wantErr:  "wayglass login",
		},
		{
			name:     "the Worker is down",
			stdin:    "",
			args:     []string{"git-credential", "--project", "demo", "get"},
			env:      map[string]string{"WAYGLASS_URL": "http://127.0.0.1:1"},
			wantExit: ExitError,
			wantOut:  wantQuit,
			wantErr:  "127.0.0.1:1",
		},
		{
			name:     "a line without = is refused",
			stdin:    "garbage\n",
			args:     []string{"git-credential", "--project", "demo", "get"},
			wantExit: ExitUsage,
			wantErr:  "garbage",
		},
		{
			name:     "an unknown operation is refused before reading stdin",
			stdin:    "protocol=https\nhost=x\n\n",
			args:     []string{"git-credential", "--project", "demo", "fill"},
			wantExit: ExitUsage,
			wantErr:  "fill",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			trunkBefore, forkBefore, tokensBefore := f.trunkReadTokens, f.readTokens, len(f.tokens)
			exit, out, errOut := runStdin(t, tc.stdin, tc.args...)
			if exit != tc.wantExit {
				t.Fatalf("exit %d, want %d; stdout %q stderr %q", exit, tc.wantExit, out, errOut)
			}
			if got := f.trunkReadTokens - trunkBefore; got != tc.wantTrunk {
				t.Fatalf("minted %d trunk tokens, want %d", got, tc.wantTrunk)
			}
			if got := f.readTokens - forkBefore; got != tc.wantFork {
				t.Fatalf("minted %d fork tokens, want %d", got, tc.wantFork)
			}
			if tc.wantOut != nil {
				tc.wantOut(t, out)
			} else if out != "" {
				t.Fatalf("stdout = %q, want nothing", out)
			}
			if tc.wantErr == "" && errOut != "" {
				t.Fatalf("stderr = %q", errOut)
			}
			if !strings.Contains(errOut, tc.wantErr) {
				t.Fatalf("stderr = %q, want %q", errOut, tc.wantErr)
			}
			for _, tok := range f.tokens[tokensBefore:] {
				base, _, _ := strings.Cut(tok, "?")
				if strings.Contains(errOut, base) {
					t.Fatal("stderr leaked a token")
				}
			}
		})
	}
}

// readGitCredential takes git's key=value lines as git writes them (checked
// against git 2.55: protocol and host, path only with credential.useHttpPath,
// capability[] and wwwauth[] lines on newer versions) and nothing else.
func TestReadGitCredential(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		want  gitCredential
		isErr bool
	}{
		{"nothing", "", gitCredential{}, false},
		{"blank line only", "\n", gitCredential{}, false},
		{"what git sends by default", "protocol=https\nhost=acct.artifacts.cloudflare.net\n\n", gitCredential{Protocol: "https", Host: "acct.artifacts.cloudflare.net"}, false},
		{"with useHttpPath", "protocol=https\nhost=h\npath=git/wayglass/demo.git\n\n", gitCredential{Protocol: "https", Host: "h", Path: "git/wayglass/demo.git"}, false},
		{"no trailing blank line", "protocol=https\nhost=h", gitCredential{Protocol: "https", Host: "h"}, false},
		{"lines after the blank line are not read", "host=h\n\nhost=evil\n", gitCredential{Host: "h"}, false},
		{"unknown keys are ignored", "capability[]=authtype\nwwwauth[]=Basic realm=\"x\"\nusername=u\npassword=p\nhost=h\n\n", gitCredential{Host: "h"}, false},
		{"a value may contain =", "path=a=b\n\n", gitCredential{Path: "a=b"}, false},
		{"an empty value is allowed", "host=\npath=\n\n", gitCredential{}, false},
		{"the last value wins", "host=a\nhost=b\n\n", gitCredential{Host: "b"}, false},
		{"a line without = is an error", "protocol=https\ngarbage\n\n", gitCredential{}, true},
		{"a line over the scanner limit is an error", "host=" + strings.Repeat("x", 70<<10) + "\n\n", gitCredential{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readGitCredential(strings.NewReader(tc.in))
			if (err != nil) != tc.isErr {
				t.Fatalf("err = %v, want error=%v", err, tc.isErr)
			}
			if err == nil && *got != tc.want {
				t.Fatalf("got %+v, want %+v", *got, tc.want)
			}
		})
	}
}

// The host decides when git sends no path (its default); with a path only
// this repo and URLs below it pass, so a sibling repo, a different port or
// a path that merely starts with the same bytes never gets the token.
func TestGitCredentialMatches(t *testing.T) {
	const remote = "https://acct.artifacts.cloudflare.net/git/wayglass/demo.git"
	const host = "acct.artifacts.cloudflare.net"
	for _, tc := range []struct {
		name string
		in   gitCredential
		ok   bool
	}{
		{"empty request", gitCredential{}, true},
		{"host only", gitCredential{Protocol: "https", Host: host}, true},
		{"host case-insensitive", gitCredential{Host: "ACCT.artifacts.cloudflare.net"}, true},
		{"default port spelled out", gitCredential{Protocol: "https", Host: host + ":443"}, true},
		{"another port", gitCredential{Protocol: "https", Host: host + ":8443"}, false},
		{"other host", gitCredential{Host: "evil.example"}, false},
		{"host with a suffix", gitCredential{Host: host + ".evil.example"}, false},
		{"host with a prefix", gitCredential{Host: "evil." + host}, false},
		{"http not https", gitCredential{Protocol: "http", Host: host}, false},
		{"http on port 80", gitCredential{Protocol: "http", Host: host + ":80"}, false},
		{"path of this repo", gitCredential{Host: host, Path: "git/wayglass/demo.git"}, true},
		{"path with a leading slash", gitCredential{Host: host, Path: "/git/wayglass/demo.git"}, true},
		{"path below this repo", gitCredential{Host: host, Path: "git/wayglass/demo.git/info/refs"}, true},
		{"path with a trailing slash", gitCredential{Host: host, Path: "git/wayglass/demo.git/"}, true},
		{"other repo", gitCredential{Host: host, Path: "git/wayglass/other.git"}, false},
		{"same bytes then more", gitCredential{Host: host, Path: "git/wayglass/demo.gitx"}, false},
		{"same bytes then a dot", gitCredential{Host: host, Path: "git/wayglass/demo.git.evil"}, false},
		{"without .git", gitCredential{Host: host, Path: "git/wayglass/demo"}, false},
		{"path case differs", gitCredential{Host: host, Path: "git/wayglass/DEMO.git"}, false},
		{"other namespace", gitCredential{Host: host, Path: "git/wayglass-eu/demo.git"}, false},
		{"traversal below the repo", gitCredential{Host: host, Path: "git/wayglass/demo.git/../other.git"}, false},
		{"traversal above the repo", gitCredential{Host: host, Path: "git/wayglass/../wayglass/demo.git"}, false},
		{"a dot segment", gitCredential{Host: host, Path: "git/wayglass/./demo.git"}, false},
		{"percent-encoded", gitCredential{Host: host, Path: "git/wayglass/demo%2Egit"}, false},
		{"path alone", gitCredential{Path: "git/wayglass/other.git"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.in.matches(remote); (err == nil) != tc.ok {
				t.Fatalf("matches = %v, want ok=%v", err, tc.ok)
			}
		})
	}
	t.Run("remote with a trailing slash", func(t *testing.T) {
		in := gitCredential{Host: host, Path: "git/wayglass/demo.git"}
		if err := in.matches(remote + "/"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("remote that does not parse", func(t *testing.T) {
		if err := (&gitCredential{}).matches("https://acct.artifacts.cloudflare.net/%zz"); err == nil {
			t.Fatal("want an error")
		}
	})
}

func TestCredentialHelperValue(t *testing.T) {
	for _, tc := range []struct {
		exe, project, intent, want string
	}{
		{"wayglass", "demo", "", "!wayglass git-credential --project demo"},
		{"/usr/local/bin/wayglass", "demo", "i0123456789", "!/usr/local/bin/wayglass git-credential --project demo --intent i0123456789"},
		{"/Users/me/My Tools/wayglass", "demo", "", "!'/Users/me/My Tools/wayglass' git-credential --project demo"},
		{"/odd/it's/wayglass", "demo", "", `!'/odd/it'\''s/wayglass' git-credential --project demo`},
		{"C:/Users/me/bin/wayglass.exe", "demo", "", "!C:/Users/me/bin/wayglass.exe git-credential --project demo"},
		{"C:/Program Files/wayglass/wayglass.exe", "demo", "", "!'C:/Program Files/wayglass/wayglass.exe' git-credential --project demo"},
		{"/tmp/$HOME/`id`/wayglass", "demo", "", "!'/tmp/$HOME/`id`/wayglass' git-credential --project demo"},
	} {
		got := credentialHelperValue(tc.exe, tc.project, tc.intent)
		if got != tc.want {
			t.Errorf("credentialHelperValue(%q) = %q, want %q", tc.exe, got, tc.want)
		}
		if tc.exe != "/odd/it's/wayglass" { // helperCommand reads the first quoted word only
			if back := helperCommand(got); back != tc.exe {
				t.Errorf("helperCommand(%q) = %q, want %q", got, back, tc.exe)
			}
		}
	}
	if helperCommand("osxkeychain") != "" || helperCommand("!") != "" {
		t.Error("non-shell helpers have no command")
	}
}

func TestTokenExpiryUnix(t *testing.T) {
	for in, want := range map[string]int64{
		"art_v2_a_" + strings.Repeat("0", 40) + "?expires=1791990000": 1791990000,
		"art_v1_" + strings.Repeat("0", 40):                           0,
		"x?expires=nope":                                              0,
	} {
		if got := tokenExpiryUnix(api.Token{Value: in}); got != want {
			t.Errorf("tokenExpiryUnix(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestWriteGitCredentialRefusesControlCharacters(t *testing.T) {
	var out strings.Builder
	if err := writeGitCredential(&out, api.Token{Value: "art_v1_x\nusername=evil"}); err == nil || out.Len() != 0 {
		t.Fatalf("err = %v, out = %q", err, out.String())
	}
}

var (
	builtOnce sync.Once
	builtBin  string
	builtErr  error
)

// builtWayglass builds the real binary once per test run (cached by go build).
func builtWayglass(t *testing.T) string {
	t.Helper()
	builtOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			builtErr = err
			return
		}
		dir, err := os.MkdirTemp("", "wayglass-test-bin")
		if err != nil {
			builtErr = err
			return
		}
		builtBin = filepath.Join(dir, "wayglass")
		cmd := exec.Command("go", "build", "-o", builtBin, "github.com/wayglass/wayglass/cmd/wayglass")
		if out, err := cmd.CombinedOutput(); err != nil {
			builtErr = fmt.Errorf("go build: %w: %s", err, out)
		}
	})
	if builtErr != nil {
		t.Skipf("no wayglass binary: %v", builtErr)
	}
	return builtBin
}

// A clone configures git so the helper, and only the helper, answers for
// origin; the configured command really runs through sh as git would run it.
func TestCloneConfiguresCredentialHelper(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	s.change("agent-a", s.a, map[string]string{"docs/a.md": "a\n"}, ExitOK)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, _ = filepath.EvalSymlinks(self)

	for _, tc := range []struct {
		name  string
		args  []string
		dir   string
		trunk bool
	}{
		{"trunk", []string{"clone", "demo"}, "demo", true},
		{"intent fork", []string{"clone", "demo", "--intent", s.idA}, "demo-" + s.idA, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			got := s.e.do(parent, ExitOK, tc.args...)
			dir := filepath.Join(parent, tc.dir)
			if get(got, "dir") != dir || get(got, "no_token_on_disk") != true {
				t.Fatalf("clone = %v", got)
			}
			remote := get(got, "remote").(string)
			helper := get(got, "credential_helper").(string)
			wantSuffix := " git-credential --project demo"
			if tc.trunk {
				if get(got, "trunk") != "demo" || get(got, "intent") != "" || remote != s.trunk {
					t.Fatalf("clone = %v", got)
				}
			} else {
				wantSuffix += " --intent " + s.idA
				it := s.e.do(s.a, ExitOK, "intent", "show", s.idA)
				if get(got, "intent") != s.idA || get(got, "trunk") != "" || get(got, "fork") != get(it, "intent", "fork_repo") || remote != get(it, "intent", "fork_remote") {
					t.Fatalf("clone = %v, intent = %v", got, it)
				}
				if get(got, "head") != get(it, "intent", "head_commit") {
					t.Fatalf("head %v, want the fork head %v", get(got, "head"), get(it, "intent", "head_commit"))
				}
			}
			if !strings.HasSuffix(helper, wantSuffix) || helperCommand(helper) != self {
				t.Fatalf("credential_helper = %q, want this binary + %q", helper, wantSuffix)
			}
			helpers := s.e.git(dir, "config", "--local", "--get-all", "credential."+remote+".helper")
			if helpers != "\n"+helper {
				t.Fatalf("configured helpers = %q, want a reset then %q", helpers, helper)
			}
			for _, line := range strings.Split(s.e.git(dir, "config", "--local", "--list"), "\n") {
				if strings.HasPrefix(line, "credential.helper=") {
					t.Fatalf("helper leaked to every remote: %s", line)
				}
			}
			assertNoTokens(t, dir, s.e.worker.tokens)

			// Run it the way git does: through sh, with the operation appended.
			// A real wayglass binary stands in for this test binary, which
			// would otherwise rerun the test suite.
			real := credentialHelperValue(builtWayglass(t), "demo", get(got, "intent").(string))
			before := len(s.e.worker.tokens)
			cmd := exec.CommandContext(t.Context(), "sh", "-c", strings.TrimPrefix(real, "!")+" get")
			cmd.Dir = dir
			cmd.Stdin = strings.NewReader("\n")
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("helper via sh: %v", err)
			}
			if len(s.e.worker.tokens) != before+1 || !strings.Contains(string(out), "password="+s.e.worker.tokens[before]+"\n") {
				t.Fatalf("helper answered %q", out)
			}

			// With a bad key the real binary tells git to quit, explains on
			// stderr and mints nothing; the token never reaches argv.
			cmd = exec.CommandContext(t.Context(), "sh", "-c", strings.TrimPrefix(real, "!")+" get")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "WAYGLASS_KEY=not-the-key")
			cmd.Stdin = strings.NewReader("protocol=https\nhost=x\n\n")
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err = cmd.Output()
			if err == nil || string(out) != "quit=1\n" || !strings.Contains(stderr.String(), "wayglass login") || len(s.e.worker.tokens) != before+1 {
				t.Fatalf("helper with a bad key: err %v, stdout %q, stderr %q", err, out, stderr.String())
			}
			if strings.Contains(real, s.e.worker.tokens[before]) {
				t.Fatal("the helper command line holds a token")
			}

			// A plain fetch in the checkout works (no helper needed for a
			// local remote; this checks the checkout itself is sound).
			s.e.git(dir, "fetch", "--quiet", "origin")
		})
	}
}

func TestDoctorReportsCredentialHelper(t *testing.T) {
	s := newTwoAgents(t, []string{"docs/**"}, []string{"docs/**"})
	parent := t.TempDir()
	got := s.e.do(parent, ExitOK, "clone", "demo")
	dir := get(got, "dir").(string)

	checks := checksByName(t, s.e.do(dir, ExitOK, "doctor"))
	if c := checks["helper"]; c["status"] != checkOK || !strings.Contains(c["detail"].(string), "mints read-only tokens") {
		t.Fatalf("helper check = %v", c)
	}

	// The binary moved away: doctor warns but stays healthy.
	s.e.git(dir, "config", "--local", "--replace-all", "credential."+s.trunk+".helper", "!/nowhere/wayglass git-credential --project demo", "git-credential")
	d := s.e.do(dir, ExitOK, "doctor")
	if c := checksByName(t, d)["helper"]; c["status"] != checkWarn || !strings.Contains(c["detail"].(string), "/nowhere/wayglass") || get(d, "healthy") != true {
		t.Fatalf("helper check = %v", c)
	}

	// Another helper after ours: that is what git would run.
	s.e.git(dir, "config", "--local", "--add", "credential."+s.trunk+".helper", "osxkeychain")
	if c := checksByName(t, s.e.do(dir, ExitOK, "doctor"))["helper"]; c["status"] != checkWarn || !strings.Contains(c["detail"].(string), "osxkeychain") {
		t.Fatalf("helper check = %v", c)
	}

	// A reset after ours leaves git with no helper at all.
	s.e.git(dir, "config", "--local", "--add", "credential."+s.trunk+".helper", "")
	if c := checksByName(t, s.e.do(dir, ExitOK, "doctor"))["helper"]; c["status"] != checkSkip {
		t.Fatalf("helper check = %v", c)
	}

	// No helper for origin at all: skip, not a failure.
	s.e.git(dir, "config", "--local", "--unset-all", "credential."+s.trunk+".helper")
	if c := checksByName(t, s.e.do(dir, ExitOK, "doctor"))["helper"]; c["status"] != checkSkip {
		t.Fatalf("helper check = %v", c)
	}

	// The init checkout has no origin, but init added the wayglass remote
	// with the helper: doctor rates that one.
	if c := checksByName(t, s.e.do(s.a, ExitOK, "doctor"))["helper"]; c["status"] != checkOK || !strings.Contains(c["detail"].(string), "git fetch wayglass") {
		t.Fatalf("helper check = %v", c)
	}
	// A checkout with neither: nothing to report.
	s.e.git(s.a, "remote", "remove", "wayglass")
	if c, ok := checksByName(t, s.e.do(s.a, ExitOK, "doctor"))["helper"]; ok {
		t.Fatalf("helper check = %v, want none", c)
	}
}
