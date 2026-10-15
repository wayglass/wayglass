package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/gitx"
)

// Doctor check states. Only "fail" makes the command exit 1.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
	checkSkip = "skip"
)

// minGit is the oldest git the README promises to work with.
const minGitMajor, minGitMinor = 2, 40

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Fix is the next thing to run or change, when there is one.
	Fix string `json:"fix,omitempty"`
}

type doctorResult struct {
	// Healthy is false when any check failed (the command then exits 1).
	Healthy bool          `json:"healthy"`
	Worker  string        `json:"worker,omitempty"`
	Agent   string        `json:"agent,omitempty"`
	Project string        `json:"project,omitempty"`
	Checks  []doctorCheck `json:"checks"`
}

func (r *doctorResult) add(name, status, detail, fix string) {
	r.Checks = append(r.Checks, doctorCheck{Name: name, Status: status, Detail: detail, Fix: fix})
	if status == checkFail {
		r.Healthy = false
	}
}

func newDoctorCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check git, your login, the Worker, and this project's trunk",
		Long: "Runs read-only checks in order: git version, stored login, Worker reachable,\n" +
			"key accepted (and the name it acts as), project config, and the trunk\n" +
			"answering over git with a 60-second read-only token. Later checks are skipped\n" +
			"when an earlier one they need failed. Exits 1 if any check failed.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			res := runDoctor(cmd.Context())
			o := a.output(cmd)
			if err := o.Emit(res, func(out io.Writer) error { return renderDoctor(out, o, res) }); err != nil {
				return err
			}
			if !res.Healthy {
				return WithExit(ExitError)
			}
			return nil
		},
	}
}

func runDoctor(ctx context.Context) *doctorResult {
	res := &doctorResult{Healthy: true, Checks: []doctorCheck{}}

	// 1. git
	if v, err := (gitx.Git{}).Version(ctx); err != nil {
		res.add("git", checkFail, "git not found on PATH", "install git 2.40 or later")
	} else if !gitx.AtLeast(v, minGitMajor, minGitMinor) {
		res.add("git", checkFail, "git "+v+" is older than 2.40", "upgrade git")
	} else {
		res.add("git", checkOK, "git "+v, "")
	}

	// 2. login
	creds, err := api.LoadCredentials()
	if err != nil {
		res.add("login", checkFail, err.Error(), "fix or remove the credentials file, then run 'wayglass login'")
		return skipRest(res, "worker", "auth", "project", "trunk")
	}
	res.Worker = creds.URL
	path, _ := api.CredentialsPath()
	switch {
	case creds.URL == "" || creds.Key == "":
		missing := []string{}
		if creds.URL == "" {
			missing = append(missing, "Worker URL")
		}
		if creds.Key == "" {
			missing = append(missing, "key")
		}
		res.add("login", checkFail, "no "+strings.Join(missing, " or "),
			"printf %s \"$KEY\" | wayglass login --worker <url> --key-stdin   (or set WAYGLASS_URL and WAYGLASS_KEY)")
		return skipRest(res, "worker", "auth", "project", "trunk")
	default:
		kind := "admin key"
		if api.IsAgentKey(creds.Key) {
			kind = "agent key"
		}
		if creds.Source == api.SourceAccess {
			kind = "Cloudflare Access human session"
		}
		res.add("login", checkOK, fmt.Sprintf("%s, %s (%s)", creds.URL, kind, source(path)), "")
	}

	// The key is only as private as its file. wayglass login writes 0600; a
	// copy, an editor or a backup may have widened it.
	if perm, ok := credentialsPerm(path); ok && perm&0o077 != 0 {
		res.add("permissions", checkWarn, fmt.Sprintf("%s is mode %04o; other users on this machine can read the key", path, perm), "chmod 600 "+path)
	}

	// A human session from 'wayglass login --human' runs out; say so before the Worker refuses it.
	if exp, ok := creds.SessionExpiry(); ok {
		if st, detail := sessionCheck(exp, time.Now()); st != checkOK {
			res.add("session", st, detail, "wayglass login --human")
			if st == checkFail {
				return skipRest(res, "worker", "auth", "project", "trunk")
			}
		} else {
			res.add("session", checkOK, detail, "")
		}
	}

	// 3 and 4. Worker reachable, key accepted.
	c := api.New(creds.URL, creds.Key)
	c.Agent = creds.Identity()
	c.HTTP.Timeout = 15 * time.Second
	who, err := c.CheckAuth(ctx)
	var apiErr *api.Error
	switch {
	case err == nil:
		res.add("worker", checkOK, "reachable", "")
		agent, how := c.Agent, "self-reported; any admin-key caller can claim any name"
		if who != nil {
			agent = who.Agent
			if who.Bound {
				how = "bound to this agent key"
			}
		} else {
			how = "Worker predates /v1/whoami"
		}
		res.Agent = agent
		res.add("auth", checkOK, fmt.Sprintf("acts as %s (%s)", agent, how), "")
	case errors.As(err, &apiErr) && apiErr.Code == api.CodeBadResponse:
		res.add("worker", checkFail, apiErr.Message, "check the URL: it should be the Worker's origin, e.g. https://wayglass.<you>.workers.dev")
		return skipRest(res, "auth", "project", "trunk")
	case errors.As(err, &apiErr) && apiErr.Code == api.CodeUnauthorized:
		res.add("worker", checkOK, "reachable", "")
		res.add("auth", checkFail, "key refused: "+apiErr.Message, "log in again with a current key; agent keys can be revoked by the admin")
		return skipRest(res, "project", "trunk")
	case errors.As(err, &apiErr) && apiErr.Code == api.CodeNotConfigured:
		res.add("worker", checkOK, "reachable", "")
		res.add("auth", checkFail, apiErr.Message, "set it: printf %s \"$KEY\" | npx wrangler secret put WAYGLASS_ADMIN_KEY (in worker/)")
		return skipRest(res, "project", "trunk")
	case apiErr != nil:
		res.add("worker", checkFail, apiErr.Error(), "")
		return skipRest(res, "auth", "project", "trunk")
	default:
		res.add("worker", checkFail, err.Error(), "check the URL and your network; 'curl -sI "+creds.URL+"/v1/whoami' should answer")
		return skipRest(res, "auth", "project", "trunk")
	}

	// 5. project config
	w, err := openWorkspace(ctx)
	if err != nil {
		var e *Error
		switch {
		case errors.As(err, &e) && e.Code == CodeNotRepo:
			res.add("project", checkSkip, "not inside a git repository", "")
		case errors.As(err, &e) && e.Code == CodeNotInitialized:
			res.add("project", checkSkip, "this repository has no .wayglass/config.yaml", "wayglass init")
		default:
			res.add("project", checkFail, err.Error(), "")
		}
		return skipRest(res, "trunk")
	}
	res.Project = w.cfg.Project
	if w.cfg.TrunkRemote == "" {
		res.add("project", checkWarn, fmt.Sprintf("%s (no trunk_remote in config)", w.cfg.Project), "")
	} else {
		res.add("project", checkOK, fmt.Sprintf("%s, jurisdiction %s", w.cfg.Project, orDash(w.cfg.Jurisdiction)), "")
	}

	// 5b. the credential helper 'wayglass clone' configured for origin, so
	// plain git fetch keeps working (never fails doctor: an agent's intent
	// clone has none and fetches through wayglass sync).
	if name, status, detail, fix := helperCheck(ctx, w.root, w.cfg.TrunkRemote); name != "" {
		res.add(name, status, detail, fix)
	}

	// 6. trunk over git with a read-only token
	tok, err := c.TrunkReadToken(ctx, w.cfg.Project)
	switch {
	case err == nil:
	case errors.As(err, &apiErr) && apiErr.Code == api.CodeNotFound && strings.HasPrefix(apiErr.Message, "no route"):
		res.add("trunk", checkSkip, "this Worker cannot mint trunk read tokens yet (redeploy it)", "")
		return res
	case errors.As(err, &apiErr) && apiErr.Code == api.CodeNotFound:
		res.add("trunk", checkFail, fmt.Sprintf("the Worker has no project %s", w.cfg.Project), "check .wayglass/config.yaml and WAYGLASS_PROJECT, or that you are logged in to the right Worker")
		return res
	default:
		res.add("trunk", checkFail, "no read token: "+err.Error(), "")
		return res
	}
	remote := tok.Trunk.Remote
	if w.cfg.TrunkRemote != "" && w.cfg.TrunkRemote != remote {
		res.add("trunk", checkWarn, fmt.Sprintf("config says %s but the Worker says %s", w.cfg.TrunkRemote, remote), "update trunk_remote in .wayglass/config.yaml")
		return res
	}
	g := gitx.Git{Dir: w.root, Token: tok.Token.Value} // in memory only
	branch, err := g.RemoteDefaultBranch(ctx, remote)
	switch {
	case err != nil:
		res.add("trunk", checkFail, "git ls-remote failed: "+err.Error(), "")
	case branch == "":
		res.add("trunk", checkWarn, "reachable but empty (nothing pushed yet)", "wayglass init pushes the first commit; rerun it without --no-push")
	default:
		res.add("trunk", checkOK, fmt.Sprintf("reachable over git, default branch %s", branch), "")
	}
	return res
}

// sessionWarnBefore is how close to its expiry a human session gets a warning.
const sessionWarnBefore = time.Hour

// sessionCheck rates a human session that expires at exp.
func sessionCheck(exp, now time.Time) (status, detail string) {
	left := exp.Sub(now)
	switch {
	case left <= 0:
		return checkFail, fmt.Sprintf("human session expired at %s", exp.Local().Format(time.RFC3339))
	case left < sessionWarnBefore:
		return checkWarn, fmt.Sprintf("human session expires in %s (at %s)", left.Round(time.Minute), exp.Local().Format(time.RFC3339))
	default:
		return checkOK, fmt.Sprintf("human session valid until %s", exp.Local().Format(time.RFC3339))
	}
}

// credentialsPerm is the mode of the credentials file; ok is false when
// there is none (the login came from the environment) or it cannot be read.
func credentialsPerm(path string) (perm os.FileMode, ok bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	return st.Mode().Perm(), true
}

func skipRest(res *doctorResult, names ...string) *doctorResult {
	for _, n := range names {
		res.add(n, checkSkip, "skipped: an earlier check failed", "")
	}
	return res
}

func source(path string) string {
	switch {
	case os.Getenv(api.EnvURL) != "" && os.Getenv(api.EnvKey) != "":
		return "from WAYGLASS_URL and WAYGLASS_KEY"
	case os.Getenv(api.EnvURL) != "" || os.Getenv(api.EnvKey) != "":
		return "partly from the environment, partly from " + path
	default:
		return "from " + path
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func renderDoctor(out io.Writer, o Output, res *doctorResult) error {
	st := cliStyles(o)
	ld := lipgloss.LightDark(isDark(o))
	marks := map[string]lipgloss.Style{
		checkOK:   lipgloss.NewStyle().Foreground(ld(lipgloss.Color("#2E7D32"), lipgloss.Color("#7BC67E"))),
		checkWarn: st.attention,
		checkFail: lipgloss.NewStyle().Bold(true).Foreground(ld(lipgloss.Color("#B3261E"), lipgloss.Color("#F28B82"))),
		checkSkip: st.muted,
	}
	summary := "all good"
	if !res.Healthy {
		summary = "something needs fixing"
	}
	_, _ = lipgloss.Fprintf(out, "%s %s\n\n", st.title.Render("Wayglass doctor"), st.muted.Render(summary))
	for _, ch := range res.Checks {
		detail := ch.Detail
		if ch.Status == checkSkip {
			detail = st.muted.Render(detail)
		}
		_, _ = lipgloss.Fprintf(out, "  %s %s%s\n", marks[ch.Status].Width(5).Render(ch.Status), st.label.Render(ch.Name), detail)
		if ch.Fix != "" && ch.Status != checkOK {
			_, _ = lipgloss.Fprintf(out, "        %s%s\n", st.label.Render(""), st.muted.Render("-> "+ch.Fix))
		}
	}
	return nil
}

// helperCheck rates the credential helper configured for the trunk remote in
// root: the remote whose URL is trunkRemote (origin after 'wayglass clone',
// wayglass after 'wayglass init'), else origin. name is "" when the checkout
// has neither (nothing to say).
func helperCheck(ctx context.Context, root, trunkRemote string) (name, status, detail, fix string) {
	g := gitx.Git{Dir: root}
	remote, url := trunkRemoteOf(ctx, g, trunkRemote)
	if url == "" {
		return "", "", "", ""
	}
	helpers, err := configuredHelpers(ctx, g, url)
	if err != nil {
		return "helper", checkWarn, "could not read git config: " + err.Error(), ""
	}
	// An empty entry resets the chain, so only helpers after the last
	// reset count; the final one is what clone configured.
	var last string
	for _, h := range helpers {
		last = h
	}
	redo := "wayglass clone again, or: git config credential." + url + ".helper '" + credentialHelperValue("wayglass", "<project>", "") + "'"
	if last == "" {
		return "helper", checkSkip, "no credential helper for " + remote + "; plain git fetch needs a token here (wayglass clone and init configure one)", ""
	}
	exe := helperCommand(last)
	if exe == "" {
		return "helper", checkWarn, remote + " uses credential helper " + last + ", not wayglass", redo
	}
	if _, err := exec.LookPath(exe); err != nil {
		return "helper", checkWarn, "the credential helper for " + remote + " cannot run: " + exe + " not found; plain git fetch will ask for a password", redo
	}
	return "helper", checkOK, "plain git fetch " + remote + " mints read-only tokens through " + exe, ""
}

// trunkRemoteOf finds the git remote that points at the trunk: by URL when
// the config names one, else origin. It returns "", "" when there is none.
func trunkRemoteOf(ctx context.Context, g gitx.Git, trunkRemote string) (name, url string) {
	if trunkRemote != "" {
		for _, n := range []string{"origin", trunkRemoteName} {
			if u, ok, _ := g.RemoteURL(ctx, n); ok && u == trunkRemote {
				return n, u
			}
		}
	}
	if u, ok, _ := g.RemoteURL(ctx, "origin"); ok && u != "" {
		return "origin", u
	}
	return "", ""
}
