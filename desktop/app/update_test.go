package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/api"
)

// fakeReleases is GitHub's "latest release" route: one JSON body, or a
// status such as 404, which is what a private repository answers.
func fakeReleases(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "wayglass-desktop/") {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckUpdate(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		current string
		want    Update
		wantErr string
	}{
		{"newer release", 200, `{"tag_name":"v0.2.0","html_url":"https://github.com/wayglass/wayglass/releases/tag/v0.2.0"}`, "v0.1.0",
			Update{Current: "v0.1.0", Latest: "v0.2.0", Available: true, URL: "https://github.com/wayglass/wayglass/releases/tag/v0.2.0"}, ""},
		{"same release", 200, `{"tag_name":"v0.1.0","html_url":"https://github.com/wayglass/wayglass/releases/tag/v0.1.0"}`, "v0.1.0",
			Update{Current: "v0.1.0", Latest: "v0.1.0"}, ""},
		{"running ahead of the release", 200, `{"tag_name":"v0.1.0","html_url":"https://github.com/wayglass/wayglass/releases/tag/v0.1.0"}`, "v0.2.0",
			Update{Current: "v0.2.0", Latest: "v0.1.0"}, ""},
		{"untagged build after the release", 200, `{"tag_name":"v0.1.0","html_url":"https://github.com/wayglass/wayglass/releases/tag/v0.1.0"}`, "v0.1.0-3-gabc1234",
			Update{Current: "v0.1.0-3-gabc1234", Latest: "v0.1.0", Available: true, URL: "https://github.com/wayglass/wayglass/releases/tag/v0.1.0"}, ""},
		{"private repository", 404, `{"message":"Not Found"}`, "v0.1.0", Update{Current: "v0.1.0"}, "404"},
		{"rate limited", 403, `{"message":"API rate limit exceeded"}`, "v0.1.0", Update{Current: "v0.1.0"}, "403"},
		{"not json", 200, `<html>`, "v0.1.0", Update{Current: "v0.1.0"}, "invalid character"},
		{"tag is not a version", 200, `{"tag_name":"nightly","html_url":"https://github.com/wayglass/wayglass/releases/tag/nightly"}`, "v0.1.0", Update{Current: "v0.1.0"}, "not a version"},
		{"pre-release is not offered", 200, `{"tag_name":"v0.2.0-rc.1","prerelease":true,"html_url":"https://github.com/wayglass/wayglass/releases/tag/v0.2.0-rc.1"}`, "v0.1.0",
			Update{Current: "v0.1.0"}, ""},
		// A link that leaves the project's releases is replaced by the tag's own page.
		{"foreign link", 200, `{"tag_name":"v0.2.0","html_url":"https://example.com/evil"}`, "v0.1.0",
			Update{Current: "v0.1.0", Latest: "v0.2.0", Available: true, URL: "https://github.com/wayglass/wayglass/releases/tag/v0.2.0"}, ""},
		{"dev build never asks", 200, `{"tag_name":"v9.0.0","html_url":"https://github.com/wayglass/wayglass/releases/tag/v9.0.0"}`, "dev",
			Update{Current: "dev"}, "no release version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fakeReleases(t, tt.status, tt.body)
			got, err := checkUpdate(context.Background(), srv.Client(), srv.URL, tt.current)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("err = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestCheckUpdateBound(t *testing.T) {
	// The bound method swallows every failure: the window sees "no update",
	// never an error, while the repository is private or the network is off.
	t.Run("quiet on 404", func(t *testing.T) {
		rel := fakeReleases(t, 404, `{"message":"Not Found"}`)
		_, w := newFake(t)
		n := &fakeNative{}
		a := newVersionedApp(t, w, rel, "v0.1.0", n)
		u := a.CheckUpdate()
		if u.Available || u.Latest != "" || u.Current != "v0.1.0" || u.CheckedAt == "" {
			t.Errorf("update = %+v", u)
		}
		if err := a.OpenUpdate(); err == nil {
			t.Error("OpenUpdate opened nothing but returned no error")
		}
		if len(n.opened) != 0 {
			t.Errorf("opened %v", n.opened)
		}
	})
	t.Run("opens only the checked release page", func(t *testing.T) {
		rel := fakeReleases(t, 200, `{"tag_name":"v0.3.0","html_url":"https://github.com/wayglass/wayglass/releases/tag/v0.3.0"}`)
		_, w := newFake(t)
		n := &fakeNative{}
		a := newVersionedApp(t, w, rel, "v0.1.0", n)
		if u := a.CheckUpdate(); !u.Available || u.Latest != "v0.3.0" {
			t.Fatalf("update = %+v", u)
		}
		if err := a.OpenUpdate(); err != nil {
			t.Fatal(err)
		}
		if len(n.opened) != 1 || n.opened[0] != "https://github.com/wayglass/wayglass/releases/tag/v0.3.0" {
			t.Errorf("opened %v", n.opened)
		}
		if s := a.Session(); s.Version != "v0.1.0" {
			t.Errorf("session version = %q", s.Version)
		}
	})
	t.Run("dev build sends no request", func(t *testing.T) {
		var hits atomic.Int32
		rel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(rel.Close)
		_, w := newFake(t)
		a := newVersionedApp(t, w, rel, "", &fakeNative{})
		ctx, cancel := context.WithCancel(context.Background())
		a.startup(ctx)
		time.Sleep(20 * time.Millisecond)
		cancel()
		if u := a.CheckUpdate(); u.Available || u.Current != "dev" {
			t.Errorf("update = %+v", u)
		}
		if hits.Load() != 0 {
			t.Errorf("a dev build asked for releases %d times", hits.Load())
		}
	})
}

func newVersionedApp(t *testing.T, w *httptest.Server, rel *httptest.Server, version string, n Native) *App {
	t.Helper()
	t.Setenv("WAYGLASS_PROJECT", "")
	return New(Options{
		Creds: api.Credentials{URL: w.URL, Key: testKey, Agent: "john"},
		HTTP:  rel.Client(), Native: n, Interval: time.Hour,
		Version: version, ReleasesURL: rel.URL,
	})
}

func TestSemver(t *testing.T) {
	parse := []struct {
		in   string
		ok   bool
		want semver
	}{
		{"v1.2.3", true, semver{nums: [3]int{1, 2, 3}}},
		{"1.2.3", true, semver{nums: [3]int{1, 2, 3}}},
		{"v1.2.3-rc.1", true, semver{nums: [3]int{1, 2, 3}, pre: "rc.1"}},
		{"v1.2.3+build.7", true, semver{nums: [3]int{1, 2, 3}}},
		{"v0.1.0-3-gabc1234", true, semver{nums: [3]int{0, 1, 0}, pre: "3-gabc1234"}},
		{"dev", false, semver{}},
		{"", false, semver{}},
		{"v1.2", false, semver{}},
		{"v01.2.3", false, semver{}},
		{"v1.2.3-", false, semver{}},
		{"v1.-2.3", false, semver{}},
	}
	for _, tt := range parse {
		got, ok := parseSemver(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseSemver(%q) = %+v, %v; want %+v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
	order := []struct {
		a, b string
		want int
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.1.0", "v1.0.9", 1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.0.0-rc.1", "v1.0.0", -1},
		{"v1.0.0-rc.1", "v1.0.0-rc.2", -1},
		{"v1.0.0-rc.10", "v1.0.0-rc.9", 1},
		{"v1.0.0-alpha", "v1.0.0-alpha.1", -1},
		{"v1.0.0-alpha.1", "v1.0.0-beta", -1},
		{"v1.0.0-1", "v1.0.0-alpha", -1},
		{"v1.0.0+a", "v1.0.0+b", 0},
	}
	for _, tt := range order {
		a, _ := parseSemver(tt.a)
		b, _ := parseSemver(tt.b)
		if got := compareSemver(a, b); got != tt.want {
			t.Errorf("compare(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
		if got := compareSemver(b, a); got != -tt.want {
			t.Errorf("compare(%s, %s) = %d, want %d", tt.b, tt.a, got, -tt.want)
		}
	}
}
