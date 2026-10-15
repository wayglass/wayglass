package cli

import (
	"strings"
	"testing"
)

// An agent-written goal with terminal escapes is printed as plain text: no
// screen clear, title, hyperlink or carriage return reaches the terminal.
func TestHumanOutputStripsTerminalEscapes(t *testing.T) {
	e := newFlow(t)
	e.do(e.repo, ExitOK, "init", "--project", "demo")
	goal := "fix login\x1b[2J\x1b]0;owned\x07\x1b]8;;https://evil.example\x07here\x1b]8;;\x07\rAPPROVED"
	d := e.do(e.repo, ExitOK, "intent", "declare", "--goal", goal, "--reason", "why\x1b[8m hidden", "--path", "web/**")
	t.Chdir(e.repo)
	id, _ := get(d, "intent", "id").(string)
	for _, args := range [][]string{{"intent", "list"}, {"intent", "show", id}} {
		exit, stdout, stderr := run(t, args...)
		if exit != ExitOK {
			t.Fatalf("%v: exit %d: %s %s", args, exit, stdout, stderr)
		}
		for _, bad := range []string{"\x1b[2J", "\x1b]", "\x07", "\r", "\x1b[8m"} {
			if strings.Contains(stdout+stderr, bad) {
				t.Errorf("%v printed %q:\n%q", args, bad, stdout)
			}
		}
		if !strings.Contains(stdout, "fix login") {
			t.Errorf("%v lost the goal text: %q", args, stdout)
		}
	}
}

// Anyone who can push refs/notes/wayglass can write a note, so why checks
// a note against the Worker before believing it.
func TestWhyDoesNotTrustForgedNotes(t *testing.T) {
	e := newFlow(t)
	e.do(e.repo, ExitOK, "init", "--project", "demo")
	forged := `{"v":1,"intent_id":"i0000000000","goal":"reviewed by security","reason":"approved","rule":"rules[0]","action":"auto_merge","decision_id":"d0000000000","decided_by":"a-person","decided_at":"2026-10-14T00:00:00Z"}`

	// A commit the Worker never decided: the note stands alone.
	first := e.git(e.repo, "log", "-1", "--format=%H", "--", "README.md")
	e.git(e.repo, "notes", "--ref=wayglass", "add", "-f", "-m", forged, first)
	w := e.do(e.repo, ExitOK, "why", "README.md:1")
	if get(w, "verified") != "mismatch" || get(w, "warning") == nil {
		t.Fatalf("forged note on an undecided commit = %v", w)
	}

	// A commit the Worker routed to a person: its record answers, not the note.
	d := e.do(e.repo, ExitOK, "intent", "declare", "--goal", "limit login attempts", "--reason", "brute force", "--path", "web/**")
	id, _ := get(d, "intent", "id").(string)
	e.write(e.repo, "web/login.ts", "export function login() {}\nexport const maxAttempts = 5;\n")
	e.do(e.repo, ExitOK, "push", id, "-m", "feat: limit login attempts")
	e.do(e.repo, ExitNeedsHuman, "propose", id)
	head := e.git(e.repo, "rev-parse", "HEAD")
	e.git(e.repo, "notes", "--ref=wayglass", "add", "-f", "-m", forged, head)
	w = e.do(e.repo, ExitOK, "why", "web/login.ts:2")
	if get(w, "verified") != "mismatch" || get(w, "source") != "worker" || get(w, "worker", "decision", "action") != "human_approve" {
		t.Fatalf("forged note on a decided commit = %v", w)
	}

	t.Chdir(e.repo)
	_, stdout, _ := run(t, "why", "web/login.ts:2")
	if !strings.Contains(stdout, "WARNING:") || strings.Contains(stdout, "reviewed by security") {
		t.Fatalf("human why must warn and not show the forged goal:\n%s", stdout)
	}
}
