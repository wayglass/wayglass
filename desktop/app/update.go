package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// EventUpdate carries an Update to the window after every check.
const EventUpdate = "update"

// DefaultReleasesURL is where the app looks for a newer release. GitHub's
// "latest" leaves out drafts and pre-releases on its own.
const DefaultReleasesURL = "https://api.github.com/repos/wayglass/wayglass/releases/latest"

// releasePrefix is the only place an update may send the person. The URL in
// the API answer is checked against it before it can be opened.
const releasePrefix = "https://github.com/wayglass/wayglass/releases/"

const (
	updateDelay    = 5 * time.Second // after startup, so the first poll wins the network
	updateInterval = 6 * time.Hour
	updateTimeout  = 15 * time.Second
	updateMaxBody  = 1 << 20
)

// Update is what the window shows: the running version and, when a newer
// tagged release exists, where to get it. The check replaces nothing on
// disk; the person downloads the new build and installs it as before.
type Update struct {
	Current   string `json:"current"`
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"available"`
	URL       string `json:"url,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

// CheckUpdate asks GitHub for the latest release now and returns the
// result. It never fails: while the repository is private or offline there
// is no release to see, and the window only shows an update when one is.
func (a *App) CheckUpdate() Update {
	ctx, cancel := context.WithTimeout(a.context(), updateTimeout)
	defer cancel()
	u, err := checkUpdate(ctx, a.httpClient(), a.releasesURL, a.version)
	if err != nil {
		u = Update{Current: a.version}
	}
	u.CheckedAt = a.now().UTC().Format(time.RFC3339)
	a.mu.Lock()
	a.update = u
	a.mu.Unlock()
	return u
}

// OpenUpdate opens the release page of the update the last check found.
// It takes no URL from the window: a script injected through agent text
// could otherwise send the person anywhere.
func (a *App) OpenUpdate() error {
	a.mu.Lock()
	u := a.update
	n := a.native
	a.mu.Unlock()
	if !u.Available || !strings.HasPrefix(u.URL, releasePrefix) {
		return errors.New("no update to open")
	}
	if n == nil {
		return errors.New("the window is not ready")
	}
	n.OpenURL(u.URL)
	return nil
}

// watchUpdates checks once shortly after startup and then every few hours,
// until the app shuts down. A build without a release version ("dev") never
// asks.
func (a *App) watchUpdates(ctx context.Context) {
	if _, ok := parseSemver(a.version); !ok {
		return
	}
	t := time.NewTimer(updateDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if u := a.CheckUpdate(); u.Available {
			a.emit(EventUpdate, u)
		}
		t.Reset(updateInterval)
	}
}

func (a *App) httpClient() *http.Client {
	if a.http != nil {
		return a.http
	}
	return http.DefaultClient
}

// checkUpdate compares current with the tag of the latest release at url.
// Anything but a 200 with a well-formed tag is an error; callers decide how
// quiet to be about it.
func checkUpdate(ctx context.Context, c *http.Client, url, current string) (Update, error) {
	u := Update{Current: current}
	cur, ok := parseSemver(current)
	if !ok {
		return u, fmt.Errorf("no release version to compare %q with", current)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return u, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "wayglass-desktop/"+current)
	resp, err := c.Do(req)
	if err != nil {
		return u, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return u, fmt.Errorf("releases: %s", resp.Status)
	}
	var rel struct {
		Tag        string `json:"tag_name"`
		URL        string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, updateMaxBody)).Decode(&rel); err != nil {
		return u, err
	}
	latest, ok := parseSemver(rel.Tag)
	if !ok {
		return u, fmt.Errorf("release tag %q is not a version", rel.Tag)
	}
	if rel.Draft || rel.Prerelease {
		return u, nil
	}
	u.Latest = rel.Tag
	if compareSemver(latest, cur) > 0 {
		u.Available = true
		if strings.HasPrefix(rel.URL, releasePrefix) {
			u.URL = rel.URL
		} else {
			u.URL = releasePrefix + "tag/" + rel.Tag
		}
	}
	return u, nil
}

// semver is MAJOR.MINOR.PATCH plus an optional pre-release; build metadata
// is ignored, as the spec says.
type semver struct {
	nums [3]int
	pre  string
}

// parseSemver accepts "v1.2.3", "1.2.3", "v1.2.3-rc.1" and "v1.2.3+build".
// A git describe such as "v0.1.0-3-gabc123" parses as a pre-release of
// v0.1.0, which is what it is: older than the next tag, newer than none.
func parseSemver(s string) (semver, bool) {
	var v semver
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.pre = s[i+1:]
		s = s[:i]
		if v.pre == "" {
			return semver{}, false
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return semver{}, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		v.nums[i] = n
	}
	return v, true
}

// compareSemver orders versions as semver 2.0 does: numbers first, then a
// pre-release sorts before the release it precedes, and pre-release
// identifiers compare numerically when both are numbers.
func compareSemver(a, b semver) int {
	for i := range a.nums {
		if a.nums[i] != b.nums[i] {
			if a.nums[i] < b.nums[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.pre == b.pre:
		return 0
	case a.pre == "":
		return 1
	case b.pre == "":
		return -1
	}
	as, bs := strings.Split(a.pre, "."), strings.Split(b.pre, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				if an < bn {
					return -1
				}
				return 1
			}
		case aerr == nil:
			return -1 // numeric identifiers sort before alphanumeric ones
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}
