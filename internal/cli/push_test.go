package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// A change the Worker would refuse to record (more files than one push
// report may carry) must not reach the fork: nothing is pushed, and the
// error says so.
func TestPushRefusesTooManyFilesBeforePushing(t *testing.T) {
	e := newFlow(t)
	e.do(e.repo, ExitOK, "init", "--project", "demo")
	d := e.do(e.repo, ExitOK, "intent", "declare", "--goal", "bulk", "--reason", "generated", "--path", "gen/**")
	remote := get(d, "fork", "remote").(string)
	before := e.worker.head(remote, "main")
	if before == "" {
		t.Fatal("fork has no head after init")
	}
	for i := range maxReportedPaths + 1 {
		e.write(e.repo, filepath.Join("gen", fmt.Sprintf("f%03d.txt", i)), "x\n")
	}
	got := e.do(e.repo, ExitError, "push", "-m", "gen: too many files")
	msg := get(got, "error", "message").(string)
	if !strings.Contains(msg, "nothing was pushed") || get(got, "error", "code") != CodeError {
		t.Fatalf("push = %v", got)
	}
	if after := e.worker.head(remote, "main"); after != before {
		t.Fatalf("fork moved from %s to %s although the push was refused", before, after)
	}
}
