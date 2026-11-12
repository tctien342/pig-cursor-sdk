package provider

import (
	"encoding/json"
	"strings"
	"testing"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func internalCall(id, command string, kind cursor.EventKind) cursor.RunEvent {
	return cursor.RunEvent{Event: cursor.Event{Kind: kind, ToolCall: &cursor.ToolCallEvent{
		ID: id, ToolName: "shell", Args: map[string]any{"command": command},
	}}}
}

func annotationBlocks(em *turnEmitter) []string {
	var out []string
	for _, raw := range em.p.content {
		b := raw.(map[string]any)
		if firstString(b, "textSignature") == cursorToolTextSignature {
			out = append(out, firstString(b, "text"))
		}
	}
	return out
}

func TestInlineToolsFollowEventOrder(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	em.add(text("Before call one."))
	a := internalCall("a", "echo one", cursor.EventCursorToolStarted)
	em.add(a)
	em.add(a) // SDK duplicates must not insert a second annotation
	em.add(internalCall("a", "echo one", cursor.EventCursorToolCompleted))
	em.add(thinking("Now inspect the result."))
	em.add(internalCall("b", "echo two", cursor.EventCursorToolStarted))
	em.add(internalCall("b", "echo two", cursor.EventCursorToolCompleted))
	em.add(text("After call two."))
	em.finish("stop", nil)
	if len(em.p.content) != 5 {
		t.Fatalf("want text/call/thinking/call/text: %#v", em.p.content)
	}
	want := []string{"Before call one.", "echo one", "Now inspect the result.", "echo two", "After call two."}
	kinds := []string{"text", "text", "thinking", "text", "text"}
	for i, raw := range em.p.content {
		b := raw.(map[string]any)
		value := firstString(b, "text", "thinking")
		if !strings.Contains(value, want[i]) || firstString(b, "type") != kinds[i] {
			t.Errorf("block %d: %#v", i, b)
		}
	}
	if len(em.tools) != 0 || len(em.pending) != 0 || em.p.msg["stopReason"] != "stop" {
		t.Fatal("internal call became executable")
	}
	// Each block has one start/end pair, with no overlapping text/thinking runs.
	open := -1
	for _, ev := range em.events() {
		typ := firstString(ev, "type")
		if typ == "text_start" || typ == "thinking_start" {
			if open != -1 {
				t.Fatalf("overlapping stream blocks: %v", ev)
			}
			open = ev["contentIndex"].(int)
		}
		if typ == "text_end" || typ == "thinking_end" {
			if open != ev["contentIndex"].(int) {
				t.Fatalf("closing wrong block: %v", ev)
			}
			open = -1
		}
	}
	if open != -1 {
		t.Fatal("unclosed block")
	}
}

func TestInlineToolsTerminalAnswerAndOutcomes(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	em.add(thinking("Before."))
	em.add(internalCall("a", "echo a", cursor.EventCursorToolStarted))
	done := internalCall("a", "echo a", cursor.EventCursorToolCompleted)
	done.ToolCall.IsError = true
	em.add(done)
	em.add(done)
	em.add(internalCall("b", "echo b", cursor.EventCursorToolStarted))
	em.finish("stop", &cursor.RunResult{Status: "FINISHED", Text: "Terminal-only answer."})
	notes := annotationBlocks(em)
	if len(notes) != 4 {
		t.Fatalf("start a, failure a, start b, unfinished b: %q", notes)
	}
	if !strings.Contains(notes[1], "failed") || !strings.Contains(notes[3], "no result") {
		t.Fatalf("outcomes missing: %q", notes)
	}
	if !strings.Contains(em.text(), "Terminal-only answer.") {
		t.Fatal("annotations swallowed terminal fallback")
	}
	if em.p.thinkOpen || em.p.textOpen {
		t.Fatal("finish left open blocks")
	}
}

func TestInlineToolsCancellationClosesBlocks(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	em.add(thinking("Before the call."))
	em.add(internalCall("a", "sleep 30", cursor.EventCursorToolStarted))
	em.add(thinking("Waiting for the call."))
	if reason := em.finish("aborted", &cursor.RunResult{Status: "CANCELLED"}); reason != "aborted" {
		t.Fatalf("cancellation lost: %s", reason)
	}
	if em.p.thinkOpen || em.p.textOpen || len(em.cursorTools.snapshot()) != 0 {
		t.Fatal("cancelled turn left a block or hint active")
	}
	notes := annotationBlocks(em)
	if len(notes) != 2 || !strings.Contains(notes[1], "no result") {
		t.Fatalf("cancelled call must not claim success: %q", notes)
	}
}

func TestInlineToolsDisabled(t *testing.T) {
	t.Setenv("PI_CURSOR_SHOW_TOOLS", "0")
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	em.add(text("Only prose."))
	em.add(internalCall("a", "echo a", cursor.EventCursorToolStarted))
	em.finish("stop", nil)
	if len(annotationBlocks(em)) != 0 || len(em.p.content) != 1 || len(em.cursorTools.snapshot()) != 0 {
		t.Fatal("disabled tools emitted display")
	}
}

func TestInlineToolsConsecutiveAndAnonymous(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	ev := internalCall("", "echo anonymous", cursor.EventCursorToolStarted)
	em.add(ev)
	em.add(ev)
	em.add(internalCall("", "echo anonymous", cursor.EventCursorToolCompleted))
	em.add(internalCall("b", "echo next\n\x1b[31munsafe", cursor.EventCursorToolStarted))
	em.add(internalCall("b", "", cursor.EventCursorToolCompleted))
	em.finish("stop", nil)
	if len(annotationBlocks(em)) != 2 {
		t.Fatalf("consecutive dedup failed: %q", annotationBlocks(em))
	}
	for _, line := range annotationBlocks(em) {
		if strings.Contains(line, "\x1b") || strings.Contains(line, "next\n") {
			t.Fatalf("unsanitized summary %q", line)
		}
	}
	if !em.p.hasNoText() {
		t.Fatal("display counted as substantive text")
	}
}

func TestInlineToolReplayStripsOnlyMarkedAssistantBlocks(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	em.add(text("Keep this prose."))
	em.add(internalCall("a", "echo extension-only", cursor.EventCursorToolStarted))
	em.add(internalCall("a", "echo extension-only", cursor.EventCursorToolCompleted))
	em.add(text("◇ cursor: shell · ordinary quoted prose"))
	em.finish("stop", nil)
	// JSON round trip represents the persisted format. The PTY test also checks the
	// actual host keeps textSignature when it saves and resumes the message.
	raw, _ := json.Marshal(em.p.msg)
	var message map[string]any
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatal(err)
	}
	ctx := ParsePiContext(map[string]any{"messages": []any{
		message,
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Keep user text.", "textSignature": cursorToolTextSignature}}},
	}})
	replay := ctx.ReplayPrompt(2000)
	if strings.Contains(replay, "extension-only") {
		t.Fatalf("display leaked to Cursor replay: %s", replay)
	}
	for _, keep := range []string{"Keep this prose.", "ordinary quoted prose", "Keep user text."} {
		if !strings.Contains(replay, keep) {
			t.Fatalf("removed real prose %q: %s", keep, replay)
		}
	}
}
