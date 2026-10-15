package app

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wayglass/wayglass/internal/api"
)

// prefs is desktop.yaml next to the CLI's credentials.yaml. It holds only
// project names, the display name and window preferences, never a key: the key stays in the
// CLI's login file, which the app reads and never writes.
type prefs struct {
	Projects      []string `yaml:"projects"`
	Current       string   `yaml:"current,omitempty"`
	Notifications *bool    `yaml:"notifications,omitempty"` // nil means on
	// DisplayName, when set, is who this app approves as. Empty falls back
	// to the CLI's identity (login, $WAYGLASS_AGENT, then user@host).
	DisplayName string `yaml:"display_name,omitempty"`
}

// PrefsPath is where the desktop app keeps its project list.
func PrefsPath() (string, error) {
	d, err := api.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "desktop.yaml"), nil
}

func loadPrefs(path string) (prefs, error) {
	var p prefs
	if path == "" {
		return p, nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // our own config file
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return p, nil
	case err != nil:
		return p, err
	}
	if err := yaml.Unmarshal(b, &p); err != nil {
		return prefs{}, err
	}
	p.Projects = slices.DeleteFunc(p.Projects, func(s string) bool { return !validProject(s) })
	return p, nil
}

func savePrefs(path string, p prefs) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// projectRe is the Worker's project name rule (internal/cli/workspace.go).
var projectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func validProject(s string) bool {
	return projectRe.MatchString(s) && !strings.Contains(s, "--")
}

// discoverProjects finds project names the CLI already knows about without
// asking the Worker (it has no list route): $WAYGLASS_PROJECT and the
// .wayglass/config.yaml of the checkout the app was started from.
func discoverProjects(cwd string) []string {
	var out []string
	if p := os.Getenv("WAYGLASS_PROJECT"); validProject(p) {
		out = append(out, p)
	}
	for dir := cwd; dir != "" && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		b, err := os.ReadFile(filepath.Join(dir, ".wayglass", "config.yaml")) //nolint:gosec // a project's own config
		if err != nil {
			continue
		}
		var c struct {
			Project string `yaml:"project"`
		}
		if yaml.Unmarshal(b, &c) == nil && validProject(c.Project) {
			out = append(out, c.Project)
		}
		break
	}
	return out
}
