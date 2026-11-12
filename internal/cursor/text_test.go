package cursor

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	"github.com/tctien342/pig-cursor-sdk/gen/sdk/v1/sdkv1connect"
)

func TestEventTextFallbackRepairsWithoutMutatingInput(t *testing.T) {
	bad := "Tiếng Việt 漢🙂 \xff\xff end\xc3"
	want := "Tiếng Việt 漢🙂 � end�"
	e := Event{Text: bad, RunID: "raw\xff-id", Offset: "raw\xff-offset",
		Status:   &StatusEvent{Message: bad},
		ToolCall: &ToolCallEvent{ID: "tool\xff-id", ToolName: "tool\xff-name", Text: bad},
		Result:   &RunResult{Text: bad, ErrorMessage: bad}, Task: &TaskEvent{Text: bad},
	}
	fixed := e.WithValidText()
	for name, got := range map[string]string{"text": fixed.Text, "status": fixed.Status.Message,
		"tool": fixed.ToolCall.Text, "result": fixed.Result.Text, "error": fixed.Result.ErrorMessage, "task": fixed.Task.Text} {
		if got != want || !utf8.ValidString(got) {
			t.Errorf("%s=%q, want %q", name, got, want)
		}
	}
	if e.Text != bad || e.Status.Message != bad || e.ToolCall.Text != bad || e.Result.Text != bad || e.Result.ErrorMessage != bad || e.Task.Text != bad {
		t.Fatal("repair mutated the producer's event")
	}
	if fixed.RunID != e.RunID || fixed.Offset != e.Offset || fixed.ToolCall.ID != e.ToolCall.ID || fixed.ToolCall.ToolName != e.ToolCall.ToolName {
		t.Fatal("repair must not rewrite machine identities")
	}
	const valid = "Valid Tiếng Việt 漢字 🙂"
	if got := (Event{Text: valid}).WithValidText().Text; got != valid {
		t.Fatalf("valid Unicode changed: %q", got)
	}
	if got := (Event{}).WithValidText(); got.Status != nil || got.Result != nil || got.Task != nil || got.ToolCall != nil {
		t.Fatal("nil fields became non-nil")
	}
}

func TestDecodeTextFallbackRepairsTerminalResult(t *testing.T) {
	bad := "answer\xff"
	msg := &sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_Result{Result: &sdkpb.RunStreamResult{
		Status: sdkpb.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR,
		Result: &sdkpb.RunResult{Status: sdkpb.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_ERROR, Result: bad, ErrorMessage: &bad},
	}}}
	events := Decode(msg, true)
	if len(events) != 1 || events[0].Result.Text != "answer�" || events[0].Result.ErrorMessage != "answer�" || events[0].Status.Message != "answer�" {
		t.Fatalf("decoded text not repaired: %+v", events)
	}
	if msg.GetResult().GetResult().GetResult() != bad {
		t.Fatal("Decode mutated its source protobuf")
	}
}

type textFallbackServer struct {
	sdkv1connect.UnimplementedSdkAgentServiceHandler
	received chan string
}

func (s *textFallbackServer) Send(_ context.Context, req *connect.Request[sdkpb.SendRequest], stream *connect.ServerStream[sdkpb.RunStreamMessage]) error {
	s.received <- req.Msg.GetMessage().GetText()
	return stream.Send(&sdkpb.RunStreamMessage{Envelope: &sdkpb.RunStreamMessage_Done{Done: &sdkpb.RunStreamDone{}}})
}

func TestSendRepairsMalformedPromptBeforeProtobuf(t *testing.T) {
	handler := &textFallbackServer{received: make(chan string, 1)}
	_, h := sdkv1connect.NewSdkAgentServiceHandler(handler)
	server := httptest.NewServer(h)
	defer server.Close()
	agent := &Agent{id: "test-agent", client: &Client{Agent: sdkv1connect.NewSdkAgentServiceClient(server.Client(), server.URL)}}
	run, err := agent.Send(context.Background(), "valid 漢🙂 \xff\xff incomplete\xc3", nil)
	if err != nil {
		t.Fatalf("malformed display text must not fail Send: %v", err)
	}
	defer run.abandon(context.Canceled)
	select {
	case text := <-handler.received:
		if text != "valid 漢🙂 � incomplete�" {
			t.Fatalf("server received %q", text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive repaired prompt")
	}
}
