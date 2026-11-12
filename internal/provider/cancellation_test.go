package provider

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	"github.com/tctien342/pig-cursor-sdk/gen/sdk/v1/sdkv1connect"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
	"google.golang.org/protobuf/types/known/structpb"
)

type scopedCancelServer struct {
	sdkv1connect.UnimplementedSdkAgentServiceHandler
	creates        atomic.Int32
	sends          atomic.Int32
	cancelled      chan string
	failDrain      bool
	doneOnly       bool
	terminalResult bool
	cancelEntered  chan struct{}
	cancelRelease  <-chan struct{}
}

func (s *scopedCancelServer) CreateAgent(context.Context, *connect.Request[sdkpb.CreateAgentRequest]) (*connect.Response[sdkpb.CreateAgentResponse], error) {
	return connect.NewResponse(&sdkpb.CreateAgentResponse{AgentId: fmt.Sprintf("agent-%d", s.creates.Add(1))}), nil
}
func (s *scopedCancelServer) Send(ctx context.Context, req *connect.Request[sdkpb.SendRequest], stream *connect.ServerStream[sdkpb.RunStreamMessage]) error {
	s.sends.Add(1)
	data, _ := structpb.NewStruct(map[string]any{"status": "running", "run_id": req.Msg.GetAgentId() + "-run", "agent_id": req.Msg.GetAgentId()})
	if err := stream.Send(&sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_SdkMessage{SdkMessage: &sdkpb.SdkMessage{Type: "status", Message: data}}}); err != nil {
		return err
	}
	if s.terminalResult {
		return stream.Send(&sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_Result{Result: &sdkpb.RunStreamResult{Result: &sdkpb.RunResult{RunId: req.Msg.GetAgentId() + "-run", Status: sdkpb.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED}}}})
	}
	if s.doneOnly {
		return stream.Send(&sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_Done{Done: &sdkpb.RunStreamDone{}}})
	}
	if s.failDrain {
		return connect.NewError(connect.CodeCanceled, context.Canceled)
	}
	<-ctx.Done()
	return ctx.Err()
}
func (s *scopedCancelServer) CancelRun(ctx context.Context, req *connect.Request[sdkpb.CancelRunRequest]) (*connect.Response[sdkpb.CancelRunResponse], error) {
	s.cancelled <- req.Msg.GetRunId()
	if s.cancelEntered != nil {
		close(s.cancelEntered)
	}
	if s.cancelRelease != nil {
		select {
		case <-s.cancelRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return connect.NewResponse(&sdkpb.CancelRunResponse{}), nil
}
func (s *scopedCancelServer) CloseAgent(context.Context, *connect.Request[sdkpb.CloseAgentRequest]) (*connect.Response[sdkpb.CloseAgentResponse], error) {
	return connect.NewResponse(&sdkpb.CloseAgentResponse{}), nil
}

func TestAbortScopeIsolatesSameCwdSessions(t *testing.T) {
	service := &scopedCancelServer{cancelled: make(chan string, 10)}
	_, handler := sdkv1connect.NewSdkAgentServiceHandler(service)
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &cursor.Client{Agent: sdkv1connect.NewSdkAgentServiceClient(server.Client(), server.URL)}
	e := NewEngine(Config{ConfigHome: t.TempDir()})
	defer e.Stop()
	scopeA, _ := scopeKey("/sessions/a.jsonl", "/same/repo", "", e.nonce)
	scopeB, _ := scopeKey("/sessions/b.jsonl", "/same/repo", "", e.nonce)
	add := func(key string) *cursor.Run {
		agent, err := client.CreateAgent(context.Background(), cursor.AgentSpec{Model: "test"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := agent.Send(context.Background(), "wait", nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, ok := run.Next(ctx); !ok {
			t.Fatal("no run identity")
		}
		session := &Session{Key: key}
		session.setAgent(agent)
		e.sessions[key] = session
		return run
	}
	a := add(scopeA + "model-a")
	b := add(scopeB + "model-a")
	report := e.abortScope(scopeA, true)
	if report.Stopped != 1 || report.Unconfirmed != 0 || !a.Status().CancelConfirmed || b.Finished() {
		t.Fatalf("abort crossed session boundary: %+v", report)
	}
	if id := <-service.cancelled; id != a.RunID() {
		t.Fatalf("cancelled wrong run: %s", id)
	}
	if report := e.abortScope(scopeB, false); report.Stopped != 0 || b.Finished() {
		t.Fatal("unknown scope cancelled another session")
	}
	first, _ := scopeKey("", "/same/repo", "session-a", e.nonce)
	second, _ := scopeKey("", "/same/repo", "session-b", e.nonce)
	if first == second {
		t.Fatal("ephemeral sessions share an abort scope")
	}
}

func TestAbortGenerationRejectsQueuedAndReplacementWork(t *testing.T) {
	s := &Session{}
	before := s.generation()
	ctx, end, ok := s.beginTurn(context.Background(), before)
	if !ok {
		t.Fatal("initial turn rejected")
	}
	s.abortTarget()
	if ctx.Err() != context.Canceled {
		t.Fatal("active pump was not cancelled")
	}
	if s.publishAgent(&cursor.Agent{}) {
		t.Fatal("agent replacement survived an intervening abort")
	}
	end()
	if _, end, ok := s.beginTurn(context.Background(), before); ok {
		end()
		t.Fatal("already-queued work restarted after abort")
	}
	_, end, ok = s.beginTurn(context.Background(), s.generation())
	if !ok {
		t.Fatal("later explicit user turn was blocked")
	}
	end()
}
