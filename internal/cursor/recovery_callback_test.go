package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	"github.com/tctien342/pig-cursor-sdk/gen/sdk/v1/sdkv1connect"
	"google.golang.org/protobuf/types/known/structpb"
)

type callbackReplayServer struct {
	sdkv1connect.UnimplementedSdkAgentServiceHandler
	callback func() error
	observes atomic.Int32
}

func hostReplayFrame(status string) *sdkpb.RunStreamMessage {
	payload, _ := structpb.NewStruct(map[string]any{"call_id": "host-once", "name": "bash", "status": status, "args": map[string]any{"command": "echo once"}})
	return &sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_SdkMessage{SdkMessage: &sdkpb.SdkMessage{Type: "tool_call", Message: payload}}}
}
func (s *callbackReplayServer) ObserveRun(_ context.Context, _ *connect.Request[sdkpb.ObserveRunRequest], stream *connect.ServerStream[sdkpb.RunStreamMessage]) error {
	n := s.observes.Add(1)
	if err := s.callback(); err != nil {
		return err
	}
	for _, status := range []string{"running", "completed"} {
		if err := stream.Send(hostReplayFrame(status)); err != nil {
			return err
		}
	}
	value := "continued"
	if n > 1 {
		value = "continued safely"
	}
	if err := stream.Send(recoveryText(value, "")); err != nil {
		return err
	}
	if n == 1 {
		return interruptedFrame()
	}
	if err := stream.Send(&sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_Result{Result: &sdkpb.RunStreamResult{Result: &sdkpb.RunResult{RunId: "recover-run", Result: "continued safely", Status: sdkpb.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED}}}}); err != nil {
		return err
	}
	return stream.Send(recoveryText("must not display late output", ""))
}

func TestHostResultReplayAcrossStreamInterruptionExecutesOnce(t *testing.T) {
	run := newTestRun()
	defer run.abandon(context.Canceled)
	run.runID = "recover-run"
	request := ToolCallRequest{ToolCallID: "host-once", ToolName: "bash", Args: map[string]any{"command": "echo once"}}
	initial := make(chan map[string]any, 1)
	go func() { value, _ := run.park(context.Background(), request); initial <- value }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, ok := run.Next(ctx); !ok {
		t.Fatal("no original native call")
	}
	run.markEmitted(request.ToolCallID)
	if !run.ResolveHostCall(request.ToolCallID, map[string]any{"content": "side-effect-result"}, false, "side-effect-result") {
		t.Fatal("original host execution was not settled")
	}
	original := <-initial
	expected, _ := json.Marshal(original)
	badRequest := request
	badRequest.Args = map[string]any{"command": "do something else"}
	if _, err := run.park(ctx, badRequest); err == nil {
		t.Fatal("different arguments reused a cached callback identity")
	}
	s := &callbackReplayServer{callback: func() error {
		result, err := run.park(ctx, request)
		if err != nil {
			return err
		}
		got, _ := json.Marshal(result)
		if string(got) != string(expected) {
			return errors.New("callback retry did not get the cached result")
		}
		return nil
	}}
	_, handler := sdkv1connect.NewSdkAgentServiceHandler(s)
	server := httptest.NewServer(handler)
	defer server.Close()
	run.agent.client = &Client{Agent: sdkv1connect.NewSdkAgentServiceClient(server.Client(), server.URL), log: testRecoveryLogger()}
	if err := run.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	results := 0
	for {
		event, ok := run.Next(ctx)
		if !ok {
			break
		}
		switch event.Kind {
		case EventToolCallStarted:
			t.Fatal("replayed callback queued another native execution")
		case EventTextDelta:
			text.WriteString(event.Text)
		case EventResult:
			results++
		}
	}
	if ctx.Err() != nil || run.DrainErr() != nil || text.String() != "continued safely" || results != 1 || s.observes.Load() != 2 {
		t.Fatalf("host recovery failed: text=%q results=%d observes=%d err=%v ctx=%v", text.String(), results, s.observes.Load(), run.DrainErr(), ctx.Err())
	}
}
