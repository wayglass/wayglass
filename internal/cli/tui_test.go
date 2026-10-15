package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wayglass/wayglass/internal/intent"
	"github.com/wayglass/wayglass/internal/tui"
)

func TestWatchDemoJSONSnapshot(t *testing.T) {
	exit, stdout, stderr := run(t, "watch", "--demo-data", "--json")
	if exit != ExitOK || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", exit, stderr)
	}
	checkEnvelope(t, stdout)
	var env struct {
		Data tui.Snapshot `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatal(err)
	}
	s := env.Data
	if !s.Sample {
		t.Fatal("demo data must say sample:true")
	}
	// The sample carries the AI reviewer: one approval, one escalation (a fourth
	// change for a person) and advice on the auth change.
	if s.Counters.Intents != 8 || s.Counters.AwaitingHuman != 4 || s.Counters.TimeSavedBasis == "" ||
		s.Counters.AIApprovals != 1 || s.Counters.AIEscalations != 1 || s.Counters.AIAdvised != 1 {
		t.Fatalf("counters = %+v", s.Counters)
	}
	if len(s.Overlaps) != 1 || s.Overlaps[0].Replan.Action != intent.ReplanLandFirst || s.Overlaps[0].Replan.First != "i3f2a91c0d1" {
		t.Fatalf("overlaps = %+v", s.Overlaps)
	}
}

func TestReviewDemoJSONQueue(t *testing.T) {
	exit, stdout, _ := run(t, "review", "--demo-data", "--json")
	if exit != ExitOK {
		t.Fatalf("exit=%d", exit)
	}
	checkEnvelope(t, stdout)
	var env struct {
		Data ReviewData `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatal(err)
	}
	if !env.Data.Sample || len(env.Data.Queue) != 4 {
		t.Fatalf("data = %+v", env.Data)
	}
	for _, it := range env.Data.Queue {
		if it.Intent.Goal == "" || it.Decision.MatchedRule == "" {
			t.Fatalf("queue item lacks intent or rule: %+v", it)
		}
	}

	_, stdout, _ = run(t, "review", "i3f2a91c0d1", "--demo-data", "--json")
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Queue) != 1 || env.Data.Queue[0].Intent.ID != "i3f2a91c0d1" {
		t.Fatalf("filtered queue = %+v", env.Data.Queue)
	}
	if a := env.Data.Queue[0].Decision.AIAdvice; a == nil || a.Summary == "" || a.Model == "" {
		t.Fatalf("the auth change should carry the sample AI advice: %+v", env.Data.Queue[0].Decision)
	}
	if !slices.Contains(env.Data.Untrusted, "queue[].decision.ai_advice.summary") {
		t.Fatalf("model-written advice must be listed as untrusted: %v", env.Data.Untrusted)
	}
}

func TestWatchFollowNeedsJSON(t *testing.T) {
	exit, _, stderr := run(t, "watch", "--demo-data", "--follow")
	if exit != ExitUsage || !strings.Contains(stderr, "--json") {
		t.Fatalf("exit=%d stderr=%q", exit, stderr)
	}
}

func TestWatchFollowStreamsNDJSON(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	exit := Execute(ctx, BuildInfo{Version: "test"}, []string{"watch", "--demo-data", "--json", "--follow", "--interval", "100ms"}, &stdout, &stderr)
	if exit != ExitOK || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("want several NDJSON envelopes as the script plays, got %d", len(lines))
	}
	var last int64
	for _, l := range lines {
		checkEnvelope(t, l)
		var env struct {
			Data tui.Snapshot `json:"data"`
		}
		if err := json.Unmarshal([]byte(l), &env); err != nil {
			t.Fatal(err)
		}
		for _, e := range env.Data.Events {
			if e.Seq <= last {
				t.Fatalf("event %d repeated or out of order", e.Seq)
			}
			last = e.Seq
		}
	}
}

func TestWatchAndReviewTextWhenPiped(t *testing.T) {
	for _, args := range [][]string{{"watch", "--demo-data"}, {"review", "--demo-data"}} {
		exit, stdout, _ := run(t, args...)
		if exit != ExitOK || !strings.Contains(stdout, "SAMPLE DATA") {
			t.Fatalf("%v: exit=%d stdout=%q", args, exit, stdout)
		}
		if strings.Contains(stdout, "\x1b[") {
			t.Fatalf("%v: piped output must be plain", args)
		}
	}
}
