package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/api"
	"github.com/wayglass/wayglass/internal/notes"
	"github.com/wayglass/wayglass/internal/tui"
)

type whyResult struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Commit string `json:"commit"`
	// Source is "note" (refs/notes/wayglass in this checkout) or "worker"
	// (GET /v1/why).
	Source string         `json:"source"`
	Note   *notes.Note    `json:"note,omitempty"`
	Worker *api.WhyResult `json:"worker,omitempty"`
	// Verified says whether the answer is backed by the Worker's record:
	// "worker" (it is, or it came from the Worker), "mismatch" (the note is
	// not: anyone who can push notes can write one, so it is not trusted)
	// or "unchecked" (the Worker could not be asked).
	Verified string `json:"verified"`
	// Warning explains a mismatch or an unchecked note.
	Warning string `json:"warning,omitempty"`
}

func newWhyCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "why <file>:<line>",
		Short: "Show the intent, decision and reason behind a line",
		Long: "Blames the line to its commit, then reads the commit's note under\n" +
			"refs/notes/wayglass. Without a note it asks the Worker's audit record.",
		Example: "  wayglass why web/login.ts:42",
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, line, err := parseFileLine(args[0])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			w, err := openRepo(ctx)
			if err != nil {
				return err
			}
			sha, err := w.git.BlameLine(ctx, file, line)
			if err != nil {
				return errorf(CodeNoWhy, ExitError, "cannot blame %s:%d: %v", file, line, err)
			}
			res := whyResult{File: file, Line: line, Commit: sha}
			n, err := notes.Read(ctx, w.git, sha)
			switch {
			case err == nil:
				res.Source, res.Note = "note", &n
				// A note is written by whoever pushed refs/notes/wayglass, so
				// it is checked against the Worker's record before it is
				// believed.
				var wr *api.WhyResult
				werr := errors.New("no workspace or login")
				if ww, oerr := openWorkspace(ctx); oerr == nil {
					if c, cerr := a.client(); cerr == nil {
						wr, werr = c.Why(ctx, ww.cfg.Project, sha)
					}
				}
				checkNote(&res, wr, werr)
			case errors.Is(err, notes.ErrNoNote):
				ww, werr := openWorkspace(ctx)
				c, cerr := a.client()
				if werr != nil || cerr != nil {
					return errorf(CodeNoWhy, ExitError, "commit %s has no Wayglass note, and the Worker cannot be asked (%v)", short(sha), errors.Join(werr, cerr))
				}
				wr, err := c.Why(ctx, ww.cfg.Project, sha)
				if api.IsCode(err, api.CodeNotFound) {
					return errorf(CodeNoWhy, ExitError, "commit %s has no Wayglass note or recorded decision", short(sha))
				}
				if err != nil {
					return err
				}
				res.Source, res.Worker, res.Verified = "worker", wr, "worker"
			default:
				return err
			}
			return a.output(cmd).Emit(res, func(out io.Writer) error { return renderWhy(out, cliStyles(a.output(cmd)), res) })
		},
	}
}

// checkNote compares the note in res with the Worker's record for the same
// commit (wr, or the error asking for it) and sets Verified. A note that
// matches gains the Worker's approvals when it has none (notes written
// before approvals were recorded). A note that does not match is shown
// only as a warning, and the Worker's record, if any, answers instead.
func checkNote(res *whyResult, wr *api.WhyResult, werr error) {
	n := res.Note
	switch {
	case werr == nil && noteMatches(n, wr):
		res.Verified = "worker"
		if len(n.Approvals) == 0 && len(wr.Approvals) > 0 {
			n.Approvals = toNoteApprovals(wr.Approvals)
		}
	case werr == nil:
		res.Verified, res.Source, res.Worker = "mismatch", "worker", wr
		res.Warning = "the git note on this commit does not match the Worker's record; showing the Worker's record (the note may be forged)"
	case api.IsCode(werr, api.CodeNotFound):
		res.Verified = "mismatch"
		res.Warning = "the Worker has no decision for this commit; the git note is not backed by any recorded decision and may be forged"
	default:
		res.Verified = "unchecked"
		res.Warning = fmt.Sprintf("could not check the git note against the Worker (%v); it is unverified", werr)
	}
}

// noteMatches reports whether every field why shows from the note is what
// the Worker recorded: the ids, the action, the intent's words, who decided
// and, when the note carries approvals, each approver's verdict and reason.
// A note that copies the public record but changes the decider or an
// approval reason is a forgery like any other.
func noteMatches(n *notes.Note, wr *api.WhyResult) bool {
	if wr.Decision.ID != n.DecisionID || wr.Intent.ID != n.IntentID || wr.Decision.Action != n.Action ||
		wr.Intent.Goal != n.Goal || wr.Intent.Reason != n.Reason {
		return false
	}
	if cmp.Or(deref(wr.Decision.DecidedBy), "policy") != n.DecidedBy {
		return false
	}
	if len(n.Approvals) == 0 {
		return true // written before approvals were recorded; filled from the Worker
	}
	if len(n.Approvals) != len(wr.Approvals) {
		return false
	}
	for i, a := range n.Approvals {
		w := wr.Approvals[i]
		if a.By != w.By || a.Verdict != w.Verdict || a.Reason != w.Reason {
			return false
		}
	}
	return true
}

func renderWhy(out io.Writer, st styles, r whyResult) error {
	_, _ = lipgloss.Fprintf(out, "%s was last changed in %s\n", st.title.Render(fmt.Sprintf("%s:%d", r.File, r.Line)), st.id(short(r.Commit)))
	if r.Warning != "" {
		_, _ = lipgloss.Fprintln(out, st.attention.Render("WARNING:")+" "+r.Warning)
	}
	var rows []field
	if n := r.Note; n != nil && r.Source == "note" {
		claimed := ""
		for _, ap := range n.Approvals {
			if ap.By == n.DecidedBy {
				claimed = ap.ClaimedBy
			}
		}
		rows = append(rows,
			field{"Intent", st.id(n.IntentID) + "  " + n.Goal},
			field{"Why", n.Reason},
			field{"Tags", fmt.Sprintf("area %s; data class %s; risk %s; jurisdiction %s",
				listOrNone(n.Tags.Areas), listOrNone(n.Tags.DataClasses), n.Tags.Risk, n.Tags.Jurisdiction)},
			field{"Decision", fmt.Sprintf("%s by rule %s, decided by %s at %s", st.action(n.Action), n.Rule, approverName(n.DecidedBy, claimed, n.IdentitySource), formatLocal(n.DecidedAt))},
		)
		for _, ap := range n.Approvals {
			rows = append(rows, verdictField(st, ap.Verdict, approverName(ap.By, ap.ClaimedBy, ap.IdentitySource), ap.Reason))
		}
		fields(out, st, rows...)
		return nil
	}
	wr := r.Worker
	d := wr.Decision
	decision := fmt.Sprintf("%s (%s) by rule %s: %s", st.action(d.Action), st.status(d.Status), deref(d.MatchedRule), d.Reason)
	rows = append(rows,
		field{"Intent", st.id(wr.Intent.ID) + "  " + wr.Intent.Goal},
		field{"Why", wr.Intent.Reason},
		field{"Decision", decision},
	)
	if by := deref(d.DecidedBy); by != "" {
		src, claimed := "", ""
		for _, ap := range wr.Approvals {
			if ap.By == by {
				src, claimed = ap.IdentitySource, ap.ClaimedBy
			}
		}
		rows = append(rows, field{"", "decided by " + approverName(by, claimed, src) + " at " + localTime(deref(d.DecidedAt))})
	}
	if e := d.Detail.Escalation; e != nil {
		rows = append(rows, escalationField(st, *e))
	}
	for _, ap := range wr.Approvals {
		rows = append(rows, verdictField(st, ap.Verdict, approverName(ap.By, ap.ClaimedBy, ap.IdentitySource), ap.Reason))
	}
	if a := d.Detail.AIAdvice; a != nil {
		rows = append(rows, field{"AI advice", "from " + aiReviewerName(a.ModelOf()) + ", advisory only: " + a.Summary})
	}
	fields(out, st, rows...)
	return nil
}

// withSource names a decider by how they signed in: "you@example.com
// (Access)" for Cloudflare Access, "AI reviewer (<model>)" for the Workers AI
// reviewer. Other sources read as the plain name.
func withSource(by, source string) string {
	switch {
	case source == api.SourceAccess:
		return by + " (Access)"
	case source == api.SourceAI || api.IsAIReviewer(by):
		return aiReviewerName(api.AIModel(by))
	}
	return by
}

// approverName is how a verdict's author reads: the identity it counts as,
// then the name the admin key reported, if any: "admin (john)". A bound key
// cannot name itself, so its name stands alone. The AI reviewer reads as
// "AI reviewer (<model>)".
func approverName(by, claimedBy, source string) string {
	if claimedBy != "" && claimedBy != by {
		by += " (" + claimedBy + ")"
	}
	return withSource(by, source)
}

// aiReviewerName is the AI reviewer as why and log name it.
func aiReviewerName(model string) string {
	if model == "" {
		return "AI reviewer"
	}
	return "AI reviewer (" + model + ")"
}

// verdictField reads "Approved: john: reason" / "Rejected: claude-reviewer: reason",
// and "Approved: by AI reviewer (<model>): reason" when the AI decided.
func verdictField(st styles, verdict, by, reason string) field {
	if strings.HasPrefix(by, "AI reviewer") {
		by = "by " + by
	}
	switch verdict {
	case "approve":
		return field{"Approved", st.good.Render(by) + ": " + reason}
	case "reject":
		return field{"Rejected", st.stopped.Render(by) + ": " + reason}
	}
	return field{verdict, by + ": " + reason}
}

// escalationField reads "Escalated: by AI reviewer (<model>): reason; concerns: a; b".
func escalationField(st styles, e api.Escalation) field {
	v := st.attention.Render("by "+aiReviewerName(e.ModelOf())) + ": " + e.Reason
	if len(e.Concerns) > 0 {
		v += "\nconcerns: " + strings.Join(e.Concerns, "; ")
	}
	if e.FromAction != "" {
		v += "\nwas " + e.FromAction + "; a person decided"
	}
	return field{"Escalated", v}
}

// parseFileLine splits "path/to/file.go:42". The last colon separates the
// line, so paths containing colons still work.
func parseFileLine(s string) (string, int, error) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", 0, UsageError("want <file>:<line>, got %q", s)
	}
	n, err := strconv.Atoi(s[i+1:])
	if err != nil || n < 1 {
		return "", 0, UsageError("line must be a positive number, got %q", s[i+1:])
	}
	return s[:i], n, nil
}

// auditPageSize is the most events the Worker returns per request.
const auditPageSize = 500

// maxAuditPages bounds how much of a long log `wayglass log` reads.
const maxAuditPages = 40

type logResult struct {
	Project string           `json:"project"`
	Events  []api.AuditEvent `json:"events"`
	// Next is the seq to pass as --since to continue after these events.
	Next int `json:"next"`
	// Truncated is true when the log was longer than log could read; use
	// --since to read further.
	Truncated bool `json:"truncated"`
}

func newLogCmd(a *app) *cobra.Command {
	var limit, since int
	var intentID, kind string
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show the audit log: every decision, who made it and why",
		Long: "Reads the project's append-only audit log from the Worker and shows the\n" +
			"most recent events: intents declared and pushed, overlaps, decisions with\n" +
			"their rule and reason, approvals with theirs, and lands.\n" +
			"--json returns the raw events, oldest first.",
		Example: "  wayglass log --limit 20\n  wayglass log --intent i5e39df9bcd --json\n  wayglass log --kind decision",
		Args:    usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 1 {
				return UsageError("--limit must be at least 1")
			}
			if since < 0 {
				return UsageError("--since must not be negative")
			}
			ctx := cmd.Context()
			c, err := a.client()
			if err != nil {
				return err
			}
			w, err := openWorkspace(ctx)
			if err != nil {
				return err
			}
			res := logResult{Project: w.cfg.Project, Events: []api.AuditEvent{}, Next: since}
			cursor := since
			for page := 0; ; page++ {
				if page == maxAuditPages {
					res.Truncated = true
					break
				}
				p, err := c.Audit(ctx, w.cfg.Project, cursor, auditPageSize)
				if err != nil {
					return err
				}
				for _, e := range p.Events {
					if keepEvent(e, intentID, kind) {
						res.Events = append(res.Events, e)
					}
				}
				if len(p.Events) > 0 {
					cursor = p.Events[len(p.Events)-1].Seq
				}
				if len(p.Events) < auditPageSize {
					break
				}
			}
			res.Next = cursor
			if len(res.Events) > limit {
				res.Events = res.Events[len(res.Events)-limit:]
			}
			return a.output(cmd).Emit(res, func(out io.Writer) error {
				rows := make([]tui.LogRow, 0, len(res.Events))
				for _, e := range res.Events {
					rows = append(rows, logRow(e))
				}
				_, err := lipgloss.Fprint(out, tui.RenderLog(rows, tui.NewStyles(true), termWidth(out, 160), time.Local))
				return err
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "show at most this many of the most recent events")
	cmd.Flags().IntVar(&since, "since", 0, "only events after this seq")
	cmd.Flags().StringVar(&intentID, "intent", "", "only events for this intent")
	cmd.Flags().StringVar(&kind, "kind", "", "only events whose kind starts with this (e.g. decision, land)")
	return cmd
}

func keepEvent(e api.AuditEvent, intentID, kind string) bool {
	if intentID != "" && deref(e.IntentID) != intentID {
		return false
	}
	return kind == "" || strings.HasPrefix(e.Kind, kind)
}

// auditData is the subset of event data fields log summarises.
type auditData struct {
	Goal        string   `json:"goal"`
	Paths       []string `json:"paths"`
	Commit      string   `json:"commit"`
	Action      string   `json:"action"`
	Status      string   `json:"status"`
	Rule        string   `json:"matched_rule"`
	Reason      string   `json:"reason"`
	Problems    []string `json:"problems"`
	StackedOn   string   `json:"stacked_on"`
	TrunkBefore string   `json:"trunk_before"`
	Ref         string   `json:"ref"`
	After       string   `json:"after"`
	Repo        string   `json:"repo"`
	// decision.superseded: what the decision was, and who approved it.
	PreviousStatus    string `json:"previous_status"`
	PreviousDecidedBy string `json:"previous_decided_by"`
	Overlaps          []struct {
		IntentID string `json:"intent_id"`
	} `json:"overlaps"`
	// The AI reviewer's entries (decision.approve by it, decision.escalated,
	// decision.ai_advice).
	IdentitySource string   `json:"identity_source"`
	Concerns       []string `json:"concerns"`
	Summary        string   `json:"summary"`
	Risks          []string `json:"risks"`
	Questions      []string `json:"questions"`
	AI             *struct {
		Model string `json:"model"`
	} `json:"ai"`
}

// aiModelOf is the model an AI reviewer's audit entry names.
func aiModelOf(e api.AuditEvent, d auditData) string {
	if d.AI != nil && d.AI.Model != "" {
		return d.AI.Model
	}
	return api.AIModel(e.Actor)
}

// logRow turns one audit event into a table row with a one-line summary.
func logRow(e api.AuditEvent) tui.LogRow {
	var d auditData
	_ = json.Unmarshal(e.Data, &d)
	at, _ := time.Parse(time.RFC3339Nano, e.At)
	r := tui.LogRow{Seq: e.Seq, At: at, Actor: e.Actor, Kind: e.Kind, Intent: deref(e.IntentID)}
	switch e.Kind {
	case "intent.declared":
		r.Detail = d.Goal
		if d.StackedOn != "" {
			r.Detail += " (stacked on " + d.StackedOn + ")"
		}
	case "intent.pushed":
		r.Detail = fmt.Sprintf("%s, %d file(s)", short(d.Commit), len(d.Paths))
	case "intent.overlap":
		ids := make([]string, 0, len(d.Overlaps))
		for _, o := range d.Overlaps {
			ids = append(ids, o.IntentID)
		}
		// Overlaps are common with many agents; they inform, they do not alarm.
		r.Detail = "overlaps " + strings.Join(ids, ", ")
	case "decision.proposed":
		r.Detail = fmt.Sprintf("%s (%s) by %s: %s", d.Action, d.Status, cmp.Or(d.Rule, "default"), d.Reason)
		r.Tone = toneForStatus(d.Status)
	case "decision.approve":
		r.Detail, r.Tone = "approved: "+d.Reason, toneForStatus(d.Status)
		if d.IdentitySource == api.SourceAI || api.IsAIReviewer(e.Actor) {
			r.Detail = "approved by " + aiReviewerName(aiModelOf(e, d)) + ": " + d.Reason
		}
	case "decision.escalated":
		// The AI could not approve this alone: it now waits for a person.
		r.Detail = "escalated to a person by " + aiReviewerName(aiModelOf(e, d)) + ": " + d.Reason
		if len(d.Concerns) > 0 {
			r.Detail += " (concerns: " + strings.Join(d.Concerns, "; ") + ")"
		}
		r.Tone = tui.ToneAttention
	case "decision.ai_advice":
		r.Detail = "AI advice from " + aiReviewerName(aiModelOf(e, d)) + ": " + cmp.Or(d.Summary, d.Reason)
		if n := len(d.Risks) + len(d.Concerns) + len(d.Questions); n > 0 {
			r.Detail += fmt.Sprintf(" (%d point(s); advisory)", n)
		}
	case "decision.reject":
		r.Detail, r.Tone = "rejected: "+d.Reason, tui.ToneStopped
	case "decision.superseded":
		// The approval stays in the log above; this row says it no longer counts.
		r.Detail = "superseded " + cmp.Or(d.PreviousStatus, "decision")
		if d.PreviousStatus == api.StatusApproved && d.PreviousDecidedBy != "" {
			r.Detail = "superseded approval by " + d.PreviousDecidedBy
		}
		r.Detail += ": " + d.Reason
	case "intent.landed":
		r.Detail, r.Tone = "landed "+short(d.Commit), tui.ToneGood
	case "land.mismatch":
		r.Detail, r.Tone = fmt.Sprintf("trunk %s is not the approved change: %s", short(d.Commit), strings.Join(d.Problems, "; ")), tui.ToneStopped
	case "land.aborted", "land.cancelled":
		r.Detail = d.Reason
	case "land.reserved":
		r.Detail = "for " + short(d.Commit)
	case "land.token_issued":
		r.Detail = "trunk write token, 300 s"
		if d.TrunkBefore != "" {
			r.Detail += ", trunk at " + short(d.TrunkBefore)
		}
	case "artifacts.repo.pushed":
		r.Detail = fmt.Sprintf("%s %s -> %s", d.Repo, d.Ref, short(d.After))
	case "token.refreshed":
		r.Detail = "fork write token"
	case "project.created":
		r.Detail = d.Repo
	}
	return r
}

func toneForStatus(status string) tui.Tone {
	switch status {
	case api.StatusApproved:
		return tui.ToneGood
	case api.StatusPendingHuman, api.StatusQuarantined:
		return tui.ToneAttention
	case api.StatusBlocked, api.StatusRejected:
		return tui.ToneStopped
	}
	return tui.ToneNone
}

// termWidth is the terminal's width when out is one, else fallback (pipes
// get full lines up to fallback cells).
func termWidth(out io.Writer, fallback int) int {
	if f, ok := osFile(out); ok && isTerminal(f) {
		if w, _, err := term.GetSize(f.Fd()); err == nil && w > 0 {
			return w
		}
	}
	return fallback
}
