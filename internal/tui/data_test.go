package tui

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/policy"
)

var t0 = time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)

// fixedClock is a settable clock for deterministic tests.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

// demoAt returns sample data with the script played up to offset.
func demoAt(offset time.Duration) (*DemoSource, *fixedClock) {
	c := &fixedClock{t: t0.Add(offset)}
	return NewDemoSource(t0, c.now), c
}

func load(t *testing.T, ds DataSource) Snapshot {
	t.Helper()
	s, err := Load(t.Context(), ds, 0, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDemoDecisionsComeFromRealPolicy(t *testing.T) {
	ds, _ := demoAt(time.Minute)
	s := load(t, ds)
	want := map[string]struct {
		action policy.Action
		rule   string
		status string
	}{
		demoDocs:  {policy.AutoMerge, "docs-auto", StatusApproved},
		demoAuth:  {policy.HumanApprove, "auth-needs-human", StatusPendingHuman},
		demoPII:   {policy.Quarantine, "eu-pii-quarantine", StatusQuarantined},
		demoInfra: {policy.MultiApprove, "infra-two-people", StatusPendingHuman},
		demoStyle: {policy.AgentReview, "low-risk-agent-review", StatusApproved},
	}
	if len(s.Decisions) != len(want) {
		t.Fatalf("got %d decisions, want %d", len(s.Decisions), len(want))
	}
	for _, d := range s.Decisions {
		w, ok := want[d.IntentID]
		if !ok {
			t.Fatalf("unexpected decision for %s", d.IntentID)
		}
		if d.Action != w.action || d.MatchedRule != w.rule || d.Status != w.status {
			t.Errorf("%s: got %s/%s/%s, want %s/%s/%s", d.IntentID, d.Action, d.MatchedRule, d.Status, w.action, w.rule, w.status)
		}
	}
	if !s.Sample {
		t.Error("demo snapshot must be marked as sample data")
	}
}

func TestCount(t *testing.T) {
	ds, _ := demoAt(time.Minute)
	c := load(t, ds).Counters
	want := Counters{
		Agents: 5, Intents: 6, OpenIntents: 4, Decisions: 5, AutoLanded: 2, HumanRouted: 3, AwaitingHuman: 3,
		Blocked: 0, Overlaps: 1, TimeSavedMinutes: 2 * MinutesSavedPerChange, TimeSavedBasis: c.TimeSavedBasis,
		ByAction: map[policy.Action]int{
			policy.AutoMerge: 1, policy.AgentReview: 1, policy.HumanApprove: 1,
			policy.MultiApprove: 1, policy.Quarantine: 1, policy.Block: 0,
		},
		LastEventAt: c.LastEventAt,
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("counters = %+v\nwant %+v", c, want)
	}
	if c.TimeSavedBasis == "" {
		t.Error("time saved must always say it is an estimate")
	}
}

func TestQueueOnlyHoldsHumanRoutedPending(t *testing.T) {
	cases := []struct {
		action policy.Action
		status string
		want   bool
	}{
		{policy.HumanApprove, StatusPendingHuman, true},
		{policy.MultiApprove, StatusPendingHuman, true},
		{policy.Quarantine, StatusQuarantined, true},
		{policy.HumanApprove, StatusApproved, false},
		{policy.AgentReview, StatusPendingAgent, false},
		{policy.AutoMerge, StatusApproved, false},
		{policy.Block, StatusBlocked, false},
	}
	for _, tc := range cases {
		q := Queue([]Decision{{ID: "d", Action: tc.action, Status: tc.status}})
		if got := len(q) == 1; got != tc.want {
			t.Errorf("%s/%s in queue = %v, want %v", tc.action, tc.status, got, tc.want)
		}
	}
}

func TestBackoff(t *testing.T) {
	b := NewBackoff(2 * time.Second)
	fail := errors.New("down")
	steps := []struct {
		err  error
		want time.Duration
	}{
		{nil, 2 * time.Second},
		{fail, 4 * time.Second},
		{fail, 8 * time.Second},
		{fail, 16 * time.Second},
		{fail, 30 * time.Second},
		{fail, 30 * time.Second},
		{nil, 2 * time.Second},
	}
	for i, s := range steps {
		if got := b.Next(s.err); got != s.want {
			t.Fatalf("step %d: got %v, want %v", i, got, s.want)
		}
	}
	if b.Failures() != 0 {
		t.Fatal("success must reset failures")
	}
}

func TestLoadIsIncremental(t *testing.T) {
	ds, clock := demoAt(2 * time.Second)
	first := load(t, ds)
	clock.t = t0.Add(time.Minute)
	next, err := Load(t.Context(), ds, first.Cursor, first.Events, t0)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range next.Events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: events must be gap-free and stable", i, e.Seq)
		}
	}
	if next.Cursor <= first.Cursor {
		t.Fatalf("cursor did not advance: %d -> %d", first.Cursor, next.Cursor)
	}
}

func TestFollowEmitsOnlyNews(t *testing.T) {
	ds, clock := demoAt(time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var got []Snapshot
	err := Follow(ctx, ds, time.Millisecond, clock.now, func(s Snapshot, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, s)
		if len(got) == 1 {
			// Nothing new happens after the script ends; a person approves.
			if _, err := ds.Decide(ctx, "d3f2a91c0d1", Approve, "ana", "Limits look right."); err != nil {
				t.Fatal(err)
			}
			return nil
		}
		cancel()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(got))
	}
	for _, e := range got[1].Events {
		if e.Seq <= got[0].Cursor {
			t.Fatalf("second snapshot repeated event %d", e.Seq)
		}
	}
}

func TestDemoDecide(t *testing.T) {
	ctx := t.Context()
	ds, _ := demoAt(time.Minute)

	if _, err := ds.Decide(ctx, "d3f2a91c0d1", Approve, "ana", "  "); err == nil {
		t.Fatal("a blank reason must be refused")
	}
	if _, err := ds.Decide(ctx, "dnope", Approve, "ana", "ok then"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := ds.Decide(ctx, "d1c2d3e4f5a", Approve, "ana", "ok then"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("err = %v, want ErrNotPending", err)
	}

	// multi_approve needs two people; maya already approved in the script.
	d, err := ds.Decide(ctx, "d7b4e0d2a55", Approve, "ana", "Second pair of eyes.")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusApproved || d.Approved() != 2 {
		t.Fatalf("after second approval: status %s, approvals %d", d.Status, d.Approved())
	}

	d, err = ds.Decide(ctx, "d0c9d8e7f61", Reject, "ana", "No DPO sign-off yet.")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusRejected || d.DecidedBy != "ana" {
		t.Fatalf("reject: %+v", d)
	}
	if n := len(Queue(load(t, ds).Decisions)); n != 1 {
		t.Fatalf("queue has %d, want 1", n)
	}
}

func TestFindOverlapsSkipsStackedPairs(t *testing.T) {
	parent := Intent{ID: "i-parent", Status: IntentOpen, CreatedAt: t0, DeclaredPaths: []string{"docs/**"}, ActualPaths: []string{"docs/usage.md"}}
	child := Intent{ID: "i-child", Status: IntentOpen, CreatedAt: t0.Add(time.Minute), DeclaredPaths: []string{"docs/**"}, Parent: "i-parent"}
	other := Intent{ID: "i-other", Status: IntentOpen, CreatedAt: t0.Add(2 * time.Minute), DeclaredPaths: []string{"docs/usage.md"}}
	got := FindOverlaps([]Intent{parent, child, other})
	for _, o := range got {
		if (o.A == "i-parent" && o.B == "i-child") || (o.A == "i-child" && o.B == "i-parent") {
			t.Fatalf("a stacked pair was reported as an overlap: %+v", o)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want the two non-stacked overlaps, got %+v", got)
	}
}

func TestFindOverlapsSkipsWholeStacks(t *testing.T) {
	g := Intent{ID: "i-g", Status: IntentOpen, CreatedAt: t0, DeclaredPaths: []string{"web/**"}}
	p := Intent{ID: "i-p", Status: IntentProposed, CreatedAt: t0.Add(time.Minute), DeclaredPaths: []string{"web/**"}, Parent: "i-g"}
	c := Intent{ID: "i-c", Status: IntentOpen, CreatedAt: t0.Add(2 * time.Minute), DeclaredPaths: []string{"web/**"}, Parent: "i-p"}
	gone := Intent{ID: "i-gone", Status: IntentAbandoned, CreatedAt: t0, DeclaredPaths: []string{"web/**"}}
	if got := FindOverlaps([]Intent{g, p, c, gone}); len(got) != 0 {
		t.Fatalf("a three-deep stack and an abandoned intent are no overlaps: %+v", got)
	}
}

func TestAbandonedDecisionsLeaveTheQueues(t *testing.T) {
	human := Decision{ID: "d-1", Action: "human_approve", Status: StatusPendingHuman, IntentStatus: IntentProposed}
	agent := Decision{ID: "d-2", Action: "agent_review", Status: StatusPendingAgent, IntentStatus: IntentProposed}
	droppedHuman, droppedAgent := human, agent
	droppedHuman.ID, droppedHuman.IntentStatus = "d-3", IntentAbandoned
	droppedAgent.ID, droppedAgent.IntentStatus = "d-4", IntentAbandoned
	all := []Decision{human, agent, droppedHuman, droppedAgent}
	if q := Queue(all); len(q) != 1 || q[0].ID != "d-1" {
		t.Fatalf("Queue = %+v", q)
	}
	if q := AgentQueue(all); len(q) != 1 || q[0].ID != "d-2" {
		t.Fatalf("AgentQueue = %+v", q)
	}
	if c := Count(nil, nil, all); c.AwaitingHuman != 1 {
		t.Fatalf("AwaitingHuman = %d", c.AwaitingHuman)
	}
}

// An intent proposed again has two pending decisions on the Worker; only
// the latest can be decided (decision_replaced otherwise), so the queues
// show that one and the counters count it once.
func TestReplacedDecisionsLeaveTheQueues(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	old := Decision{ID: "d-old", IntentID: "i-1", Action: "human_approve", Status: StatusPendingHuman, IntentStatus: IntentProposed, CreatedAt: t0}
	replacement := old
	replacement.ID, replacement.CreatedAt = "d-new", t0.Add(time.Minute)
	otherOld := Decision{ID: "d-a-old", IntentID: "i-2", Action: "agent_review", Status: StatusPendingAgent, IntentStatus: IntentProposed, CreatedAt: t0}
	otherNew := otherOld
	otherNew.ID, otherNew.CreatedAt = "d-a-new", t0.Add(time.Minute)
	all := []Decision{replacement, old, otherNew, otherOld} // order on the wire does not matter
	if q := Queue(all); len(q) != 1 || q[0].ID != "d-new" {
		t.Fatalf("Queue = %+v", q)
	}
	if q := AgentQueue(all); len(q) != 1 || q[0].ID != "d-a-new" {
		t.Fatalf("AgentQueue = %+v", q)
	}
	if c := Count(nil, nil, all); c.AwaitingHuman != 1 {
		t.Fatalf("AwaitingHuman = %d", c.AwaitingHuman)
	}
}

func TestStaleStatusText(t *testing.T) {
	cases := []struct {
		in   Intent
		want string
	}{
		{Intent{Status: IntentOpen}, "open"},
		{Intent{Status: IntentOpen, Stale: true}, "stale"},
		{Intent{Status: IntentProposed, Stale: true}, "stale"},
		{Intent{Status: IntentAbandoned, Stale: true}, "abandoned"},
	}
	for _, c := range cases {
		if got := c.in.StatusText(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.in, got, c.want)
		}
	}
}

// A decision superseded by a restack waits for nobody and says so.
func TestSupersededDecision(t *testing.T) {
	d := Decision{Action: policy.HumanApprove, Status: StatusSuperseded}
	if d.AwaitsHuman() {
		t.Fatal("a superseded decision still awaits a person")
	}
	if got := statusLabel(d); got != "superseded (propose again)" {
		t.Fatalf("label = %q", got)
	}
}
