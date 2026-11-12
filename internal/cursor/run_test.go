package cursor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newTestRun() *Run {
	ctx, cancel := context.WithCancel(context.Background())
	r := newRun(&Agent{toolSet: map[string]bool{"bash": true}}, ctx, cancel)
	return r
}

func req(id, tool string) ToolCallRequest {
	return ToolCallRequest{
		ToolName:   tool,
		Args:       map[string]any{"command": "echo " + id},
		ToolCallID: id,
		AgentID:    "agent-1",
		ArrivedAt:  time.Now(),
	}
}

// TestParkedCallSurvivesProviderBoundary is the property the whole design
// rests on: the bridge's callback stays open until the host resolves it, and
// the run keeps streaming in the meantime.
func TestParkedCallSurvivesProviderBoundary(t *testing.T) {
	r := newTestRun()
	defer r.c()

	type outcome struct {
		m   map[string]any
		err error
	}
	got := make(chan outcome, 1)
	go func() {
		m, err := r.park(context.Background(), req("call_1", "bash"))
		got <- outcome{m, err}
	}()

	ev, ok := r.Next(context.Background())
	if !ok || ev.Kind != EventToolCallStarted || ev.Pending == nil {
		t.Fatalf("first event = %+v ok=%v, want a pending host tool call", ev, ok)
	}
	if ev.ToolCall.ToolName != "bash" {
		t.Errorf("tool = %q", ev.ToolCall.ToolName)
	}
	if pending := r.PendingHostCalls(); len(pending) != 1 || pending[0].ID != "call_1" {
		t.Fatalf("pending = %+v", pending)
	}

	// The provider turn ends here (pig runs the tool). The parked call must not
	// vanish, and the run must not be considered finished.
	time.Sleep(50 * time.Millisecond)
	if r.Finished() {
		t.Fatal("run reported finished while a host call was parked")
	}
	if got := r.PendingHostCalls(); len(got) != 1 {
		t.Fatalf("parked call disappeared across the boundary: %d pending", len(got))
	}

	if !r.ResolveHostCall("call_1", map[string]any{"content": "hello from host"}, false, "") {
		t.Fatal("resolve reported failure")
	}
	select {
	case o := <-got:
		if o.err != nil {
			t.Fatalf("park returned error: %v", o.err)
		}
		if o.m["content"] != "hello from host" {
			t.Errorf("resolved payload = %v", o.m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked callback never resolved")
	}
	if left := r.PendingHostCalls(); len(left) != 0 {
		t.Errorf("pending after resolve = %d", len(left))
	}
}

// TestExactlyOnceResolution proves a tool result cannot be delivered twice, and
// a second resolve attempt is reported rather than silently accepted.
func TestExactlyOnceResolution(t *testing.T) {
	r := newTestRun()
	defer r.c()

	done := make(chan error, 1)
	go func() {
		_, err := r.park(context.Background(), req("call_once", "bash"))
		done <- err
	}()
	if _, ok := r.Next(context.Background()); !ok {
		t.Fatal("no event")
	}
	if !r.ResolveHostCall("call_once", map[string]any{"content": "first"}, false, "") {
		t.Fatal("first resolve should succeed")
	}
	if err := <-done; err != nil {
		t.Fatalf("park error: %v", err)
	}
	if r.ResolveHostCall("call_once", map[string]any{"content": "second"}, false, "") {
		t.Fatal("second resolve must fail: the tool already returned")
	}
}

// TestDuplicateCallbackDoesNotQueueTwice covers a bridge retry of the same call.
func TestDuplicateCallbackDoesNotQueueTwice(t *testing.T) {
	r := newTestRun()
	defer r.c()

	first := make(chan error, 1)
	go func() {
		_, err := r.park(context.Background(), req("dup", "bash"))
		first <- err
	}()
	if _, ok := r.Next(context.Background()); !ok {
		t.Fatal("no event")
	}
	if _, err := r.park(context.Background(), req("dup", "bash")); err == nil {
		t.Fatal("duplicate callback must not start a second execution")
	} else if !errors.Is(err, err) || err.Error() == "" {
		t.Errorf("duplicate callback error = %v", err)
	}
	if len(r.PendingHostCalls()) != 1 {
		t.Fatalf("pending = %d, want the single original call", len(r.PendingHostCalls()))
	}
	r.ResolveHostCall("dup", map[string]any{"content": "ok"}, false, "")
	if err := <-first; err != nil {
		t.Errorf("park error: %v", err)
	}
}

// TestConcurrentCallbacksResolvesIndependently checks a batch of tool calls,
// which is what a model asking for three commands in one turn produces.
func TestConcurrentCallbacksResolvesIndependently(t *testing.T) {
	r := newTestRun()
	defer r.c()

	ids := []string{"a", "b", "c"}
	var wg sync.WaitGroup
	results := make(map[string]string)
	var mu sync.Mutex
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			m, err := r.park(context.Background(), req(id, "bash"))
			if err != nil {
				mu.Lock()
				results[id] = "err:" + err.Error()
				mu.Unlock()
				return
			}
			mu.Lock()
			results[id], _ = m["content"].(string)
			mu.Unlock()
		}(id)
	}
	// All three must be queued as pending before any is resolved.
	deadline := time.Now().Add(3 * time.Second)
	for len(r.PendingHostCalls()) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(r.PendingHostCalls()); n != 3 {
		t.Fatalf("pending = %d, want 3", n)
	}
	// Resolve out of order: the last call first.
	for _, id := range []string{"c", "a", "b"} {
		if !r.ResolveHostCall("call_"+id[0:1], nil, false, "result-"+id) {
			// ids are the tool call ids themselves
			if !r.ResolveHostCall(id, nil, false, "result-"+id) {
				t.Fatalf("resolve %s failed", id)
			}
		}
	}
	wg.Wait()
	for _, id := range ids {
		if results[id] != "result-"+id {
			t.Errorf("result[%s] = %q", id, results[id])
		}
	}
}

// TestAbandonReleasesParkedCalls is the abort path: the user stops the turn, so
// parked callbacks must fail fast instead of hanging the bridge.
func TestAbandonReleasesParkedCalls(t *testing.T) {
	r := newTestRun()
	defer r.c()

	parked := make(chan error, 1)
	go func() {
		_, err := r.park(context.Background(), req("abort_me", "bash"))
		parked <- err
	}()
	if _, ok := r.Next(context.Background()); !ok {
		t.Fatal("no event")
	}
	r.abandon(errors.New("user aborted"))
	select {
	case err := <-parked:
		if err == nil {
			t.Fatal("abandoned park returned no error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abandon did not release the parked call")
	}
	if len(r.PendingHostCalls()) != 0 {
		t.Error("pending calls survived abandon")
	}
}

// TestParkHonoursRunCancellation covers a cancel that arrives mid-park.
func TestParkHonoursRunCancellation(t *testing.T) {
	r := newTestRun()
	defer r.c()
	errCh := make(chan error, 1)
	go func() {
		_, err := r.park(context.Background(), req("cancel_me", "bash"))
		errCh <- err
	}()
	if _, ok := r.Next(context.Background()); !ok {
		t.Fatal("no event")
	}
	r.c() // run-owned cancel
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("cancelled run should release the parked call with an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("park ignored the run cancellation")
	}
}

// TestQueueDoesNotBlockDrain proves a slow or ended provider cannot stall the
// Cursor run: events accumulate instead of blocking the stream reader.
func TestQueueDoesNotBlockDrain(t *testing.T) {
	q := newEventQueue(100)
	for i := 0; i < 50; i++ {
		if !q.push(RunEvent{Event: Event{Kind: EventTextDelta, Text: "x"}}) {
			t.Fatalf("push %d refused", i)
		}
	}
	if q.len() != 50 {
		t.Fatalf("queued %d, want 50", q.len())
	}
	var order []string
	for {
		ev, ok := q.popReady()
		if !ok {
			break
		}
		order = append(order, string(ev.Kind))
	}
	if len(order) != 50 || order[0] != string(EventTextDelta) {
		t.Fatalf("order wrong: %d items", len(order))
	}
}

// TestQueueOverflowIsReported covers the runaway case: a run producing events
// nobody consumes must surface an error rather than grow forever.
func TestQueueOverflowIsReported(t *testing.T) {
	q := newEventQueue(4)
	for i := 0; i < 4; i++ {
		q.push(RunEvent{Event: Event{Kind: EventTextDelta, Text: "x"}})
	}
	if q.push(RunEvent{Event: Event{Kind: EventTextDelta, Text: "x"}}) {
		t.Fatal("overflow should be refused")
	}
}

// TestPopEndsWhenQueueCloses checks the consumer's exit path.
func TestPopEndsWhenQueueCloses(t *testing.T) {
	q := newEventQueue(10)
	go func() { time.Sleep(20 * time.Millisecond); q.close() }()
	if _, ok := q.pop(context.Background(), context.Background()); ok {
		t.Fatal("pop should end after close")
	}
}

func TestToolResultIsAlwaysAnObject(t *testing.T) {
	st, err := structFromToolResult("plain text result")
	if err != nil {
		t.Fatal(err)
	}
	if got := st.AsMap()["content"]; got != "plain text result" {
		t.Fatalf("tool result payload = %v", st.AsMap())
	}
}

func TestErrorClassification(t *testing.T) {
	cases := map[string]ErrorCode{
		"SDK_ERROR_CODE_UNAUTHORIZED":         CodeAuth,
		"SDK_ERROR_CODE_API_KEY_NOT_FOUND":    CodeAuth,
		"SDK_ERROR_CODE_USAGE_LIMIT_EXCEEDED": CodeRateLimit,
		"SDK_ERROR_CODE_AGENT_BUSY":           CodeBusy,
		"SDK_ERROR_CODE_INVALID_MODEL":        CodeValidation,
		"SDK_ERROR_CODE_UPSTREAM_ERROR":       CodeUpstream,
		"SDK_ERROR_CODE_CLIENT_CANCELLED":     CodeCancelled,
	}
	for sdk, want := range cases {
		if got := classify(sdk, 0, ""); got != want {
			t.Errorf("classify(%s) = %s, want %s", sdk, got, want)
		}
	}
	if got := classify("", 0, "context window exceeded"); got != CodeContextOverflow {
		t.Errorf("overflow message classified as %s", got)
	}
	if !Retryable(&Error{Code: CodeRateLimit}) {
		t.Error("rate limit should be retryable")
	}
	if Retryable(&Error{Code: CodeAuth}) {
		t.Error("auth failure should not be retried")
	}
}
