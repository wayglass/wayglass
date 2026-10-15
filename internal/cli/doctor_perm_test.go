package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/wayglass/wayglass/internal/api"
)

// The key is only as private as its file, so a credentials file other
// users can read gets a warning (never a failure: the login still works).
func TestDoctorWarnsOnOpenCredentialsFile(t *testing.T) {
	e := newFlow(t)
	url, key := os.Getenv("WAYGLASS_URL"), os.Getenv("WAYGLASS_KEY")
	t.Setenv("WAYGLASS_URL", "")
	t.Setenv("WAYGLASS_KEY", "")
	path, err := api.SaveCredentials(api.Credentials{URL: url, Key: key, Agent: "agent-a"})
	if err != nil {
		t.Fatal(err)
	}
	data := e.do(t.TempDir(), ExitOK, "doctor")
	if c, ok := checksByName(t, data)["permissions"]; ok {
		t.Fatalf("a 0600 file was flagged: %v", c)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	data = e.do(t.TempDir(), ExitOK, "doctor")
	c, ok := checksByName(t, data)["permissions"]
	if !ok || c["status"] != checkWarn || !strings.Contains(c["fix"].(string), "chmod 600 ") || get(data, "healthy") != true {
		b, _ := json.Marshal(data)
		t.Fatalf("doctor = %s", b)
	}
}
