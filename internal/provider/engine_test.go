package provider

import (
	"errors"
	"testing"
)

// Two runs in one directory must not end up sharing a Cursor agent unless they are
// actually the same conversation. Sharing one is how a session that exits closes
// the agent — or leaves a run holding it — while another session is still talking
// to it.
func TestConversationKeyKeepsUnrelatedSessionsApart(t *testing.T) {
	const dir = "/work/repo"

	durable := conversationKey("/sessions/a.jsonl", dir, "grok-4.7", "n1")
	if durable != conversationKey("/sessions/a.jsonl", dir, "grok-4.7", "n2") {
		t.Fatal("the same session file must reuse the same agent across processes")
	}
	if durable == conversationKey("/sessions/b.jsonl", dir, "grok-4.7", "n1") {
		t.Fatal("two different sessions in one directory must not share an agent")
	}

	first := conversationKey("", dir, "grok-4.7", "n1")
	if first != conversationKey("", dir, "grok-4.7", "n1") {
		t.Fatal("one ephemeral run must keep its key across its own turns")
	}
	if first == conversationKey("", dir, "grok-4.7", "n2") {
		t.Fatal("two ephemeral runs in one directory must not share an agent")
	}
	if conversationKey("   ", dir, "grok-4.7", "n1") != first {
		t.Fatal("a blank session file is the same as no session file")
	}
}

// A send the agent never received must be re-sent, not mistaken for a rewritten
// branch, which would otherwise rebuild the agent and replay the transcript.
func TestUndoSendRestoresTheTurnThatNeverLanded(t *testing.T) {
	first := ParsePiContext(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "first"},
		}}},
	})
	second := ParsePiContext(map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "first"}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "ok"}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "second"}}},
		},
	})

	s := &Session{}
	if text, appended := s.appendedText(first); !appended || text == "" {
		t.Fatalf("a new conversation should send its first message, got %q/%v", text, appended)
	}
	s.sentFingerprints = fingerprints(first)

	text, appended := s.appendedText(second)
	if !appended || text == "" {
		t.Fatalf("an appended turn should send only the new text, got %q/%v", text, appended)
	}
	// The turn is put on the wire, and the run dies before the agent sees it.
	s.prevFingerprints = s.sentFingerprints
	s.sentFingerprints = fingerprints(second)

	s.undoSend()
	again, appended := s.appendedText(second)
	if !appended || again != text {
		t.Fatalf("retry must re-send %q, got %q/%v", text, again, appended)
	}
	if len(s.prevFingerprints) != 0 {
		t.Fatal("undoSend should not leave a stale snapshot behind")
	}
}

// Both wordings for "a run nobody is reading blocks this agent" must take the same
// recovery path. The bridge says one; this package's own guard said the other, and
// that second one used to fail the turn outright.
func TestStaleAgentErrorMatchesBothWordings(t *testing.T) {
	for _, msg := range []string{
		"cursor run stream: Agent c8c7d38a already has active run (unknown)",
		"cursor: run  is still active for this agent",
		"cursor: run run-42 is still active for this agent",
	} {
		if !isStaleAgentError(errors.New(msg)) {
			t.Errorf("should be treated as stale: %q", msg)
		}
	}
	for _, msg := range []string{"cursor: agent is closed", "context deadline exceeded", ""} {
		if isStaleAgentError(errors.New(msg)) {
			t.Errorf("should not be treated as stale: %q", msg)
		}
	}
	if isStaleAgentError(nil) {
		t.Error("nil is not a stale agent error")
	}
}
