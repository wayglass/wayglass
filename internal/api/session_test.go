package api

import (
	"net/http"
	"testing"
)

func TestParseSessionURL(t *testing.T) {
	tests := []struct {
		in      string
		want    SessionRef
		wantErr bool
	}{
		{in: "https://api.wayglass.dev/v1/projects/wayglass/sessions/s0123456789",
			want: SessionRef{Worker: "https://api.wayglass.dev", Project: "wayglass", ID: "s0123456789"}},
		{in: "  https://api.wayglass.dev/v1/projects/a.b_c-d/sessions/sabcdef0123/\n",
			want: SessionRef{Worker: "https://api.wayglass.dev", Project: "a.b_c-d", ID: "sabcdef0123"}},
		{in: "https://example.com/prefix/v1/projects/demo/sessions/s0123456789",
			want: SessionRef{Worker: "https://example.com/prefix", Project: "demo", ID: "s0123456789"}},
		{in: "http://localhost:8787/v1/projects/demo/sessions/s0123456789",
			want: SessionRef{Worker: "http://localhost:8787", Project: "demo", ID: "s0123456789"}},
		{in: "http://evil.example/v1/projects/demo/sessions/s0123456789", wantErr: true}, // http off localhost
		{in: "https://api.wayglass.dev/v1/projects/demo/sessions/i0123456789", wantErr: true},
		{in: "https://api.wayglass.dev/v1/projects/Demo/sessions/s0123456789", wantErr: true},
		{in: "https://api.wayglass.dev/v1/projects/demo/sessions/s0123456789?x=1", wantErr: true},
		{in: "https://user:pw@api.wayglass.dev/v1/projects/demo/sessions/s0123456789", wantErr: true},
		{in: "wayglass://demo/s0123456789", wantErr: true},
		{in: "s0123456789", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseSessionURL(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			if !tt.wantErr && got.URL() != got.Worker+"/v1/projects/"+got.Project+"/sessions/"+got.ID {
				t.Fatalf("URL() = %s", got.URL())
			}
		})
	}
}

func TestSameWorker(t *testing.T) {
	c := New("https://API.wayglass.dev/", "k")
	tests := []struct {
		worker string
		want   bool
	}{
		{"https://api.wayglass.dev", true},
		{"https://api.wayglass.dev.evil.example", false},
		{"http://api.wayglass.dev", false},
		{"https://other.example", false},
	}
	for _, tt := range tests {
		if got := c.SameWorker(SessionRef{Worker: tt.worker}); got != tt.want {
			t.Errorf("SameWorker(%s) = %v, want %v", tt.worker, got, tt.want)
		}
	}
}

func TestSessionRoutes(t *testing.T) {
	var seen []string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/v1/intents/i0123456789/sessions":
			writeOK(w, 201, map[string]any{"session": map[string]any{"id": "s0123456789", "notes": "n"}, "url": "https://w/v1/projects/demo/sessions/s0123456789"})
		case "/v1/projects/demo/sessions/s0123456789":
			writeOK(w, 200, map[string]any{"project": "demo", "session": map[string]any{"id": "s0123456789"}, "forks": []any{}})
		case "/v1/projects/demo/sessions/s0123456789/fork":
			writeOK(w, 201, map[string]any{"intent": map[string]any{"id": "iaaaaaaaaaa", "parent_intent_id": "i0123456789"},
				"token": map[string]any{"value": "art_v1_x", "scope": "write"}})
		default:
			writeFail(w, 404, "not_found", "no route")
		}
	})
	sh, err := c.ShareSession(t.Context(), "i0123456789", ShareSessionInput{Project: "demo", Notes: "n"})
	if err != nil || sh.Session.ID != "s0123456789" || sh.URL == "" {
		t.Fatalf("share = %+v, %v", sh, err)
	}
	v, err := c.GetSession(t.Context(), "demo", "s0123456789")
	if err != nil || v.Project != "demo" {
		t.Fatalf("get = %+v, %v", v, err)
	}
	f, err := c.ForkSession(t.Context(), "demo", "s0123456789")
	if err != nil || f.Intent.ParentIntentID == nil || *f.Intent.ParentIntentID != "i0123456789" || f.Token.Value != "art_v1_x" {
		t.Fatalf("fork = %+v, %v", f, err)
	}
	if _, err := c.GetSession(t.Context(), "demo", "../x"); !IsCode(err, CodeBadRequest) {
		t.Fatalf("bad id err = %v", err)
	}
	want := []string{"POST /v1/intents/i0123456789/sessions", "GET /v1/projects/demo/sessions/s0123456789", "POST /v1/projects/demo/sessions/s0123456789/fork"}
	if len(seen) != len(want) {
		t.Fatalf("seen = %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen = %v", seen)
		}
	}
}
