package provider

import (
	"encoding/json"
	"strings"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

// turnEmitter is the display contract of this extension, as one object.
//
// It turns Cursor run events into the only pig provider events this extension
// ever emits: assistant text (including marked inline tool annotations),
// thinking, and real host tool calls. Step frames, status chatter and duplicate
// sdk_message copies of text already streamed as deltas remain bookkeeping.
// Keeping the mapping in one pure object makes it testable without a bridge, with rules
// enforceable instead of scattered through the streaming path.
type turnEmitter struct {
	p    *partial
	sink func(map[string]any)

	tools       []map[string]any
	pending     []pendingRef
	hostToolSet map[string]bool
	cursorTools *cursorToolTracker
}

func newTurnEmitter(model map[string]any, entry *ModelEntry) *turnEmitter {
	t := &turnEmitter{p: newPartial(model, entry)}
	if !cursorToolsDisabled() {
		t.cursorTools = &cursorToolTracker{calls: map[string]*cursorToolCall{}}
	}
	return t
}

// emit records the event and forwards it when a sink is attached. Tests read
// events(); the streaming path attaches a sink that pushes into pig's stream.
func (t *turnEmitter) emit(ev map[string]any) {
	t.p.events = append(t.p.events, ev)
	if t.sink != nil {
		t.sink(ev)
	}
}

func (t *turnEmitter) push(ev map[string]any) {
	ev["partial"] = t.p.msg
	t.emit(ev)
}

// events returns every pig event this turn produced, in order.
func (t *turnEmitter) events() []map[string]any { return t.p.events }

// sawContent reports whether anything visible reached the host yet. A failure
// before that point is safe to retry.
func (t *turnEmitter) sawContent() bool {
	return len(t.p.events) > 1 || len(t.tools) > 0
}

// text returns the assistant text the turn accumulated.
func (t *turnEmitter) text() string { return t.p.joinedText() }

// begin starts the assistant message.
func (t *turnEmitter) begin() { t.push(map[string]any{"type": "start"}) }

// add translates one run event. stop=true means the provider turn is over
// because a host tool call was handed to the host.
func (t *turnEmitter) add(ev cursor.RunEvent) (stop bool) {
	switch ev.Kind {
	case cursor.EventThinkingDelta:
		if ev.Text == "" {
			return false
		}
		t.closeText()
		idx := t.p.openThinking()
		if !t.p.thinkStarted {
			t.p.thinkStarted = true
			t.push(map[string]any{"type": "thinking_start", "contentIndex": idx})
		}
		t.p.appendThinking(ev.Text)
		t.push(map[string]any{"type": "thinking_delta", "contentIndex": idx, "delta": ev.Text})
	case cursor.EventTextDelta:
		if ev.Text == "" {
			return false
		}
		t.closeThinking()
		idx := t.p.openText()
		if !t.p.textStarted {
			t.p.textStarted = true
			t.push(map[string]any{"type": "text_start", "contentIndex": idx})
		}
		t.p.appendText(ev.Text)
		t.push(map[string]any{"type": "text_delta", "contentIndex": idx, "delta": ev.Text})
	case cursor.EventToolCallStarted:
		// A Cursor-side call normally reaches this emitter already tagged (see
		// cursor.EventCursorToolStarted) and is displayed as inline text. This
		// check is the backstop: an untagged one must still never become a host card.
		if ev.ToolCall != nil && !t.isHostTool(ev.ToolCall.ToolName) {
			return false
		}
		return t.hostToolCall(ev)
	case cursor.EventCursorToolStarted, cursor.EventCursorToolCompleted:
		call, fresh, completed := t.cursorTools.note(ev)
		if fresh {
			t.cursorToolAnnotation(call, "")
		}
		if completed && call.isError {
			t.cursorToolAnnotation(call, "failed")
		}
	case cursor.EventUsage:
		if ev.Usage != nil {
			t.p.usage = ev.Usage
		}
	case cursor.EventResult:
		if ev.Result != nil && ev.Result.Usage != nil {
			t.p.usage = ev.Result.Usage
		}
	}
	return false
}

// cursorToolAnnotation is display text, never a toolCall. At the start event it
// separates the preceding prose/thinking from whatever the model emits next.
// Successful calls need no second line; failure notes appear when reported.
func (t *turnEmitter) cursorToolAnnotation(call *cursorToolCall, outcome string) {
	if call == nil {
		return
	}
	t.closeThinking()
	t.closeText()
	line := cursorToolLines(cursorToolCard{tool: oneLine(call.tool),
		summary: cursorToolSummary(call.tool, call.args), status: outcome},
		func(_ string, text string) string { return text }, false, 160)[0]
	idx := t.p.openText()
	block := t.p.block(idx).(map[string]any)
	block["textSignature"] = cursorToolTextSignature
	t.p.textStarted = true
	t.push(map[string]any{"type": "text_start", "contentIndex": idx})
	delta := "\n\n" + strings.TrimSpace(line) + "\n\n"
	t.p.appendText(delta)
	t.push(map[string]any{"type": "text_delta", "contentIndex": idx, "delta": delta})
	t.closeText()
}

// hostTools is the set the agent advertised to Cursor; a name outside it is
// Cursor's own tool, which the host cannot run or render.
func (t *turnEmitter) isHostTool(name string) bool {
	if t.hostToolSet == nil {
		return true // unknown set: never suppress a card on a guess
	}
	return t.hostToolSet[name]
}

// hostToolCall surfaces one host-owned tool call as a real pig tool call, so the
// host renders its own card and runs its own hooks and permissions.
func (t *turnEmitter) hostToolCall(ev cursor.RunEvent) bool {
	if ev.ToolCall == nil {
		return false
	}
	id, name, args := ev.ToolCall.DisplayID, ev.ToolCall.ToolName, ev.ToolCall.Args
	if id == "" {
		id = ev.ToolCall.ID
	}
	t.closeThinking()
	t.closeText()
	tc := t.p.openToolCall(id, name, args)
	idx := len(t.p.content) - 1
	argsJSON, _ := json.Marshal(args)
	t.push(map[string]any{"type": "toolcall_start", "contentIndex": idx, "id": id, "toolName": name})
	t.push(map[string]any{"type": "toolcall_delta", "contentIndex": idx, "delta": string(argsJSON)})
	t.push(map[string]any{"type": "toolcall_end", "contentIndex": idx, "toolCall": tc})
	t.tools = append(t.tools, tc)
	t.pending = append(t.pending, pendingRef{ID: id, ToolName: name})
	return true
}

// statusFailure reports a run-level failure the host must see as an error.
func (t *turnEmitter) statusFailure(ev cursor.RunEvent) (string, bool) {
	if ev.Kind != cursor.EventStatus || ev.Status == nil {
		return "", false
	}
	if ev.Status.Status != "ERROR" && ev.Status.Status != "EXPIRED" {
		return "", false
	}
	if ev.Status.Message != "" {
		return ev.Status.Message, true
	}
	return "Cursor reported the run failed", true
}

// finish closes the message. terminal is the run's terminal result when one is
// known; resuming reports whether this turn continued a run that had been parked
// on a host tool call, whose final text arrives on the result frame rather than
// as deltas.
func (t *turnEmitter) finish(reason string, terminal *cursor.RunResult) string {
	if t.cursorTools != nil {
		for _, id := range t.cursorTools.order {
			call := t.cursorTools.calls[id]
			if !call.finished {
				t.cursorToolAnnotation(call, "unfinished")
				call.finished = true
			}
		}
	}
	if len(t.tools) > 0 {
		t.closeThinking()
		t.closeText()
		t.p.setStop("toolUse", t.p.usage)
		t.push(map[string]any{"type": "done", "reason": "toolUse", "message": t.p.msg})
		return "toolUse"
	}
	if terminal != nil && terminal.Status == "CANCELLED" {
		reason = "aborted"
	}
	if terminal != nil && terminal.Text != "" && t.p.hasNoText() {
		// The durable stream reports the run's answer on its result frame instead
		// of replaying deltas, so the text arrives here.
		idx := t.p.openText()
		if !t.p.textStarted {
			t.p.textStarted = true
			t.push(map[string]any{"type": "text_start", "contentIndex": idx})
		}
		t.p.appendText(terminal.Text)
		t.push(map[string]any{"type": "text_delta", "contentIndex": idx, "delta": terminal.Text})
	}
	t.closeThinking()
	t.closeText()
	t.p.setStop(reason, t.p.usage)
	t.push(map[string]any{"type": "done", "reason": reason, "message": t.p.msg})
	return reason
}

func (t *turnEmitter) closeText() {
	if !t.p.textOpen && !t.p.textStarted {
		return
	}
	block, _ := t.p.block(t.p.textIndex).(map[string]any)
	t.push(map[string]any{"type": "text_end", "contentIndex": t.p.textIndex, "content": firstString(block, "text")})
	t.p.textOpen, t.p.textStarted = false, false
}

func (t *turnEmitter) closeThinking() {
	if !t.p.thinkOpen && !t.p.thinkStarted {
		return
	}
	block, _ := t.p.block(t.p.thinkIndex).(map[string]any)
	t.push(map[string]any{"type": "thinking_end", "contentIndex": t.p.thinkIndex, "content": firstString(block, "thinking")})
	t.p.thinkOpen, t.p.thinkStarted = false, false
}
