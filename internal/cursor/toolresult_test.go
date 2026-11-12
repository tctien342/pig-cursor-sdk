package cursor

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestHostToolResultEnvelopePreservesTextImagesAndErrors(t *testing.T) {
	payload := map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "tool failed with details"},
		map[string]any{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"},
	}, "structuredContent": map[string]any{"ok": false}}
	before := hostToolResult(payload, true, "")
	result := hostToolResult(payload, true, "unused fallback")
	if result["isError"] != true {
		t.Fatal("ordinary tool failure lost isError")
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatal("bridge-recognized content envelope was not returned")
	}
	if !reflect.DeepEqual(content, payload["content"]) || !reflect.DeepEqual(before, result) {
		t.Fatal("tool content or image was changed")
	}
	if !reflect.DeepEqual(result["structuredContent"], payload["structuredContent"]) {
		t.Fatal("structured tool result dropped")
	}
	empty := hostToolResult(map[string]any{"content": ""}, false, "must not fabricate this")
	if payloadText(empty) != "" {
		t.Fatal("empty success replaced with fallback text")
	}
}

func TestOrdinaryToolFailureIsResultNotCallbackTransportError(t *testing.T) {
	run := newTestRun()
	defer run.abandon(context.Canceled)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type outcome struct {
		result map[string]any
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := run.park(ctx, ToolCallRequest{ToolCallID: "failure", ToolName: "bash"})
		done <- outcome{result, err}
	}()
	if _, ok := run.Next(ctx); !ok {
		t.Fatal("tool was not queued")
	}
	run.ResolveHostCall("failure", map[string]any{"content": "command exited 1"}, true, "command exited 1")
	got := <-done
	if got.err != nil || got.result["isError"] != true || payloadText(got.result) != "command exited 1" {
		t.Fatalf("ordinary error became infrastructure failure: %+v", got)
	}
	cached, err := run.park(ctx, ToolCallRequest{ToolCallID: "failure", ToolName: "bash"})
	if err != nil || cached["isError"] != true {
		t.Fatal("error outcome did not survive callback replay")
	}
}
