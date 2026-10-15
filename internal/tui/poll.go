package tui

import (
	"context"
	"time"
)

// Backoff decides how long to wait before the next poll. It polls at Base
// while things work and doubles the wait after each consecutive failure, up
// to Max, so a Worker outage does not turn into a request storm. There are no
// websockets yet; this is the whole live-update mechanism.
type Backoff struct {
	Base     time.Duration
	Max      time.Duration
	failures int
}

// DefaultPollInterval is the poll interval against the Worker.
const DefaultPollInterval = 2 * time.Second

// MaxPollInterval caps the backoff.
const MaxPollInterval = 30 * time.Second

// NewBackoff returns a backoff with the given base interval.
func NewBackoff(base time.Duration) Backoff {
	if base <= 0 {
		base = DefaultPollInterval
	}
	return Backoff{Base: base, Max: max(base, MaxPollInterval)}
}

// Next records the outcome of a poll and returns the wait before the next.
func (b *Backoff) Next(err error) time.Duration {
	if err == nil {
		b.failures = 0
		return b.Base
	}
	b.failures++
	d := b.Base
	for range b.failures {
		d *= 2
		if d >= b.Max {
			return b.Max
		}
	}
	return d
}

// Failures is the number of consecutive failed polls.
func (b *Backoff) Failures() int { return b.failures }

// Follow polls ds until ctx ends, calling emit with each snapshot that has
// news: the first snapshot, then any with new audit events. Each snapshot
// carries only the events since the previous one. Errors are passed to emit as
// well, and the wait backs off. emit returning an error stops the loop.
func Follow(ctx context.Context, ds DataSource, interval time.Duration, now func() time.Time, emit func(Snapshot, error) error) error {
	b := NewBackoff(interval)
	var (
		cursor int64
		first  = true
	)
	for {
		s, err := Load(ctx, ds, cursor, nil, now())
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case err != nil:
			if eerr := emit(Snapshot{}, err); eerr != nil {
				return eerr
			}
		case first || s.Cursor > cursor:
			if eerr := emit(s, nil); eerr != nil {
				return eerr
			}
			cursor, first = s.Cursor, false
		}
		t := time.NewTimer(b.Next(err))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}
