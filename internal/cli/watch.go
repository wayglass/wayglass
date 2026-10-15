package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/wayglass/wayglass/internal/tui"
)

// tuiFlags are shared by watch and review.
type tuiFlags struct {
	demo     bool
	interval time.Duration
	pick     bool
}

func (f *tuiFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.demo, "demo-data", false, "show built-in SAMPLE data instead of the Worker (labelled on screen and in JSON)")
	_ = cmd.Flags().MarkHidden("demo-data")
	cmd.Flags().DurationVar(&f.interval, "interval", tui.DefaultPollInterval, "poll interval (backs off on errors)")
	cmd.Flags().BoolVar(&f.pick, "pick", false, "start in the project picker (also 'p' while watching): every project this key can read, with / to filter")
}

// source returns the data source for watch and review: the project on the
// Worker, or with --demo-data the built-in sample. live means the sample
// script should play out from now (a person is watching); otherwise it is
// shown complete, so one-shot output has something to show.
func (f *tuiFlags) source(ctx context.Context, a *app, live bool) (tui.DataSource, error) {
	if f.interval <= 0 {
		return nil, UsageError("--interval must be positive")
	}
	if f.demo {
		start := time.Now()
		if !live {
			start = start.Add(-tui.DemoDuration() - time.Second)
		}
		// The sample includes the AI reviewer: an approval, an escalation
		// and advice, and `i` in review answers from the script.
		return tui.NewDemoSource(start, time.Now).WithAIReviewer(), nil
	}
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	w, err := openWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	return &liveSource{c: c, project: w.cfg.Project, jurisdiction: w.cfg.Jurisdiction}, nil
}

func newWatchCmd(a *app) *cobra.Command {
	var (
		flags  tuiFlags
		follow bool
	)
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Watch live intents, tags and decisions (TUI on a terminal)",
		Long: "Shows the swarm at a glance: intents, each change's tags as it is classified,\n" +
			"overlaps and how they were re-planned, decisions with the rule that matched,\n" +
			"and the counters. Only changes routed to a person draw attention.\n\n" +
			"With --json it prints one snapshot; add --follow for one envelope per line\n" +
			"(NDJSON) whenever something changes, until interrupted.\n\n" +
			"On a terminal, 'p' opens the project picker: every project this key can read\n" +
			"(see 'wayglass projects'), / filters, enter switches the view. --pick starts there.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := a.output(cmd)
			if follow && !out.JSON {
				return UsageError("--follow needs --json; on a terminal, watch already follows")
			}
			interactive := out.Interactive()
			ctx := cmd.Context()
			ds, err := flags.source(ctx, a, interactive || follow)
			if err != nil {
				return err
			}
			switch {
			case out.JSON && follow:
				return followJSON(ctx, out.Out, ds, flags.interval)
			case interactive:
				return runProgram(ctx, tui.NewWatch(ds, tui.Options{Interval: flags.interval, Pick: flags.pick}), out)
			}
			s, err := tui.Load(ctx, ds, 0, nil, time.Now())
			if err != nil {
				return err
			}
			return out.Emit(s, func(w io.Writer) error {
				_, err := lipgloss.Fprintln(w, tui.RenderWatch(s, tui.WatchLayout{Styles: tui.NewStyles(true), Width: 100}))
				return err
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().BoolVar(&follow, "follow", false, "with --json, keep polling and print an envelope per change (NDJSON)")
	return cmd
}

// followJSON streams one envelope per snapshot with news. Poll errors become
// error envelopes on their own line, and polling continues with backoff.
func followJSON(ctx context.Context, w io.Writer, ds tui.DataSource, interval time.Duration) error {
	return tui.Follow(ctx, ds, interval, time.Now, func(s tui.Snapshot, err error) error {
		if err != nil {
			return writeEnvelope(w, Envelope{V: EnvelopeVersion, OK: false, Error: &ErrorBody{Code: CodeError, Message: err.Error()}})
		}
		return writeEnvelope(w, Envelope{V: EnvelopeVersion, OK: true, Data: s})
	})
}

// runProgram runs a full-screen model. The renderer picks the colour profile
// from the terminal and the environment, so NO_COLOR is honoured.
func runProgram(ctx context.Context, m tea.Model, out Output) error {
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithOutput(rawStream(out.Out)), tea.WithInput(os.Stdin))
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil // interrupted by a signal: a normal way to leave
	}
	return err
}
