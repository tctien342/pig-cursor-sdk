package cursor

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

func testRecoveryLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func recoveryStatus() *sdkpb.RunStreamMessage {
	payload, _ := structpb.NewStruct(map[string]any{"status": "running", "run_id": "recover-run", "agent_id": "agent"})
	return &sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_SdkMessage{SdkMessage: &sdkpb.SdkMessage{Type: "status", Message: payload}}}
}
func recoveryText(text string, offset string) *sdkpb.RunStreamMessage {
	payload, _ := structpb.NewStruct(map[string]any{"text": text})
	msg := &sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_InteractionUpdate{InteractionUpdate: &sdkpb.InteractionUpdate{Type: "text-delta", Update: payload}}}
	if offset != "" {
		msg.Offset = &offset
	}
	return msg
}
func recoveryTool(status string) *sdkpb.RunStreamMessage {
	payload, _ := structpb.NewStruct(map[string]any{"call_id": "own-1", "name": "shell", "status": status, "args": map[string]any{"command": "echo once"}})
	return &sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_SdkMessage{SdkMessage: &sdkpb.SdkMessage{Type: "tool_call", Message: payload}}}
}

type recoveryServer struct {
	sdkv1connect.UnimplementedSdkAgentServiceHandler
	mode            string
	sends, observes atomic.Int32
	offsets         chan string
}

func interruptedFrame() error {
	return connect.NewError(connect.CodeUnavailable, errors.New("protocol error: incomplete envelope: chunked line ends with bare LF"))
}
func (s *recoveryServer) Send(_ context.Context, _ *connect.Request[sdkpb.SendRequest], stream *connect.ServerStream[sdkpb.RunStreamMessage]) error {
	s.sends.Add(1)
	if s.mode != "no-id" {
		if err := stream.Send(recoveryStatus()); err != nil {
			return err
		}
	}
	for _, msg := range []*sdkpb.RunStreamMessage{recoveryText("abc", "send-offset-not-reusable"), recoveryTool("running"), recoveryTool("completed")} {
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
	return interruptedFrame()
}
func (s *recoveryServer) ObserveRun(_ context.Context, req *connect.Request[sdkpb.ObserveRunRequest], stream *connect.ServerStream[sdkpb.RunStreamMessage]) error {
	n := s.observes.Add(1)
	s.offsets <- req.Msg.GetAfterOffset()
	if req.Msg.GetRunId() != "recover-run" {
		return errors.New("wrong run")
	}
	if s.mode == "exhausted" {
		return interruptedFrame()
	}
	if s.mode == "mismatch" {
		return stream.Send(recoveryText("different", ""))
	}
	if s.mode == "short-replay" || s.mode == "premature-done" {
		if err := stream.Send(recoveryText("ab", "")); err != nil {
			return err
		}
		if s.mode == "premature-done" {
			return stream.Send(&sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_Done{Done: &sdkpb.RunStreamDone{}}})
		}
		return nil
	}
	var frames []*sdkpb.RunStreamMessage
	if n == 1 {
		frames = []*sdkpb.RunStreamMessage{recoveryText("ab", ""), recoveryTool("running"), recoveryText("c", ""), recoveryTool("completed"), recoveryText("d", "durable-d")}
	} else {
		frames = []*sdkpb.RunStreamMessage{recoveryText("ef", "durable-ef")}
	}
	for _, msg := range frames {
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
	if n == 1 {
		return interruptedFrame()
	}
	return stream.Send(&sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_Result{Result: &sdkpb.RunStreamResult{Result: &sdkpb.RunResult{
		RunId: "recover-run", Status: sdkpb.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_FINISHED, Result: "abcdef",
	}}}})
}

func TestStreamRecoveryReattachesWithoutResendingOrDuplicating(t *testing.T) {
	s := &recoveryServer{offsets: make(chan string, 3)}
	run, text, starts, ends := exerciseRecovery(t, s)
	if text != "abcdef" {
		t.Fatalf("replayed/lost text: %q", text)
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("internal calls replayed: %d/%d", starts, ends)
	}
	if run.DrainErr() != nil {
		t.Fatalf("recovery failed: %v", run.DrainErr())
	}
	if s.sends.Load() != 1 || s.observes.Load() != 2 {
		t.Fatalf("Send=%d Observe=%d", s.sends.Load(), s.observes.Load())
	}
	if first, second := <-s.offsets, <-s.offsets; first != "" || second != "durable-d" {
		t.Fatalf("wrong offset contract: %q %q", first, second)
	}
}

func TestStreamRecoveryFailsSafelyWhenContinuityUnknown(t *testing.T) {
	for _, mode := range []string{"no-id", "mismatch", "exhausted", "short-replay", "premature-done"} {
		t.Run(mode, func(t *testing.T) {
			s := &recoveryServer{mode: mode, offsets: make(chan string, 3)}
			run, text, _, _ := exerciseRecovery(t, s)
			if text != "abc" || run.DrainErr() == nil {
				t.Fatalf("must retain partial text and fail safely: text=%q err=%v", text, run.DrainErr())
			}
			if s.sends.Load() != 1 || s.observes.Load() > maxStreamRecoveries {
				t.Fatal("prompt resent or unbounded retries")
			}
			if mode == "no-id" && s.observes.Load() != 0 {
				t.Fatal("attempted to resume an unknown run")
			}
		})
	}
}

func exerciseRecovery(t *testing.T, s *recoveryServer) (*Run, string, int, int) {
	t.Helper()
	_, h := sdkv1connect.NewSdkAgentServiceHandler(s)
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	agent := &Agent{id: "agent", toolSet: map[string]bool{"bash": true}, client: &Client{log: testRecoveryLogger(), Agent: sdkv1connect.NewSdkAgentServiceClient(server.Client(), server.URL)}}
	run, err := agent.Send(context.Background(), "one request", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run.abandon(context.Canceled) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var text strings.Builder
	starts, ends := 0, 0
	for {
		ev, ok := run.Next(ctx)
		if !ok {
			break
		}
		switch ev.Kind {
		case EventTextDelta:
			text.WriteString(ev.Text)
		case EventCursorToolStarted:
			starts++
		case EventCursorToolCompleted:
			ends++
		}
	}
	if ctx.Err() != nil {
		t.Fatal("run did not finish within test deadline")
	}
	return run, text.String(), starts, ends
}

func TestInterruptedStreamClassification(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("invalid API key"), errors.New("unknown agent"), nil} {
		if IsInterruptedStream(err) {
			t.Fatalf("must not retry %v", err)
		}
	}
	if !IsInterruptedStream(interruptedFrame()) {
		t.Fatal("reported framing error was not recognized")
	}
}
