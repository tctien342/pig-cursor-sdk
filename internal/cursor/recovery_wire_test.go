package cursor

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	"github.com/tctien342/pig-cursor-sdk/gen/sdk/v1/sdkv1connect"
	"google.golang.org/protobuf/proto"
)

// Real incomplete Connect framing, not just an error string returned by a mock.
// Valid envelopes are fragmented across writes, followed by a truncated header.
func TestRecoveryFromTruncatedWireEnvelope(t *testing.T) {
	s := &recoveryServer{offsets: make(chan string, 3)}
	_, observe := sdkv1connect.NewSdkAgentServiceHandler(s)
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/Send") {
			observe.ServeHTTP(w, r)
			return
		}
		sends.Add(1)
		w.Header().Set("Content-Type", "application/connect+proto")
		for _, frame := range []*sdkpb.RunStreamMessage{recoveryStatus(), recoveryText("abc", "")} {
			payload, err := proto.Marshal(frame)
			if err != nil {
				t.Error(err)
				return
			}
			header := make([]byte, 5)
			binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
			_, _ = w.Write(header[:2])
			w.(http.Flusher).Flush()
			_, _ = w.Write(header[2:])
			_, _ = w.Write(payload)
			w.(http.Flusher).Flush()
		}
		_, _ = w.Write([]byte{0, 0}) // EOF before the remaining length bytes
	}))
	defer server.Close()
	agent := &Agent{id: "agent", toolSet: map[string]bool{"bash": true}, client: &Client{log: testRecoveryLogger(), Agent: sdkv1connect.NewSdkAgentServiceClient(server.Client(), server.URL)}}
	run, err := agent.Send(context.Background(), "one request", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer run.abandon(context.Canceled)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var text strings.Builder
	for {
		ev, ok := run.Next(ctx)
		if !ok {
			break
		}
		if ev.Kind == EventTextDelta {
			text.WriteString(ev.Text)
		}
	}
	if ctx.Err() != nil || run.DrainErr() != nil || text.String() != "abcdef" || sends.Load() != 1 {
		t.Fatalf("unsafe/incomplete recovery: text=%q sends=%d err=%v ctx=%v", text.String(), sends.Load(), run.DrainErr(), ctx.Err())
	}
}

func TestSettledHostCallbackReplayDoesNotQueueAnotherExecution(t *testing.T) {
	run := newTestRun()
	defer run.abandon(context.Canceled)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := ToolCallRequest{ToolCallID: "same-host-call", ToolName: "bash", Args: map[string]any{"command": "echo once"}}
	done := make(chan error, 1)
	go func() { _, err := run.park(ctx, req); done <- err }()
	if _, ok := run.Next(ctx); !ok {
		t.Fatal("first callback did not queue")
	}
	if !run.ResolveHostCall(req.ToolCallID, map[string]any{"text": "once"}, false, "") {
		t.Fatal("could not settle first callback")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	result, err := run.park(ctx, req)
	if err != nil || result == nil {
		t.Fatalf("duplicate callback should get the stored result: %v", err)
	}
	if _, ok := run.NextReady(); ok {
		t.Fatal("duplicate settled callback queued another host execution")
	}
}
