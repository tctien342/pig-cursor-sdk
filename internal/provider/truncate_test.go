package provider

import (
	"strings"
	"testing"
	"unicode/utf8"

	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
	"google.golang.org/protobuf/proto"
)

func TestTruncateKeepsUTF8AtEveryByteBoundary(t *testing.T) {
	for _, s := range []string{"ASCII text", "ééé", "漢字漢", "🙂🚀🙂", "aé漢🙂z"} {
		for n := 1; n <= len(s)+1; n++ {
			got := truncate(s, n)
			if !utf8.ValidString(got) {
				t.Fatalf("truncate(%q,%d) produced invalid UTF-8: %x", s, n, got)
			}
			want := s
			if n < len(s) {
				end := n
				for end > 0 && !utf8.RuneStart(s[end]) {
					end--
				}
				want = s[:end] + "…[truncated]"
			}
			if got != want {
				t.Errorf("truncate(%q,%d)=%q, want %q", s, n, got, want)
			}
		}
	}
	if got := truncate("  hello world  ", 5); got != "hello…[truncated]" {
		t.Fatalf("ASCII behavior changed: %q", got)
	}
	for _, n := range []int{0, -1, 100} {
		if got := truncate("  hé🙂  ", n); got != "hé🙂" {
			t.Errorf("untruncated behavior changed: %q", got)
		}
	}
}

func TestTruncateRepairsMalformedInputBeforeLimiting(t *testing.T) {
	for _, s := range []string{"bad\xfftext", "\xc3", "\xf0\x9f", "a\x80\x80b"} {
		for _, n := range []int{-1, 0, 1, 2, 3, 20} {
			if got := truncate(s, n); !utf8.ValidString(got) {
				t.Errorf("invalid output for %x at %d: %x", s, n, got)
			}
		}
	}
	if got := truncate("a\xffb", 0); got != "a�b" {
		t.Fatalf("repair = %q", got)
	}
}

func TestReplayPromptUTF8CanBeMarshaledAsSendRequest(t *testing.T) {
	// This is the former behavior at the actual 400-byte tool-result budget.
	original := strings.Repeat("x", 399) + "é ending"
	broken := original[:400] + "…[truncated]"
	if _, err := proto.Marshal(&sdkpb.SendRequest{Message: &sdkpb.UserMessage{Text: broken}}); err == nil {
		t.Fatal("fixture must reproduce protobuf's invalid UTF-8 error")
	}
	for _, role := range []string{"user", "assistant", "toolResult", "bashExecution"} {
		input := strings.Repeat("x", 1999) + "漢🙂 ending"
		if role == "toolResult" || role == "bashExecution" {
			input = original
		}
		ctx := ParsePiContext(map[string]any{"messages": []any{
			map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": input}}},
		}})
		prompt := ctx.ReplayPrompt(2000)
		if !strings.Contains(prompt, "…[truncated]") {
			t.Fatalf("fixture for %s did not reach cutoff", role)
		}
		if strings.ContainsRune(prompt, '�') {
			t.Fatalf("valid Unicode was corrupted for %s", role)
		}
		if _, err := proto.Marshal(&sdkpb.SendRequest{Message: &sdkpb.UserMessage{Text: prompt}}); err != nil {
			t.Fatalf("%s replay cannot be sent to Cursor: %v", role, err)
		}
	}
	// Tool arguments use a separate 200-byte budget after JSON encoding.
	ctx := PiContext{Messages: []Message{{Role: "assistant", Blocks: []Block{
		{Type: "toolCall", ToolName: "read", Args: map[string]any{"path": strings.Repeat("x", 190) + "🙂/file"}},
	}}}}
	prompt := ctx.ReplayPrompt(2000)
	if _, err := proto.Marshal(&sdkpb.SendRequest{Message: &sdkpb.UserMessage{Text: prompt}}); err != nil {
		t.Fatalf("tool argument replay cannot be sent to Cursor: %v", err)
	}
}
