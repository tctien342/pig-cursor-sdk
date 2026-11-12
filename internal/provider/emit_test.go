package provider

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

// The display rules this extension promises, enforced as tests. They exist
// because the integration it replaces put Cursor's tool traces into the
// thinking channel and persisted no tool card at all, which is what made the
// transcript unreadable.

func modelMap() map[string]any {
	return map[string]any{"id": "grok-4.7@500k", "provider": "cursor", "api": "pig-cursor-sdk"}
}

func entryFor() *ModelEntry {
	return &ModelEntry{ID: "grok-4.7@500k", BaseID: "grok-4.7", Name: "Grok 4.7"}
}

func thinking(s string) cursor.RunEvent {
	return cursor.RunEvent{Event: cursor.Event{Kind: cursor.EventThinkingDelta, Text: s}}
}

func text(s string) cursor.RunEvent {
	return cursor.RunEvent{Event: cursor.Event{Kind: cursor.EventTextDelta, Text: s}}
}

func hostCall(id, name string, args map[string]any) cursor.RunEvent {
	return cursor.RunEvent{
		Event: cursor.Event{Kind: cursor.EventToolCallStarted, ToolCall: &cursor.ToolCallEvent{
			ID: id, ToolName: name, Args: args, Status: "running",
		}},
		Pending: &cursor.ParkedCall{ID: id, ToolName: name, Args: args},
	}
}

// cursorOwnTool models a tool Cursor executed itself: it has a card upstream but
// no tool the host can run, so it must not appear in the transcript.
func cursorOwnTool(id, name string) cursor.RunEvent {
	return cursor.RunEvent{Event: cursor.Event{Kind: cursor.EventToolCallStarted, ToolCall: &cursor.ToolCallEvent{
		ID: id, ToolName: name, Status: "running",
	}}}
}

func TestEmitsOnlyTextThinkingAndHostToolCalls(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	// Only bash was advertised to Cursor, so "Shell" is Cursor's own tool.
	em.hostToolSet = map[string]bool{"bash": true}
	var notices []string
	em.notice = func(verb, name string) { notices = append(notices, verb+" "+name) }
	em.begin()
	events := []cursor.RunEvent{
		thinking("The user wants a shell command. "),
		text("I'll run it. "),
		cursorOwnTool("call_cursor_shell", "Shell"),
		cursor.RunEvent{Event: cursor.Event{Kind: cursor.EventToolCallCompleted, ToolCall: &cursor.ToolCallEvent{ID: "call_cursor_shell", ToolName: "Shell", Status: "completed", Text: "$ echo hi\nhi"}}},
		hostCall("call_1", "bash", map[string]any{"command": "echo hi"}),
	}
	for _, ev := range events {
		if em.add(ev) {
			break
		}
	}
	em.finish("toolUse", nil)

	allowed := map[string]bool{
		"start": true, "thinking_start": true, "thinking_delta": true, "thinking_end": true,
		"text_start": true, "text_delta": true, "text_end": true,
		"toolcall_start": true, "toolcall_delta": true, "toolcall_end": true,
		"done": true, "error": false,
	}
	var types []string
	for _, ev := range em.events() {
		typ, _ := ev["type"].(string)
		types = append(types, typ)
		if !allowed[typ] {
			t.Fatalf("extension emitted a disallowed provider event %q (rules: text, thinking, host tool calls only)", typ)
		}
	}

	msg := em.p.msg
	roles := map[string]int{}
	for _, blk := range msg["content"].([]any) {
		m, _ := blk.(map[string]any)
		roles[firstString(m, "type")]++
	}
	if roles["toolCall"] != 1 {
		t.Fatalf("want exactly one tool card, got %d (%v)", roles["toolCall"], roles)
	}
	if roles["thinking"] != 1 || roles["text"] != 1 {
		t.Fatalf("want one thinking and one text block, got %v", roles)
	}

	// Cursor's own tool activity must leave nothing behind, but must not be
	// silently invisible either: the footer is told about it.
	if len(notices) != 1 || notices[0] != "ran Shell" {
		t.Fatalf("notices = %v, want exactly one \"ran Shell\" footer notice", notices)
	}
	joined := em.text() + thinkingText(t, msg)
	for _, banned := range []string{"$ echo hi", "call_cursor_shell", "Shell", "tool_call", "\"type\""} {
		if strings.Contains(joined, banned) {
			t.Fatalf("transcript leaked Cursor-side tool noise %q into %q", banned, joined)
		}
	}
}

func TestHostToolCardCarriesRealNameAndArgs(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	em.add(hostCall("call_9", "bash", map[string]any{"command": "echo hi"}))
	em.finish("toolUse", nil)

	var tc map[string]any
	for _, blk := range em.p.msg["content"].([]any) {
		m, _ := blk.(map[string]any)
		if firstString(m, "type") == "toolCall" {
			tc = m
		}
	}
	if tc == nil {
		t.Fatal("no toolCall block emitted")
	}
	if got := firstString(tc, "name"); got != "bash" {
		t.Fatalf("tool card name = %q, want the host tool name bash", got)
	}
	if got := firstString(tc, "id"); got != "call_9" {
		t.Fatalf("tool card id = %q, want the Cursor call id so the host result matches", got)
	}
	args, ok := tc["arguments"].(map[string]any)
	if !ok || args["command"] != "echo hi" {
		t.Fatalf("tool card arguments = %#v, want the parsed argument object", tc["arguments"])
	}
	if em.p.msg["stopReason"] != "toolUse" {
		t.Fatalf("stopReason = %v, want toolUse so the host runs the tool", em.p.msg["stopReason"])
	}
}

func TestStreamedTextIsNeverDuplicated(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	// The decoder already drops the sdk_message copies of delta text; feeding a
	// second segment must not produce a second copy either.
	for _, ev := range []cursor.RunEvent{thinking("a "), text("hello "), text("world")} {
		em.add(ev)
	}
	em.finish("stop", &cursor.RunResult{Status: "FINISHED"})

	if got := em.text(); got != "hello world" {
		t.Fatalf("text = %q, want exactly the streamed deltas once", got)
	}
	deltas := 0
	for _, ev := range em.events() {
		if ev["type"] == "text_delta" {
			deltas++
		}
	}
	if deltas != 2 {
		t.Fatalf("text_delta events = %d, want 2 (one per delta, no repeats)", deltas)
	}
}

func TestResumedTurnStillDeliversTheFinalAnswer(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	// A resumed durable replay suppresses the already-streamed segment and
	// reports the answer on the result frame.
	em.finish("stop", &cursor.RunResult{Status: "FINISHED", Text: "It printed hello-go."})

	if got := em.text(); got != "It printed hello-go." {
		t.Fatalf("text = %q, want the run result text", got)
	}
}

func TestStatusFailureBecomesAReadableError(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	msg, failed := em.statusFailure(cursor.RunEvent{Event: cursor.Event{
		Kind:   cursor.EventStatus,
		Status: &cursor.StatusEvent{Status: "ERROR", Message: "Invalid user API key"},
	}})
	if !failed || msg != "Invalid user API key" {
		t.Fatalf("statusFailure = (%q, %v), want the Cursor message surfaced", msg, failed)
	}
}

func TestNoProviderEventCarriesRawBridgeJSON(t *testing.T) {
	// Feed the real captured run through the decoder and the emitter, then assert
	// nothing that reaches the host looks like a protocol payload.
	frames := loadFrames(t)
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	for _, m := range frames {
		for _, ev := range cursor.Decode(m, true) {
			if ev.Kind == cursor.EventToolCallStarted && ev.ToolCall != nil && !strings.HasPrefix(ev.ToolCall.ID, "call_") {
				continue // Cursor-side tools in the captured run
			}
			if em.add(cursor.RunEvent{Event: ev}) {
				break
			}
		}
	}
	em.finish("stop", &cursor.RunResult{Status: "FINISHED"})

	raw, err := json.Marshal(em.events())
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, banned := range []string{
		`"providerIdentifier"`, `"custom-user-tools"`, `"interactionUpdate"`,
		`"tool-call-started"`, `"RUN_LIFECYCLE_STATUS`, `"modelCallId"`, `"agent_id"`,
	} {
		if strings.Contains(body, banned) {
			t.Fatalf("a provider event carries raw bridge payload %q", banned)
		}
	}
	var contentTypes []string
	for _, blk := range em.p.msg["content"].([]any) {
		m, _ := blk.(map[string]any)
		contentTypes = append(contentTypes, firstString(m, "type"))
	}
	for _, ct := range contentTypes {
		if ct != "text" && ct != "thinking" && ct != "toolCall" {
			t.Fatalf("assistant message carries a %q block; only text, thinking and host tool calls are allowed", ct)
		}
	}
}

func thinkingText(t *testing.T, msg map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, blk := range msg["content"].([]any) {
		m, _ := blk.(map[string]any)
		if firstString(m, "type") == "thinking" {
			b.WriteString(firstString(m, "thinking"))
		}
	}
	return b.String()
}

// loadFrames reads the run captured from a real bridge, so the display rules are
// enforced against protocol traffic rather than hand-written fixtures.
func loadFrames(t *testing.T) []*sdkpb.RunStreamMessage {
	t.Helper()
	path := "../cursor/testdata/send-frames.jsonl"
	if _, err := os.Stat(path); err != nil {
		path = "internal/cursor/testdata/send-frames.jsonl"
		if _, err2 := os.Stat(path); err2 != nil {
			t.Skip("captured frames not present")
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	var out []*sdkpb.RunStreamMessage
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		msg := &sdkpb.RunStreamMessage{}
		if err := opts.Unmarshal([]byte(line), msg); err != nil {
			t.Fatalf("frame does not parse: %v", err)
		}
		out = append(out, msg)
	}
	if len(out) == 0 {
		t.Fatal("no frames loaded")
	}
	return out
}
