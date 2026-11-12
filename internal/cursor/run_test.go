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

// A batch can arrive after the provider turn already parked on the first call:
// the grace window may expire, the stream gets paused, and Cursor sends the rest
// of the turn afterwards. Both calls must still reach the host, each be answered
// exactly once, and nothing may hang.
func TestLateCallbackAfterFirstParkStillResolves(t *testing.T) {
	r := newTestRun()
	defer r.c()

	type outcome struct {
		m   map[string]any
		err error
	}
	got := make(chan outcome, 2)
	park := func(id string) {
		go func() {
			m, err := r.park(context.Background(), req(id, "bash"))
			got <- outcome{m, err}
		}()
	}

	park("call_1")
	ev, ok := r.Next(context.Background())
	if !ok || ev.Pending == nil {
		t.Fatalf("first event = %+v ok=%v, want a pending host call", ev, ok)
	}
	r.PauseStream() // the turn handed call_1 back and ended

	park("call_2") // the late tail of the same batch
	deadline := time.Now().Add(2 * time.Second)
	for len(r.PendingHostCalls()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	pending := r.PendingHostCalls()
	if len(pending) != 2 {
		t.Fatalf("pending = %d, want both calls of the batch", len(pending))
	}
	if pending[0].ID != "call_1" || pending[1].ID != "call_2" {
		t.Fatalf("pending order = %s, %s, want arrival order", pending[0].ID, pending[1].ID)
	}

	for _, p := range pending {
		if !r.ResolveHostCall(p.ID, map[string]any{"content": "out " + p.ID}, false, "out "+p.ID) {
			t.Fatalf("resolve %s was refused", p.ID)
		}
		if r.ResolveHostCall(p.ID, map[string]any{"content": "again"}, false, "again") {
			t.Fatalf("a second resolve of %s was accepted", p.ID)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case o := <-got:
			if o.err != nil {
				t.Fatalf("parked callback returned an error: %v", o.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a parked callback never returned; the run hung")
		}
	}
	if left := r.PendingHostCalls(); len(left) != 0 {
		t.Fatalf("%d host call(s) still pending after all results were delivered", len(left))
	}
}

// The host must see a one-token id while the ledger keeps Cursor's own.
func TestParkedCallCarriesHostSafeID(t *testing.T) {
	r := newTestRun()
	defer r.c()

	raw := "call-9\nfc_9_0"
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.park(context.Background(), req(raw, "bash"))
	}()
	ev, ok := r.Next(context.Background())
	if !ok || ev.Pending == nil {
		t.Fatalf("first event = %+v ok=%v, want a pending host call", ev, ok)
	}
	if ev.Pending.ID != raw {
		t.Fatalf("ledger id = %q, want Cursor's raw id %q", ev.Pending.ID, raw)
	}
	if ev.Pending.HostID() != "call-9%0Afc_9_0" {
		t.Fatalf("host id = %q, want call-9%%0Afc_9_0", ev.Pending.HostID())
	}
	if ev.ToolCall.ID != raw || ev.ToolCall.DisplayID != ev.Pending.HostID() {
		t.Fatalf("event ids = %q / %q, want raw and host-safe forms", ev.ToolCall.ID, ev.ToolCall.DisplayID)
	}
	if !r.ResolveHostCall(raw, map[string]any{"content": "ok"}, false, "ok") {
		t.Fatal("resolve by the raw id was refused")
	}
	<-done
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
		"SDK_ERROR_CODE_USAGE_LIMIT_EXCEEDED": CodeUsageLimit, // a plan window, not a burst
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

// Cursor only describes a usage window when it refuses a call, so the rendering
// has to work with whatever subset it sends and invent nothing.
func TestRateLimitSummaryShowsOnlyWhatCursorReported(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name string
		rate *RateLimit
		want string
	}{
		{"nil", nil, ""},
		{"empty", &RateLimit{}, ""},
		{"limit and remaining", &RateLimit{Limit: 500, HasLimit: true, Remaining: 12, HasRemaining: true}, "12 of 500 left"},
		{"remaining only", &RateLimit{Remaining: 3, HasRemaining: true}, "3 left"},
		{"limit only", &RateLimit{Limit: 500, HasLimit: true}, "limit 500"},
		{
			"with reset",
			&RateLimit{Remaining: 0, HasRemaining: true, ResetAt: now.Add(2*time.Hour + 5*time.Minute), HasReset: true},
			"0 left · resets in 2h05m (" + now.Add(125*time.Minute).Local().Format("15:04") + ")",
		},
		{"reset already past", &RateLimit{HasReset: true, ResetAt: now.Add(-time.Hour)}, "resets now (" + now.Add(-time.Hour).Local().Format("15:04") + ")"},
	}
	for _, c := range cases {
		if got := c.rate.Summary(now); got != c.want {
			t.Errorf("%s: Summary = %q, want %q", c.name, got, c.want)
		}
	}
}

// A new turn must not die because the previous one left this agent holding a run.
// That is what happened in a real session: the user sent a message while Cursor was
// still running its own shell tool, and the turn failed instantly with
// "run … is still active for this agent" — no answer, no card, nothing to retry.
func TestSupersedeReleasesALeftoverRun(t *testing.T) {
	a := &Agent{toolSet: map[string]bool{}}
	left := newRun(a, context.Background(), func() {})
	a.run = left

	if err := a.supersedeActiveRun("superseded by a new turn"); err != nil {
		t.Fatalf("a leftover run must not refuse the new turn: %v", err)
	}
	if !left.Finished() {
		t.Fatal("the leftover run should have been released")
	}

	// Nothing to do when the previous run is already over.
	done := newRun(a, context.Background(), func() {})
	done.abandon(errors.New("finished"))
	a.run = done
	if err := a.supersedeActiveRun("again"); err != nil {
		t.Fatalf("a finished run should be left alone: %v", err)
	}

	// A closed agent is a real refusal, and must stay one.
	a.closed = true
	if err := a.supersedeActiveRun("closed"); err == nil {
		t.Fatal("a closed agent must refuse to send")
	}
}
