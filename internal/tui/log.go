package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/wayglass/wayglass/internal/textsafe"
)

// Tone is how much attention a log row asks for.
type Tone int

// Tones, from calm to loud. Only ToneAttention uses the attention colour.
const (
	ToneNone Tone = iota
	ToneGood
	ToneAttention
	ToneStopped
)

// LogRow is one audit event as `wayglass log` shows it.
type LogRow struct {
	Seq    int
	At     time.Time
	Actor  string
	Kind   string
	Intent string
	Detail string
	Tone   Tone
}

// RenderLog renders audit events as a calm table, oldest first: seq, time,
// actor, event, intent and a one-line summary truncated to width. A text
// marker repeats every coloured signal so pipes lose nothing.
func RenderLog(rows []LogRow, st Styles, width int, loc *time.Location) string {
	if len(rows) == 0 {
		return st.Muted.Render("No audit events yet.") + "\n"
	}
	seqW := len(fmt.Sprint(rows[len(rows)-1].Seq))
	actorW, kindW := len("ACTOR"), len("EVENT")
	for _, r := range rows {
		actorW = max(actorW, min(16, len(r.Actor)))
		kindW = max(kindW, len(r.Kind))
	}
	const timeW, intentW = len("01-02 15:04:05"), 11
	fixed := seqW + timeW + actorW + kindW + intentW + 5*2
	detailW := max(20, width-fixed)

	var b strings.Builder
	b.WriteString(st.Header.Render(joinCols(
		fit("#", seqW), fit("TIME", timeW), fit("ACTOR", actorW), fit("EVENT", kindW), fit("INTENT", intentW), "DETAIL",
	)))
	b.WriteString("\n")
	for _, r := range rows {
		at := "--"
		if !r.At.IsZero() {
			at = r.At.In(loc).Format("01-02 15:04:05")
		}
		r.Actor, r.Kind, r.Intent = textsafe.Line(r.Actor), textsafe.Line(r.Kind), textsafe.Line(r.Intent)
		detail := textsafe.Line(r.Detail)
		if r.Tone == ToneAttention {
			detail = "▲ " + detail
		}
		b.WriteString(joinCols(
			st.Muted.Render(fmt.Sprintf("%*d", seqW, r.Seq)),
			st.Muted.Render(fit(at, timeW)),
			st.Text.Render(fit(r.Actor, actorW)),
			st.Text.Render(fit(r.Kind, kindW)),
			st.Muted.Render(fit(orDash(r.Intent), intentW)),
			st.tone(r.Tone).Render(clip(detail, detailW)),
		))
		b.WriteString("\n")
	}
	return b.String()
}

func (st Styles) tone(t Tone) lipgloss.Style {
	switch t {
	case ToneGood:
		return st.Good
	case ToneAttention:
		return st.Attention
	case ToneStopped:
		return st.Stopped
	}
	return st.Text
}
