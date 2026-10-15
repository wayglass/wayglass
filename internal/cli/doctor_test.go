package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checksByName(t *testing.T, data map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, c := range get(data, "checks").([]any) {
		m := c.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

func TestDoctorHealthyProject(t *testing.T) {
	e := newFlow(t)
	e.do(e.repo, ExitOK, "init", "--project", "demo")
	data := e.do(e.repo, ExitOK, "doctor")
	if get(data, "healthy") != true || get(data, "project") != "demo" || get(data, "agent") != "agent-a" {
		t.Fatalf("doctor = %v", data)
	}
	checks := checksByName(t, data)
	for _, name := range []string{"git", "login", "worker", "auth", "project", "trunk"} {
		if checks[name]["status"] != checkOK {
			t.Errorf("%s = %v", name, checks[name])
		}
	}
	if d := checks["trunk"]["detail"].(string); !strings.Contains(d, "default branch main") {
		t.Errorf("trunk detail = %q", d)
	}
	// The read token is used in memory only.
	for _, tok := range e.worker.tokens {
		if b, _ := os.ReadFile(filepath.Join(e.repo, ".git", "config")); strings.Contains(string(b), tok) {
			t.Fatal("token written to .git/config")
		}
	}
}

func TestDoctorOutsideARepoSkipsProject(t *testing.T) {
	e := newFlow(t)
	data := e.do(t.TempDir(), ExitOK, "doctor")
	checks := checksByName(t, data)
	if checks["project"]["status"] != checkSkip || checks["trunk"]["status"] != checkSkip || get(data, "healthy") != true {
		t.Fatalf("doctor = %v", data)
	}
}

func TestDoctorFailures(t *testing.T) {
	t.Run("not logged in", func(t *testing.T) {
		hermetic(t)
		e := &flowEnv{t: t}
		data := e.do(t.TempDir(), ExitError, "doctor")
		checks := checksByName(t, data)
		if get(data, "healthy") != false || checks["login"]["status"] != checkFail || checks["worker"]["status"] != checkSkip {
			t.Fatalf("doctor = %v", data)
		}
		if !strings.Contains(checks["login"]["fix"].(string), "wayglass login") {
			t.Errorf("fix = %v", checks["login"]["fix"])
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		e := newFlow(t)
		t.Setenv("WAYGLASS_KEY", "not-the-key")
		data := e.do(e.repo, ExitError, "doctor")
		checks := checksByName(t, data)
		if checks["worker"]["status"] != checkOK || checks["auth"]["status"] != checkFail {
			t.Fatalf("doctor = %v", data)
		}
	})
	t.Run("unknown project", func(t *testing.T) {
		e := newFlow(t)
		e.do(e.repo, ExitOK, "init", "--project", "demo")
		t.Setenv(EnvProject, "gone")
		data := e.do(e.repo, ExitError, "doctor")
		if c := checksByName(t, data)["trunk"]; c["status"] != checkFail || !strings.Contains(c["detail"].(string), "no project gone") {
			t.Fatalf("trunk = %v", c)
		}
	})
	t.Run("older worker", func(t *testing.T) {
		e := newFlow(t)
		e.worker.noWhoami = true
		data := e.do(t.TempDir(), ExitOK, "doctor")
		if c := checksByName(t, data)["auth"]; c["status"] != checkOK || !strings.Contains(c["detail"].(string), "predates") {
			t.Fatalf("auth = %v", c)
		}
	})
}

func TestAgentKeysEndToEnd(t *testing.T) {
	e := newFlow(t)
	dir := t.TempDir()

	created := e.do(dir, ExitOK, "keys", "create", "--agent", "ci-bot", "--note", "nightly")
	key, _ := get(created, "key").(string)
	if !strings.HasPrefix(key, "wgk_") || get(created, "info", "agent") != "ci-bot" {
		t.Fatalf("create = %v", created)
	}
	id := get(created, "info", "id").(string)
	if got := e.do(dir, ExitUsage, "keys", "create"); get(got, "error", "code") != CodeUsage {
		t.Fatalf("create without --agent = %v", got)
	}

	list := e.do(dir, ExitOK, "keys", "list")
	if ks := get(list, "keys").([]any); len(ks) != 1 || strings.Contains(listString(ks), key) {
		t.Fatalf("list = %v", list)
	}

	// Log in with the agent key: the bound name is stored, --agent is ignored with a warning.
	t.Setenv("WAYGLASS_KEY", key)
	login := e.do(dir, ExitOK, "login", "--worker", os.Getenv("WAYGLASS_URL"), "--agent", "someone-else")
	if get(login, "agent") != "ci-bot" || get(login, "bound") != true || get(login, "role") != "agent" || len(get(login, "warnings").([]any)) != 1 {
		t.Fatalf("login = %v", login)
	}
	creds, err := os.ReadFile(filepath.Join(os.Getenv("WAYGLASS_CONFIG_DIR"), "credentials.yaml"))
	if err != nil || !strings.Contains(string(creds), "agent: ci-bot") {
		t.Fatalf("credentials = %q, %v", creds, err)
	}
	doc := e.do(dir, ExitOK, "doctor")
	if c := checksByName(t, doc)["auth"]; !strings.Contains(c["detail"].(string), "acts as ci-bot (bound") {
		t.Fatalf("doctor auth = %v", c)
	}
	// An agent key cannot manage keys.
	if got := e.do(dir, ExitError, "keys", "list"); get(got, "error", "code") != "forbidden" {
		t.Fatalf("agent list = %v", got)
	}

	t.Setenv("WAYGLASS_KEY", e.worker.key)
	if got := e.do(dir, ExitOK, "keys", "revoke", id); get(got, "revoked_at") == nil {
		t.Fatalf("revoke = %v", got)
	}
	t.Setenv("WAYGLASS_KEY", key)
	if got := e.do(dir, ExitError, "doctor"); checksByName(t, got)["auth"]["status"] != checkFail {
		t.Fatalf("doctor after revoke = %v", got)
	}
}

func listString(v []any) string {
	var b strings.Builder
	for _, x := range v {
		for k, val := range x.(map[string]any) {
			b.WriteString(k)
			if s, ok := val.(string); ok {
				b.WriteString(s)
			}
		}
	}
	return b.String()
}
