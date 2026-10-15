package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/wayglass/wayglass/internal/api"
)

// Error codes for 'wayglass login --human'.
const (
	// CodeLoginTimeout: no browser callback arrived in time.
	CodeLoginTimeout = "login_timeout"
	// CodeAccessNotConfigured: the Worker has no Cloudflare Access settings.
	CodeAccessNotConfigured = "access_not_configured"
)

// humanLoginTimeout is how long the loopback listener waits for the
// browser. A variable so tests can shorten it.
var humanLoginTimeout = 5 * time.Minute

// openBrowser opens url in the person's browser. A variable so tests can
// stand in for the browser and the Access login.
var openBrowser = func(u string) error {
	// u is the Worker's sign-in URL built by humanLogin, passed as one argument (no shell).
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u) //nolint:gosec // fixed program, URL as a single argument
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u) //nolint:gosec // fixed program, URL as a single argument
	default:
		cmd = exec.Command("xdg-open", u) //nolint:gosec // fixed program, URL as a single argument
	}
	return cmd.Start()
}

// listenLoopback listens on a free port of 127.0.0.1 only: never on all
// interfaces, so nothing else on the network can deliver or read a session.
func listenLoopback() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on 127.0.0.1: %w", err)
	}
	return ln, nil
}

// humanSession is what the browser hands the loopback listener.
type humanSession struct {
	key, agent, expiresAt string
}

// humanLogin runs the browser half of 'wayglass login --human': it listens
// on 127.0.0.1 only, opens <worker>/v1/human/login?port=&state= (Access does
// the identity-provider login there), and waits for the page to POST the
// session key back in a form body, so the key never appears in a URL, argv
// or on disk. The listener takes one valid callback, then closes.
// notify is told the URL to open before the wait starts.
func humanLogin(ctx context.Context, worker string, noBrowser bool, notify func(loginURL string, opened bool)) (humanSession, error) {
	ln, err := listenLoopback()
	if err != nil {
		return humanSession{}, err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return humanSession{}, err
	}
	state := base64.RawURLEncoding.EncodeToString(b)
	loginURL := fmt.Sprintf("%s/v1/human/login?%s", worker, url.Values{"port": {fmt.Sprint(port)}, "state": {state}}.Encode())

	got := make(chan humanSession, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodPost {
			// A key in a query string would end up in logs and history.
			http.Error(w, "send the session in a POST body", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if err := r.ParseForm(); err != nil || r.URL.RawQuery != "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("state")), []byte(state)) != 1 {
			http.Error(w, "this sign-in was not started by the waiting wayglass command", http.StatusBadRequest)
			return
		}
		s := humanSession{key: r.PostForm.Get("key"), agent: r.PostForm.Get("agent"), expiresAt: r.PostForm.Get("expires_at")}
		if !api.IsAgentKey(s.key) || s.agent == "" {
			http.Error(w, "the Worker sent no session key", http.StatusBadRequest)
			return
		}
		select {
		case got <- s:
		default:
			http.Error(w, "this sign-in was already completed", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>Wayglass</title><p>Signed in as %s. You can close this tab and return to the terminal.</p>", html.EscapeString(s.agent))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	opened := false
	if !noBrowser {
		opened = openBrowser(loginURL) == nil
	}
	notify(loginURL, opened)

	timer := time.NewTimer(humanLoginTimeout)
	defer timer.Stop()
	select {
	case s := <-got:
		return s, nil
	case <-ctx.Done():
		return humanSession{}, ctx.Err()
	case <-timer.C:
		msg := fmt.Sprintf("no sign-in arrived within %s", humanLoginTimeout)
		if !opened {
			msg += "; no browser was opened, so open the printed URL on this computer and sign in"
		}
		return humanSession{}, &Error{Code: CodeLoginTimeout, Message: msg, Exit: ExitError}
	}
}

// runHumanLogin signs a person in through Cloudflare Access and stores the
// short-lived human key like 'wayglass login' does.
func runHumanLogin(ctx context.Context, o Output, worker string, noBrowser bool) error {
	// Fail fast, before a browser opens, when the Worker has Access off.
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, worker+"/v1/human/session", nil)
	if err != nil {
		return err
	}
	probe.Header.Set("Accept", "application/json")
	if resp, err := (&http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(probe); err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), CodeAccessNotConfigured) {
			return &Error{Code: CodeAccessNotConfigured, Exit: ExitError,
				Message: "this Worker has no Cloudflare Access sign-in (ACCESS_TEAM_DOMAIN and ACCESS_AUD unset); ask its admin for a human key: wayglass keys create --human"}
		}
	}

	s, err := humanLogin(ctx, worker, noBrowser, func(u string, opened bool) {
		// stderr in both modes: stdout carries only the result (or the JSON envelope).
		if opened {
			fmt.Fprintf(o.Err, "Opened your browser to sign in with Cloudflare Access. If it did not open, visit:\n  %s\n", u)
		} else {
			fmt.Fprintf(o.Err, "Open this URL in a browser on this computer to sign in with Cloudflare Access:\n  %s\n", u)
		}
		fmt.Fprintf(o.Err, "Waiting up to %s...\n", humanLoginTimeout)
	})
	if err != nil {
		return err
	}
	c := api.New(worker, s.key)
	who, err := c.WhoAmI(ctx)
	if err != nil {
		return fmt.Errorf("the Worker refused the new session: %w", err)
	}
	if who.Kind != api.KindHuman || who.Source != api.SourceAccess {
		return &Error{Code: CodeError, Exit: ExitError, Message: fmt.Sprintf("the Worker returned a %s key from %q, not an Access human session; not saved", who.Kind, who.Source)}
	}
	expires := s.expiresAt
	if who.ExpiresAt != nil {
		expires = *who.ExpiresAt
	}
	if t, err := time.Parse(time.RFC3339, expires); err == nil {
		expires = t.UTC().Format(time.RFC3339)
	}
	p, err := api.SaveCredentials(api.Credentials{URL: worker, Key: s.key, Agent: who.Agent, Source: api.SourceAccess, ExpiresAt: expires})
	if err != nil {
		return err
	}
	res := loginResult{URL: worker, Agent: who.Agent, Path: p, Verified: true, Role: who.Role, Bound: who.Bound, Kind: who.Kind, Source: api.SourceAccess, ExpiresAt: expires, Warnings: []string{}}
	return o.Emit(res, func(out io.Writer) error {
		_, err := fmt.Fprintf(out, "Signed in to %s as %s through Cloudflare Access. Saved to %s.\nThis human session expires at %s; run 'wayglass login --human' again after that.\n", worker, who.Agent, p, formatExpiry(expires))
		return err
	})
}

func formatExpiry(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.Local().Format("Mon Jan 2 15:04 MST")
}
