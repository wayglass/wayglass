package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalizeFamily(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "claude": "claude", " Anthropic ": "claude", "codex": "openai", "GPT": "openai",
		"google": "gemini", "llama": "meta", "other": "other", "person": "human", "human": "human",
	} {
		if got, ok := NormalizeFamily(in); !ok || got != want {
			t.Errorf("NormalizeFamily(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := NormalizeFamily("mistral"); ok {
		t.Error("unknown family accepted")
	}
}

// The client's family rides on declare and decide unless the call names one.
func TestFamilySentOnDeclareAndDecide(t *testing.T) {
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Family string `json:"family"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got[r.URL.Path] = body.Family
		_, _ = w.Write([]byte(`{"v":1,"ok":true,"data":{}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	c.Family = "claude"
	if _, err := c.DeclareIntent(t.Context(), DeclareIntentInput{Project: "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DecideIntent(t.Context(), "i0123456789", DecideInput{Project: "p", Family: "openai"}); err != nil {
		t.Fatal(err)
	}
	if got["/v1/intents"] != "claude" || got["/v1/intents/i0123456789/decide"] != "openai" {
		t.Fatalf("families sent = %v", got)
	}
}
