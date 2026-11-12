package cursor

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	"github.com/tctien342/pig-cursor-sdk/gen/sdk/v1/sdkv1connect"
)

type cancellationServer struct {
	sdkv1connect.UnimplementedSdkAgentServiceHandler
	calls     atomic.Int32
	failFirst bool
	block     bool
}

func (s *cancellationServer) CancelRun(ctx context.Context, req *connect.Request[sdkpb.CancelRunRequest]) (*connect.Response[sdkpb.CancelRunResponse], error) {
	n := s.calls.Add(1)
	if req.Msg.GetRunId() != "owned-run" || req.Msg.GetAgentId() != "owned-agent" {
		return nil, errors.New("wrong cancellation target")
	}
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.failFirst && n == 1 {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("temporary cancellation failure"))
	}
	return connect.NewResponse(&sdkpb.CancelRunResponse{}), nil
}
func cancelTestRun(t *testing.T, s *cancellationServer) *Run {
	t.Helper()
	_, handler := sdkv1connect.NewSdkAgentServiceHandler(s)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	a := &Agent{id: "owned-agent", client: &Client{Agent: sdkv1connect.NewSdkAgentServiceClient(server.Client(), server.URL)}}
	r := newRun(a, context.Background(), func() {})
	r.runID = "owned-run"
	a.run = r
	return r
}
func TestCancellationFailureCanRetryAndThenIsIdempotent(t *testing.T) {
	s := &cancellationServer{failFirst: true}
	r := cancelTestRun(t, s)
	if err := r.CancelRun("first"); err == nil {
		t.Fatal("failed RPC was acknowledged as success")
	}
	status := r.Status()
	if !status.Finished || !status.CancelFailed || status.CancelConfirmed {
		t.Fatalf("incorrect cancellation state: %+v", status)
	}
	if err := r.CancelRun("retry"); err != nil {
		t.Fatal(err)
	}
	if err := r.CancelRun("again"); err != nil || s.calls.Load() != 2 {
		t.Fatal("confirmed cancellation was resent")
	}
	if !r.Status().CancelConfirmed {
		t.Fatal("successful cancellation not recorded")
	}
}
func TestCancellationContextBoundsRPCAndClosesLocalQueue(t *testing.T) {
	s := &cancellationServer{block: true}
	r := cancelTestRun(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.CancelRunContext(ctx, "stop")
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("unbounded or falsely confirmed cancellation: %v", err)
	}
	if !r.Finished() || r.Status().CancelConfirmed {
		t.Fatal("local/remote stop was conflated")
	}
	if _, ok := r.Next(context.Background()); ok {
		t.Fatal("cancelled queue remained active")
	}
}
func TestQueueCancellationWinsOverQueuedWork(t *testing.T) {
	r := newTestRun()
	defer r.abandon(context.Canceled)
	r.queue.push(RunEvent{Event: Event{Kind: EventToolCallStarted}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := r.Next(ctx); ok {
		t.Fatal("queued tool emitted after caller cancellation")
	}
}
func TestLateBatchedCallbackSurvivesClosedPauseQueue(t *testing.T) {
	r := newTestRun()
	defer r.abandon(context.Canceled)
	r.PauseStream()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := r.park(ctx, ToolCallRequest{ToolCallID: "late\ncall", ToolName: "bash"})
		result <- err
	}()
	for r.queueRef().len() == 0 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	event, ok := r.Next(ctx)
	if !ok || event.ToolCall == nil || event.ToolCall.ID != "late\ncall" {
		t.Fatal("late native callback was dropped")
	}
	r.ResolveHostCall("late\ncall", map[string]any{"content": "ok"}, false, "")
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
func TestCancelledWaitCannotLoseWakeup(t *testing.T) {
	for i := 0; i < 100; i++ {
		q := newEventQueue(2)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { q.pop(ctx, context.Background()); close(done) }()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("lost cancellation wakeup")
		}
	}
}
