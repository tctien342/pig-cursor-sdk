package provider

import (
	"reflect"
	"testing"
)

func TestToolOutcomePreservesNativePayloadAndFailure(t *testing.T) {
	content := []any{map[string]any{"type": "text", "text": "failure detail"}, map[string]any{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"}}
	raw := map[string]any{"role": "toolResult", "toolCallId": "native", "content": content, "isError": true, "structuredContent": map[string]any{"exitCode": 1}, "details": map[string]any{"nativeExpansion": "keep in pig"}}
	context := ParsePiContext(map[string]any{"messages": []any{raw}})
	outcome := context.PendingToolResults([]string{"native"})["native"]
	if !outcome.IsError || outcome.Text != "failure detail" {
		t.Fatalf("outcome=%+v", outcome)
	}
	if !reflect.DeepEqual(outcome.Payload()["content"], content) || !reflect.DeepEqual(outcome.Payload()["structuredContent"], raw["structuredContent"]) {
		t.Fatal("supported tool result content was dropped")
	}
	if _, ok := outcome.Payload()["details"]; ok {
		t.Fatal("native expansion metadata unnecessarily leaked to Cursor")
	}
	if !reflect.DeepEqual(raw["content"], content) || raw["details"].(map[string]any)["nativeExpansion"] != "keep in pig" {
		t.Fatal("native transcript/expansion data was mutated")
	}
}
