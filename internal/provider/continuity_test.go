package provider

import (
	"strings"
	"testing"
)

func TestImageOnlyTurnIsAppendOnly(t *testing.T) {
	first := PiContext{Messages: []Message{{Role: "user", Blocks: []Block{{Type: "text", Text: "Inspect this project."}}}}}
	s := &Session{sentFingerprints: fingerprints(first)}
	next := PiContext{Messages: append(append([]Message(nil), first.Messages...),
		Message{Role: "assistant", Blocks: []Block{{Type: "text", Text: "Ready."}}},
		Message{Role: "user", Blocks: []Block{{Type: "image", Text: "image/png", Args: map[string]any{"data": "aGVsbG8="}}}},
	)}
	text, ok := s.appendedText(next)
	if !ok || text != "" || len(next.Images()) != 1 {
		t.Fatalf("image-only turn forced replay: text=%q append=%v", text, ok)
	}
	// Without a new user message, do not repeat the earlier user request.
	next.Messages = next.Messages[:len(next.Messages)-1]
	if _, ok := s.appendedText(next); ok {
		t.Fatal("assistant-only suffix resends an old user request")
	}
}

func TestContinuityDetectsCompleteContentNotOnlyPrefixes(t *testing.T) {
	cases := []struct {
		name          string
		before, after Message
	}{
		{"long text", Message{Role: "user", Blocks: []Block{{Type: "text", Text: strings.Repeat("x", 5000) + "a"}}}, Message{Role: "user", Blocks: []Block{{Type: "text", Text: strings.Repeat("x", 5000) + "b"}}}},
		{"image data", Message{Role: "user", Blocks: []Block{{Type: "image", Text: "image/png", Args: map[string]any{"data": strings.Repeat("A", 5000) + "a"}}}}, Message{Role: "user", Blocks: []Block{{Type: "image", Text: "image/png", Args: map[string]any{"data": strings.Repeat("A", 5000) + "b"}}}}},
		{"tool args", Message{Role: "assistant", Blocks: []Block{{Type: "toolCall", ToolID: "call", ToolName: "bash", Args: map[string]any{"command": strings.Repeat("x", 5000) + "a"}}}}, Message{Role: "assistant", Blocks: []Block{{Type: "toolCall", ToolID: "call", ToolName: "bash", Args: map[string]any{"command": strings.Repeat("x", 5000) + "b"}}}}},
		{"tool error", Message{Role: "toolResult", Raw: map[string]any{"toolCallId": "call", "isError": false}, Blocks: []Block{{Type: "text", Text: "same output"}}}, Message{Role: "toolResult", Raw: map[string]any{"toolCallId": "call", "isError": true}, Blocks: []Block{{Type: "text", Text: "same output"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.before.Fingerprint() == c.after.Fingerprint() {
				t.Fatal("meaningful content edit was missed")
			}
			s := &Session{sentFingerprints: []string{c.before.Fingerprint()}}
			rewritten := PiContext{Messages: []Message{c.after, {Role: "user", Blocks: []Block{{Type: "text", Text: "Continue."}}}}}
			if _, ok := s.appendedText(rewritten); ok {
				t.Fatal("rewritten prefix incorrectly treated as append-only")
			}
		})
	}
}
