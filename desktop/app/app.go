// Package app is the Go side of the Wayglass desktop window. Its exported
// methods on *App are the frontend's whole API (Wails binds them); all
// network work goes through internal/api, the same client the CLI uses, so
// the app needs no secret of its own.
package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/textsafe"
	"github.com/wayglass/wayglass/internal/tui"
)

// Events the Go side pushes to the window.
const (
	EventSnapshot = "snapshot" // a fresh Snapshot
	EventPoll     = "poll"     // a PollStatus after every poll
)

// auditPage and auditPages bound one poll's audit read.
const (
	auditPage  = 500
	auditPages = 10
	keepEvents = 300
	// fullEvery forces a full re-read even without audit news, so a status
	// the audit log does not mention (none today) still shows up.
	fullEvery = 30 * time.Second
)

// Native is everything the app needs from the Wails runtime, behind an
// interface so tests never touch a window.
type Native interface {
	Emit(event string, data ...any)
	Notify(title, body string) error
	SetTitle(title string)
	OpenURL(url string) // in the person's browser
	// CopyText puts text on the clipboard (the clone command).
	CopyText(text string) error
}

// PollStatus tells the window whether the last poll worked.
type PollStatus struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Failures int    `json:"failures"`
	NextMS   int64  `json:"next_ms"`
	At       string `json:"at"`
}

// Options configures New. Zero values are fine except where noted.
type Options struct {
	Creds     api.Credentials // from api.LoadCredentials
	CredsErr  error           // why the login could not be read
	PrefsPath string          // desktop.yaml; "" keeps the list in memory
	Cwd       string          // to discover the checkout's project
	Interval  time.Duration   // poll interval; default tui.DefaultPollInterval
	HTTP      *http.Client    // tests
	Now       func() time.Time
	Native    Native // tests; the window's runtime otherwise
	// Version is the build's release version ("v0.2.0"). Anything else,
	// such as "dev", turns the update check off.
	Version string
	// ReleasesURL overrides DefaultReleasesURL (tests).
	ReleasesURL string
}

// App is bound to the window.
type App struct {
	mu        sync.Mutex
	client    *api.Client
	session   Session
	prefs     prefs
	prefsPath string
	baseID    string // the CLI's identity; a display name overrides it
	native    Native
	interval  time.Duration
	now       func() time.Time
	http      *http.Client

	version     string
	releasesURL string
	update      Update // the last check

	root   context.Context
	cancel context.CancelFunc // stops the current poller

	fetchMu  sync.Mutex // one refresh at a time keeps the audit cursor sane
	project  string     // the project the cached state below belongs to
	cursor   int
	events   []EventView // newest first
	last     *Snapshot
	lastFull time.Time
	// dirty: audit events were read since last was built (the rest of that
	// refresh failed), so the next poll must rebuild instead of serving last.
	dirty    bool
	notified map[string]bool // decision ids already announced
}

// requestTimeout bounds every request the window waits on, so a Worker
// that stops answering mid-request never leaves a promise hanging.
const requestTimeout = 30 * time.Second

// New builds the app from the CLI's login. It never fails: a missing login
// is shown in the window with what to run.
func New(o Options) *App {
	a := &App{
		prefsPath: o.PrefsPath, native: o.Native,
		interval:    cmp.Or(o.Interval, tui.DefaultPollInterval),
		now:         o.Now,
		http:        o.HTTP,
		notified:    map[string]bool{},
		version:     cmp.Or(o.Version, "dev"),
		releasesURL: cmp.Or(o.ReleasesURL, DefaultReleasesURL),
	}
	if a.now == nil {
		a.now = time.Now
	}
	a.update = Update{Current: a.version}
	a.baseID = o.Creds.Identity()
	p, err := loadPrefs(o.PrefsPath)
	if err != nil {
		log.Printf("wayglass desktop: ignoring %s: %v", o.PrefsPath, err)
	}
	if checkDisplayName(p.DisplayName) != nil {
		p.DisplayName = ""
	}
	identity := cmp.Or(p.DisplayName, a.baseID)
	a.session = Session{Worker: o.Creds.URL, Identity: identity, DefaultIdentity: a.baseID, PollMillis: a.interval.Milliseconds(), Projects: []string{}, Version: a.version}
	switch {
	case o.CredsErr != nil:
		a.session.Problem = "Could not read the Wayglass login: " + o.CredsErr.Error()
	case o.Creds.URL == "" || o.Creds.Key == "":
		a.session.Problem = "Not logged in. Run 'wayglass login --worker <url>' in a terminal, then reopen Wayglass."
	case sessionExpired(o.Creds, a.now()):
		// The stored human session is over: every request would get 401,
		// which would read as the Worker being down. Say what to run instead.
		a.session.Problem = "Your Wayglass session expired. Run 'wayglass login --human' in a terminal, then reopen Wayglass."
	default:
		if err := api.ValidateURL(o.Creds.URL); err != nil {
			a.session.Problem = err.Error()
			break
		}
		c := api.New(o.Creds.URL, o.Creds.Key)
		c.Agent = identity // approvals are recorded as this name
		if o.HTTP != nil {
			c.HTTP = o.HTTP
		}
		a.client = c
		a.session.LoggedIn = true
	}

	for _, d := range discoverProjects(o.Cwd) {
		if !slices.Contains(p.Projects, d) {
			p.Projects = append(p.Projects, d)
		}
		if p.Current == "" {
			p.Current = d
		}
	}
	if !slices.Contains(p.Projects, p.Current) {
		p.Current = ""
	}
	if p.Current == "" && len(p.Projects) > 0 {
		p.Current = p.Projects[0]
	}
	a.prefs = p
	a.syncSession()
	return a
}

// sessionExpired reports a stored human session ('wayglass login --human')
// whose key has stopped working. Admin and agent keys have no expiry.
func sessionExpired(c api.Credentials, now time.Time) bool {
	t, ok := c.SessionExpiry()
	return ok && !now.Before(t)
}

// syncSession copies prefs into the session. Callers hold a.mu.
func (a *App) syncSession() {
	a.session.Projects = slices.Clone(a.prefs.Projects)
	if a.session.Projects == nil {
		a.session.Projects = []string{}
	}
	a.session.Current = a.prefs.Current
	a.session.Notifications = a.prefs.Notifications == nil || *a.prefs.Notifications
	a.session.DisplayName = a.prefs.DisplayName
	a.session.Identity = cmp.Or(a.prefs.DisplayName, a.baseID)
	if a.client != nil && a.client.Agent != a.session.Identity {
		c := *a.client // a copy: requests in flight keep the old one
		c.Agent = a.session.Identity
		a.client = &c
	}
}

// startup is the OnStartup hook (unexported, so it is not bound).
func (a *App) startup(ctx context.Context) {
	a.mu.Lock()
	if a.native == nil {
		a.native = &wailsNative{ctx: ctx}
	}
	a.root = ctx
	a.mu.Unlock()
	a.restartPoller()
	go a.watchUpdates(ctx)
}

// shutdown is the OnShutdown hook.
func (a *App) shutdown(context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
}

// Session returns the login, identity and project list.
func (a *App) Session() Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.session
}

// AddProject checks that the Worker knows the project, adds it to the list
// and switches to it.
func (a *App) AddProject(name string) (Session, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !validProject(name) {
		return a.Session(), fmt.Errorf("%q is not a project name (lower case letters, digits, '.', '_' and '-')", name)
	}
	c := a.clientOrNil()
	if c == nil {
		return a.Session(), errors.New(a.Session().Problem)
	}
	ctx, cancel := context.WithTimeout(a.context(), 15*time.Second)
	defer cancel()
	if _, err := c.ListIntents(ctx, name, ""); err != nil {
		if api.IsCode(err, api.CodeNotFound) {
			return a.Session(), fmt.Errorf("the Worker has no project %q; create it with 'wayglass init'", name)
		}
		return a.Session(), err
	}
	a.mu.Lock()
	if !slices.Contains(a.prefs.Projects, name) {
		a.prefs.Projects = append(a.prefs.Projects, name)
	}
	a.prefs.Current = name
	err := a.persist()
	a.mu.Unlock()
	a.restartPoller()
	return a.Session(), err
}

// SelectProject switches the window to a project already in the list.
func (a *App) SelectProject(name string) (Session, error) {
	a.mu.Lock()
	if !slices.Contains(a.prefs.Projects, name) {
		a.mu.Unlock()
		return a.Session(), fmt.Errorf("project %q is not in the list; add it first", name)
	}
	a.prefs.Current = name
	err := a.persist()
	a.mu.Unlock()
	a.restartPoller()
	return a.Session(), err
}

// RemoveProject drops a project from the list (not from the Worker).
func (a *App) RemoveProject(name string) (Session, error) {
	a.mu.Lock()
	a.prefs.Projects = slices.DeleteFunc(a.prefs.Projects, func(p string) bool { return p == name })
	if a.prefs.Current == name {
		a.prefs.Current = ""
		if len(a.prefs.Projects) > 0 {
			a.prefs.Current = a.prefs.Projects[0]
		}
	}
	err := a.persist()
	a.mu.Unlock()
	a.restartPoller()
	return a.Session(), err
}

// SetNotifications turns native notifications on or off.
func (a *App) SetNotifications(on bool) (Session, error) {
	a.mu.Lock()
	a.prefs.Notifications = &on
	err := a.persist()
	a.mu.Unlock()
	return a.Session(), err
}

// SetDisplayName changes who this app approves as and saves it in
// desktop.yaml. An empty name goes back to the CLI's identity.
func (a *App) SetDisplayName(name string) (Session, error) {
	name = strings.TrimSpace(name)
	if err := checkDisplayName(name); err != nil {
		return a.Session(), err
	}
	a.mu.Lock()
	a.prefs.DisplayName = name
	err := a.persist()
	live := a.root != nil && a.client != nil && a.prefs.Current != ""
	a.mu.Unlock()
	if live {
		go a.pushNow() // "mine" and "needs you" depend on the identity
	}
	return a.Session(), err
}

// checkDisplayName keeps the name to what the Worker records (at most 128
// characters) and what an HTTP header can carry.
func checkDisplayName(name string) error {
	if len(name) > 128 {
		return errors.New("keep the display name to 128 characters or fewer")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("the display name cannot contain control characters")
		}
	}
	return nil
}

// persist saves prefs and refreshes the session. Callers hold a.mu.
func (a *App) persist() error {
	a.syncSession()
	if err := savePrefs(a.prefsPath, a.prefs); err != nil {
		return fmt.Errorf("save desktop settings: %w", err)
	}
	return nil
}

// Snapshot reads the current project now, bypassing the poll cache.
func (a *App) Snapshot() (Snapshot, error) {
	ctx, cancel := context.WithTimeout(a.context(), requestTimeout)
	defer cancel()
	s, _, err := a.refresh(ctx, true)
	if err != nil {
		return s, errors.New(describe(err))
	}
	return s, nil
}

// describe turns a request error into what the window should say. A 401
// means the stored login is no longer accepted, not that the Worker is
// down, and the fix is a terminal command, so it says so.
func describe(err error) string {
	if api.IsCode(err, api.CodeUnauthorized) {
		return "The Worker no longer accepts the stored login (" + err.Error() + "). Run 'wayglass login' or 'wayglass login --human' in a terminal, then reopen Wayglass."
	}
	return err.Error()
}

// intentIDRe is the Worker's intent id rule (worker/src/index.ts INTENT_ID).
var intentIDRe = regexp.MustCompile(`^i[0-9a-f]{10}$`)

// Approve records the logged-in person's approval of an intent's change.
// decisionID is the decision the window rendered ("" from an older
// frontend); the verdict is bound to it.
func (a *App) Approve(intentID, decisionID, reason string) DecideOutcome {
	return a.decide(intentID, decisionID, "approve", reason)
}

// Reject records the logged-in person's rejection of an intent's change.
func (a *App) Reject(intentID, decisionID, reason string) DecideOutcome {
	return a.decide(intentID, decisionID, "reject", reason)
}

// decide calls the same Worker route as 'wayglass approve|reject'. The app
// refuses what the Worker would refuse anyway (no reason, the author
// deciding its own change, a change not waiting for a person) so a mistake
// costs no request; anything else the Worker refuses, such as a placeholder
// reason, comes back as its own words.
func (a *App) decide(intentID, decisionID, verdict, reason string) DecideOutcome {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return refused("reason_required", "Write why. The reason is kept in the audit log for good.")
	}
	if len(reason) > 2000 {
		return refused("reason_too_long", "Keep the reason under 2,000 characters.")
	}
	if !intentIDRe.MatchString(intentID) {
		return refused(api.CodeBadRequest, fmt.Sprintf("%q is not an intent id.", intentID))
	}
	c := a.clientOrNil()
	// One read of the session: the project and the identity must belong to
	// the same moment, or a project switch mid-click decides the wrong one.
	sess := a.Session()
	if c == nil {
		return refused("not_logged_in", sess.Problem)
	}
	project, identity := sess.Current, sess.Identity
	if project == "" {
		return refused("no_project", "Pick a project first.")
	}
	ctx, cancel := context.WithTimeout(a.context(), requestTimeout)
	defer cancel()

	in, err := c.GetIntent(ctx, project, intentID)
	if err != nil {
		return a.workerRefused(err)
	}
	if sameIdentity(in.Agent, identity) {
		return refused("self_approval", fmt.Sprintf("%s wrote this change, so %s cannot decide it. Someone else must.", in.Agent, identity))
	}
	ds, err := c.ListDecisions(ctx, project, "")
	if missingRoute(err) {
		return refused("not_supported", noDecisionsRoute)
	}
	if err != nil {
		return a.workerRefused(err)
	}
	var latest *api.Decision
	for i := range ds {
		if ds[i].IntentID == intentID {
			latest = &ds[i]
		}
	}
	if latest == nil {
		return refused("not_waiting", "This intent has no proposed change to decide.")
	}
	if latest.Status == api.StatusSuperseded {
		return refused(api.CodeSuperseded, "This intent was re-stacked after it was proposed, so this decision no longer covers the change. Its author proposes again; the new decision will show here.")
	}
	if v, err := decisionView(*latest); err != nil || !v.AwaitsHuman {
		return refused("not_waiting", fmt.Sprintf("This change is %s, not waiting for a person.", strings.ReplaceAll(latest.Status, "_", " ")))
	}
	// multi_approve counts distinct approvers: a second approval from the
	// same person would be recorded but change nothing, and the window would
	// wrongly say "more approvals needed". A reject still goes through.
	if verdict == "approve" {
		for _, ap := range latest.Approvals {
			if ap.Verdict == "approve" && sameIdentity(ap.By, identity) {
				return refused("already_approved", fmt.Sprintf("%s already approved this change; it waits for someone else.", identity))
			}
		}
	}

	// Name the decision the window showed: if the author proposed again while
	// the person read it, the Worker refuses instead of approving the new one.
	if decisionID != "" && decisionID != latest.ID {
		// The window is behind: push the snapshot with the new proposal now
		// rather than leaving the stale card until the next poll notices.
		go a.pushNow()
		return refused(api.CodeDecisionReplaced, "The author proposed this change again while you were reading it. The new proposal is shown now; read it, then decide.")
	}
	res, err := c.DecideIntent(ctx, intentID, api.DecideInput{Project: project, Verdict: verdict, Reason: reason, DecisionID: latest.ID})
	if err != nil {
		return a.workerRefused(err)
	}
	d := res.Decision
	d.Approvals = res.Approvals
	v, err := decisionView(d)
	if err != nil {
		return fromErr(err)
	}
	go a.pushNow() // show the result everywhere without waiting for the next poll
	return DecideOutcome{OK: true, Decision: &v}
}

// AskAI asks the Worker's AI reviewer about an intent's latest decision: a
// verdict while a reviewer agent is awaited (POST /v1/intents/:id/ai-review:
// the AI approves or hands the change to a person), advice once a person
// must decide (POST /v1/intents/:id/ai-advice; it decides nothing). The
// Worker's refusals come back as outcomes in its own words: the reviewer
// off (ai_unavailable), a change only a person may decide (needs_human), a
// Worker without the advice route.
func (a *App) AskAI(intentID string) AIOutcome {
	if !intentIDRe.MatchString(intentID) {
		return AIOutcome{Code: api.CodeBadRequest, Message: fmt.Sprintf("%q is not an intent id.", intentID)}
	}
	c := a.clientOrNil()
	sess := a.Session()
	if c == nil {
		return AIOutcome{Code: "not_logged_in", Message: sess.Problem}
	}
	project := sess.Current
	if project == "" {
		return AIOutcome{Code: "no_project", Message: "Pick a project first."}
	}
	ctx, cancel := context.WithTimeout(a.context(), 90*time.Second) // a model call can take a while
	defer cancel()
	ds, err := c.ListDecisions(ctx, project, "")
	if missingRoute(err) {
		return AIOutcome{Code: "not_supported", Message: noDecisionsRoute}
	}
	if err != nil {
		return aiRefused(err)
	}
	var latest *api.Decision
	for i := range ds {
		if ds[i].IntentID == intentID {
			latest = &ds[i]
		}
	}
	if latest == nil {
		return AIOutcome{Code: "not_waiting", Message: "This intent has no proposed change to ask about."}
	}
	var out AIOutcome
	switch latest.Status {
	case api.StatusPendingAgent:
		r, err := c.AIReviewIntent(ctx, project, intentID)
		if err != nil {
			return aiRefused(err)
		}
		d := r.Decision
		d.Approvals = r.Approvals
		v, err := decisionView(d)
		if err != nil {
			return AIOutcome{Code: "bad_response", Message: err.Error()}
		}
		out = AIOutcome{OK: true, Kind: "review", Model: r.Review.Model, Verdict: r.Review.Verdict, Reason: r.Review.Reason, Concerns: nonNil(r.Review.Concerns), Decision: &v}
	case api.StatusPendingHuman, api.StatusQuarantined:
		r, err := c.AIAdviseIntent(ctx, project, intentID)
		if missingRoute(err) {
			return AIOutcome{Code: api.CodeAIUnavailable, Message: "This Worker has no AI advice route yet (POST /v1/intents/:id/ai-advice). Deploy the current worker/ to ask for advice here."}
		}
		if err != nil {
			return aiRefused(err)
		}
		v, err := decisionView(r.Decision)
		if err != nil {
			return AIOutcome{Code: "bad_response", Message: err.Error()}
		}
		adv := adviceView(r.Advice)
		if v.AIAdvice == nil {
			v.AIAdvice = adv
		}
		out = AIOutcome{OK: true, Kind: "advice", Model: adv.Model, Advice: adv, Decision: &v}
	default:
		return AIOutcome{Code: "not_waiting", Message: fmt.Sprintf("This change is %s; the AI reviews changes waiting for a reviewer agent and advises on those waiting for a person.", strings.ReplaceAll(latest.Status, "_", " "))}
	}
	// Model-written text is as untrusted as an agent's: one plain line each.
	textsafe.CleanAll(&out)
	go a.pushNow() // the verdict or advice shows everywhere without waiting for the next poll
	return out
}

// aiRefused turns the Worker's refusal into an outcome the window shows as
// it is; a 401 says what to run, like everywhere else.
func aiRefused(err error) AIOutcome {
	var e *api.Error
	if errors.As(err, &e) {
		if e.Code == api.CodeUnauthorized {
			return AIOutcome{Code: e.Code, Message: describe(err)}
		}
		return AIOutcome{Code: e.Code, Message: textsafe.Line(e.Message)}
	}
	return AIOutcome{Code: "network", Message: "Could not reach the Worker: " + err.Error()}
}

// noDecisionsRoute explains a Worker deployed before GET /v1/decisions.
const noDecisionsRoute = "Decisions are unavailable: this Worker predates GET /v1/decisions. Intents, overlaps and the audit log still show; deploy the current worker/ to review and decide here."

// missingRoute reports the Worker's "no route" reply, which means the
// deployed Worker is older than this app, not that something failed.
func missingRoute(err error) bool {
	return api.IsCode(err, api.CodeNotFound) && strings.Contains(err.Error(), "no route")
}

func refused(code, msg string) DecideOutcome { return DecideOutcome{Code: code, Message: msg} }

// workerRefused is fromErr, plus a refresh when the Worker's reply means the
// window is behind (a decision superseded, decided or gone since the last
// poll), so the card the person is looking at catches up without waiting.
func (a *App) workerRefused(err error) DecideOutcome {
	out := fromErr(err)
	switch out.Code {
	case api.CodeSuperseded, api.CodeConflict, api.CodeNotFound:
		go a.pushNow()
	}
	return out
}

func fromErr(err error) DecideOutcome {
	var e *api.Error
	if errors.As(err, &e) {
		if e.Code == api.CodeUnauthorized {
			return DecideOutcome{Code: e.Code, Message: describe(err)}
		}
		// The Worker's message quotes the reason the person typed: keep it
		// to one plain line before it goes back into the window.
		return DecideOutcome{Code: e.Code, Message: textsafe.Line(e.Message)}
	}
	return DecideOutcome{Code: "network", Message: "Could not reach the Worker: " + err.Error()}
}

func (a *App) clientOrNil() *api.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

func (a *App) context() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cmp.Or[context.Context](a.root, context.Background())
}

// refresh reads the current project. Without force it reads only the audit
// log when nothing happened, which keeps an idle window to one request per
// poll. changed reports whether the snapshot is new.
func (a *App) refresh(ctx context.Context, force bool) (Snapshot, bool, error) {
	c := a.clientOrNil()
	sess := a.Session()
	if c == nil {
		return Snapshot{}, false, errors.New(sess.Problem)
	}
	if sess.Current == "" {
		return Snapshot{}, false, errors.New("pick a project")
	}
	a.fetchMu.Lock()
	defer a.fetchMu.Unlock()
	if a.project != sess.Current {
		a.project, a.cursor, a.events, a.last, a.dirty = sess.Current, 0, nil, nil, false
		a.notified = map[string]bool{}
	}

	var fresh []EventView
	for range auditPages {
		page, err := c.Audit(ctx, a.project, a.cursor, auditPage)
		if err != nil {
			return Snapshot{}, false, err
		}
		for _, e := range page.Events {
			fresh = append(fresh, eventView(e))
			a.cursor = max(a.cursor, e.Seq)
		}
		a.cursor = max(a.cursor, page.Next)
		if len(page.Events) < auditPage {
			break
		}
	}
	slices.Reverse(fresh)
	a.events = append(fresh, a.events...)
	if len(a.events) > keepEvents {
		a.events = a.events[:keepEvents]
	}
	if len(fresh) > 0 {
		// The audit cursor has moved past these events; if the reads below
		// fail, the next poll sees no news and must still rebuild.
		a.dirty = true
	}

	now := a.now()
	if !force && !a.dirty && a.last != nil && now.Sub(a.lastFull) < fullEvery {
		return *a.last, false, nil
	}
	intents, err := c.ListIntents(ctx, a.project, "")
	if err != nil {
		return Snapshot{}, false, err
	}
	decisions, err := c.ListDecisions(ctx, a.project, "")
	var warnings []string
	if missingRoute(err) { // degrade like the TUI: everything else still shows
		decisions, err = nil, nil
		warnings = append(warnings, noDecisionsRoute)
	}
	if err != nil {
		return Snapshot{}, false, err
	}
	s, err := build(a.project, sess.Identity, intents, decisions, slices.Clone(a.events), now)
	if err != nil {
		return Snapshot{}, false, err
	}
	s.Warnings = append(s.Warnings, warnings...)
	// Everything in s but the counters was written by agents. The window
	// renders it as text, never markup; this also drops escape sequences
	// and bidi controls, which text rendering alone does not stop.
	textsafe.CleanAll(&s)
	a.last, a.lastFull, a.dirty = &s, now, false
	return s, true, nil
}

// restartPoller stops the poller and starts one for the current project.
func (a *App) restartPoller() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	if a.root == nil || a.client == nil || a.prefs.Current == "" {
		return
	}
	ctx, cancel := context.WithCancel(a.root)
	a.cancel = cancel
	go a.follow(ctx)
}

// follow polls like the TUI (internal/tui/poll.go): every interval while the
// Worker answers, backing off to 30 s while it does not.
func (a *App) follow(ctx context.Context) {
	b := tui.NewBackoff(a.interval)
	first := true
	for {
		s, changed, err := a.refresh(ctx, first)
		if ctx.Err() != nil {
			return
		}
		wait := b.Next(err)
		st := PollStatus{OK: err == nil, Failures: b.Failures(), NextMS: wait.Milliseconds(), At: a.now().UTC().Format(time.RFC3339)}
		if err != nil {
			st.Error = describe(err)
		}
		a.emit(EventPoll, st)
		if err == nil && changed {
			a.emit(EventSnapshot, s)
			a.alert(s, first)
			first = false
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// pushNow refreshes and emits outside the poll schedule (after a decision).
func (a *App) pushNow() {
	ctx, cancel := context.WithTimeout(a.context(), 30*time.Second)
	defer cancel()
	if s, _, err := a.refresh(ctx, true); err == nil {
		a.emit(EventSnapshot, s)
		a.alert(s, false)
	}
}

func (a *App) emit(event string, data any) {
	a.mu.Lock()
	n := a.native
	a.mu.Unlock()
	if n != nil {
		n.Emit(event, data)
	}
}

// alert keeps the window title's count current and sends one native
// notification per change newly routed to a person. The first snapshot of a
// project only sets the count: opening the app should not ring for work that
// was already waiting.
func (a *App) alert(s Snapshot, quiet bool) {
	a.mu.Lock()
	n, on := a.native, a.session.Notifications
	a.mu.Unlock()
	if n == nil {
		return
	}
	title := "Wayglass"
	if s.Counters.NeedsYou > 0 {
		verb := "need"
		if s.Counters.NeedsYou == 1 {
			verb = "needs"
		}
		title = fmt.Sprintf("Wayglass · %d %s you", s.Counters.NeedsYou, verb)
	}
	n.SetTitle(title)

	a.fetchMu.Lock()
	var fresh []NeedsYou
	for _, x := range s.NeedsYou {
		if x.Own || a.notified[x.Decision.ID] {
			continue
		}
		a.notified[x.Decision.ID] = true
		fresh = append(fresh, x)
	}
	a.fetchMu.Unlock()
	if quiet || !on {
		return
	}
	for _, x := range fresh {
		if err := n.Notify(actionTitle(x.Decision.Action), notificationBody(x)); err != nil {
			log.Printf("wayglass desktop: notification: %v", err)
		}
	}
}

// notificationBody is one line for the notification centre, which shows it
// outside the window: agent-written text is flattened to plain words and
// kept short, so a goal cannot push the intent id off the banner or carry
// anything but text. The window shows the full goal.
func notificationBody(x NeedsYou) string {
	const maxGoal = 120
	goal := textsafe.Line(x.Intent.Goal)
	if r := []rune(goal); len(r) > maxGoal {
		goal = string(r[:maxGoal-1]) + "…"
	}
	return fmt.Sprintf("%s · %s: %s", textsafe.Line(x.Intent.Agent), x.Intent.ID, goal)
}

func actionTitle(action string) string {
	switch action {
	case "quarantine":
		return "Quarantined: a person must look"
	case "multi_approve":
		return "Needs your approval (one of several)"
	}
	return "Needs your approval"
}
