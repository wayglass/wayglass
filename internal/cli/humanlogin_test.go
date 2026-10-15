package cli

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/notes"
)

const sessionKey = "wgk_" + "ab12" + "0000000000000000000000000000000000000000000000000000000000cd"

// fakeBrowser stands in for the browser and the Access sign-in page: it
// checks the login URL, then posts to the loopback callback.
func fakeBrowser(t *testing.T, worker string, post func(callback, state string)) func(string) error {
	return func(u string) error {
		p, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(u, worker+"/v1/human/login?") {
			t.Errorf("login URL = %s", u)
		}
		if strings.Contains(u, "wgk_") {
			t.Errorf("login URL carries a key: %s", u)
		}
		post("http://127.0.0.1:"+p.Query().Get("port")+"/callback", p.Query().Get("state"))
		return nil
	}
}

func postForm(t *testing.T, target string, v url.Values) int {
	t.Helper()
	resp, err := http.PostForm(target, v) //nolint:noctx // loopback test listener
	if err != nil {
		return -1
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestLoginHumanStoresAccessSession(t *testing.T) {
	hermetic(t)
	f, srv := newFakeWorker(t)
	exp := time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)
	f.agentKeys[sessionKey] = &api.AgentKey{ID: "k0000000001", Agent: "person@example.com", Kind: api.KindHuman, Source: api.SourceAccess, ExpiresAt: &exp}

	var callback string
	old := openBrowser
	t.Cleanup(func() { openBrowser = old })
	openBrowser = fakeBrowser(t, srv.URL, func(cb, state string) {
		callback = cb
		good := url.Values{"state": {state}, "key": {sessionKey}, "agent": {"person@example.com"}, "expires_at": {exp}}
		// A callback that did not start here is refused, and the login keeps waiting.
		bad := url.Values{"state": {"not-the-state"}, "key": {sessionKey}, "agent": {"x"}}
		if got := postForm(t, cb, bad); got != http.StatusBadRequest {
			t.Errorf("state mismatch = %d, want 400", got)
		}
		// The key is never accepted in a URL.
		resp, err := http.Get(cb + "?" + good.Encode()) //nolint:noctx // loopback test listener
		if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET callback = %v %v, want 405", resp, err)
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		if got := postForm(t, cb, good); got != http.StatusOK {
			t.Errorf("good callback = %d", got)
		}
	})

	exit, stdout, stderr := run(t, "login", "--human", "--worker", srv.URL, "--json")
	if exit != ExitOK {
		t.Fatalf("login --human: exit %d\n%s\n%s", exit, stdout, stderr)
	}
	if strings.Contains(stdout, sessionKey) || strings.Contains(stderr, sessionKey) {
		t.Fatal("login --human printed the key")
	}
	if !strings.Contains(stderr, srv.URL+"/v1/human/login?") {
		t.Errorf("the sign-in URL was not printed for headless use: %q", stderr)
	}
	env := checkEnvelope(t, stdout)
	var data loginResult
	if err := json.Unmarshal(*env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Agent != "person@example.com" || data.Source != api.SourceAccess || data.Kind != api.KindHuman || data.ExpiresAt != exp {
		t.Fatalf("login result = %+v", data)
	}
	// Single use: the listener is gone once the session arrived.
	if got := postForm(t, callback, url.Values{"state": {"x"}}); got != -1 {
		t.Errorf("listener still answering after login: %d", got)
	}
	// On disk only in the 0600 credentials file.
	dir, _ := api.ConfigDir()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "credentials.yaml" {
		t.Fatalf("config dir = %v", entries)
	}
	st, err := os.Stat(filepath.Join(dir, "credentials.yaml"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %v %v", st, err)
	}
	c, err := api.LoadCredentials()
	if err != nil || c.Key != sessionKey || c.Source != api.SourceAccess || c.ExpiresAt != exp || c.Agent != "person@example.com" {
		t.Fatalf("stored = %+v %v", c, err)
	}
	if e, ok := c.SessionExpiry(); !ok || e.Format(time.RFC3339) != exp {
		t.Fatalf("SessionExpiry = %v %v", e, ok)
	}
}

func TestLoginHumanTimesOutWithoutBrowser(t *testing.T) {
	hermetic(t)
	_, srv := newFakeWorker(t)
	oldT, oldB := humanLoginTimeout, openBrowser
	t.Cleanup(func() { humanLoginTimeout, openBrowser = oldT, oldB })
	humanLoginTimeout = 200 * time.Millisecond
	openBrowser = func(string) error { return errors.New("no display") }
	exit, stdout, stderr := run(t, "login", "--human", "--worker", srv.URL, "--json")
	if exit != ExitError {
		t.Fatalf("exit %d, want 1: %s", exit, stdout)
	}
	env := checkEnvelope(t, stdout)
	if env.Error.Code != CodeLoginTimeout || !strings.Contains(env.Error.Message, "no browser was opened") {
		t.Fatalf("error = %+v", env.Error)
	}
	if !strings.Contains(stderr, "Open this URL") {
		t.Errorf("stderr = %q", stderr)
	}
	if p, _ := api.CredentialsPath(); func() bool { _, err := os.Stat(p); return err == nil }() {
		t.Fatal("a timed-out login stored credentials")
	}
}

func TestLoginHumanAccessNotConfigured(t *testing.T) {
	hermetic(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"v":1,"ok":false,"error":{"code":"access_not_configured","message":"off"}}`))
	}))
	t.Cleanup(srv.Close)
	old := openBrowser
	t.Cleanup(func() { openBrowser = old })
	openBrowser = func(string) error { t.Error("opened a browser for a Worker without Access"); return nil }
	exit, stdout, _ := run(t, "login", "--human", "--worker", srv.URL, "--json")
	env := checkEnvelope(t, stdout)
	if exit != ExitError || env.Error.Code != CodeAccessNotConfigured {
		t.Fatalf("exit %d, %+v", exit, env.Error)
	}
}

func TestListenLoopbackOnly(t *testing.T) {
	ln, err := listenLoopback()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a := ln.Addr().(*net.TCPAddr)
	if !a.IP.Equal(net.IPv4(127, 0, 0, 1)) || a.Port == 0 {
		t.Fatalf("listening on %v", a)
	}
}

func TestSessionCheck(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		exp  time.Time
		want string
	}{
		{now.Add(-time.Minute), checkFail},
		{now.Add(30 * time.Minute), checkWarn},
		{now.Add(5 * time.Hour), checkOK},
	} {
		if got, _ := sessionCheck(tc.exp, now); got != tc.want {
			t.Errorf("sessionCheck(%v) = %s, want %s", tc.exp, got, tc.want)
		}
	}
}

func TestDoctorReportsHumanSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exp    time.Duration
		status string
		exit   int
	}{
		{"expired", -time.Hour, checkFail, ExitError},
		{"expiring", 20 * time.Minute, checkWarn, ExitOK},
		{"valid", 6 * time.Hour, checkOK, ExitOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlow(t)
			exp := time.Now().Add(tc.exp).UTC().Format(time.RFC3339)
			e.worker.agentKeys[sessionKey] = &api.AgentKey{ID: "k0000000002", Agent: "person@example.com", Kind: api.KindHuman, Source: api.SourceAccess, ExpiresAt: &exp}
			t.Setenv("WAYGLASS_KEY", "")
			if _, err := api.SaveCredentials(api.Credentials{URL: os.Getenv("WAYGLASS_URL"), Key: sessionKey, Agent: "person@example.com", Source: api.SourceAccess, ExpiresAt: exp}); err != nil {
				t.Fatal(err)
			}
			data := e.do(t.TempDir(), tc.exit, "doctor")
			checks := checksByName(t, data)
			if checks["session"]["status"] != tc.status {
				t.Fatalf("session check = %v", checks["session"])
			}
			if tc.status == checkFail && checks["session"]["fix"] != "wayglass login --human" {
				t.Errorf("fix = %v", checks["session"]["fix"])
			}
		})
	}
	t.Run("WAYGLASS_KEY overrides the stored session", func(t *testing.T) {
		e := newFlow(t)
		exp := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		if _, err := api.SaveCredentials(api.Credentials{URL: os.Getenv("WAYGLASS_URL"), Key: sessionKey, Source: api.SourceAccess, ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
		checks := checksByName(t, e.do(t.TempDir(), ExitOK, "doctor"))
		if _, ok := checks["session"]; ok {
			t.Fatalf("session check with WAYGLASS_KEY set = %v", checks["session"])
		}
	})
}

func TestWhyMarksAccessDeciders(t *testing.T) {
	aps := []notes.Approval{
		{By: "bot", Verdict: "approve", IdentitySource: "key"},
		{By: "you@example.com", Verdict: "approve", IdentitySource: api.SourceAccess},
	}
	if got := deciderSource("you@example.com", aps); got != api.SourceAccess {
		t.Fatalf("deciderSource = %q", got)
	}
	if got := deciderSource("policy", aps); got != "" {
		t.Fatalf("deciderSource(policy) = %q", got)
	}
	if got := withSource("you@example.com", api.SourceAccess); got != "you@example.com (Access)" {
		t.Fatalf("withSource = %q", got)
	}
	if got := withSource("bot", "key"); got != "bot" {
		t.Fatalf("withSource(key) = %q", got)
	}
	// The note stays additive: old readers see the same fields.
	b, _ := json.Marshal(notes.Note{DecidedBy: "x"})
	if strings.Contains(string(b), "identity_source") {
		t.Fatalf("empty identity_source serialised: %s", b)
	}
}
