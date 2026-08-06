package executil

import (
	"context"
	"strings"
	"testing"
)

// Run captures stdout and stderr into one combined buffer, and os/exec copies
// each stream on its own goroutine — so a child writing to both concurrently
// used to race on that shared buffer (caught by -race the first time a test
// exercised a chatty-on-both-streams command, git push). The writes must be
// synchronized; this test is only meaningful under -race.
func TestRunCapturesConcurrentStdoutAndStderrWithoutRacing(t *testing.T) {
	r := Run(context.Background(), t.TempDir(), "sh", "-c",
		"for i in $(seq 1 200); do echo out$i; echo err$i >&2; done")
	if !r.Ok() {
		t.Fatalf("Run: %v", r.Err)
	}
	if !strings.Contains(r.Output, "out200") || !strings.Contains(r.Output, "err200") {
		t.Errorf("combined output missing a stream's tail: %q", r.Output[max(0, len(r.Output)-80):])
	}
}
