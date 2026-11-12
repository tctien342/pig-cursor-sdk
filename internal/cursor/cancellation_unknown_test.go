package cursor

import (
	"context"
	"strings"
	"testing"
)

func TestCancellationWithoutRunIdentityIsExplicitlyUnconfirmed(t *testing.T) {
	run := newTestRun()
	run.mu.Lock()
	run.runID = ""
	run.mu.Unlock()
	err := run.CancelRunContext(context.Background(), "startup interrupted")
	status := run.Status()
	if err == nil || !strings.Contains(err.Error(), "without a run ID") || !status.Finished || !status.CancelRequested || !status.CancelFailed || status.CancelConfirmed {
		t.Fatalf("unknown upstream identity falsely acknowledged: status=%+v err=%v", status, err)
	}
}
