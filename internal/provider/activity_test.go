package provider

import (
	"errors"
	"strings"
	"testing"
	"time"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func TestActivityWatchProgressAndToolBudget(t *testing.T) {
	t.Setenv("PI_CURSOR_IDLE_TIMEOUT", "5m")
	t.Setenv("PI_CURSOR_TOOL_TIMEOUT", "30m")
	start := time.Unix(1000, 0)
	w := newActivityWatch(start)
	w.note(text("still working"), start.Add(4*time.Minute))
	if d := w.wait(start.Add(6 * time.Minute)); d != 3*time.Minute {
		t.Fatalf("progress should reset idle budget: %s", d)
	}
	w.note(cursor.RunEvent{Event: cursor.Event{Kind: cursor.EventStatus}}, start.Add(7*time.Minute))
	w.note(text(" \n\t"), start.Add(7*time.Minute))
	if d := w.wait(start.Add(8 * time.Minute)); d != time.Minute {
		t.Fatalf("status chatter extended deadline: %s", d)
	}
	call := internalCall("slow", "sleep", cursor.EventCursorToolStarted)
	w.note(call, start.Add(8*time.Minute))
	w.note(call, start.Add(20*time.Minute)) // duplicate start must not restart tool budget
	w.note(text("progress"), start.Add(35*time.Minute))
	if d := w.wait(start.Add(36 * time.Minute)); d != 2*time.Minute {
		t.Fatalf("tool budget is not absolute: %s", d)
	}
	w.note(internalCall("slow", "sleep", cursor.EventCursorToolCompleted), start.Add(37*time.Minute))
	if d := w.wait(start.Add(38 * time.Minute)); d != 4*time.Minute {
		t.Fatalf("completion should restore idle budget: %s", d)
	}
	if !strings.Contains(w.timeoutMessage(false, errors.New("cancel failed")), "not confirmed") {
		t.Fatal("must not claim release if cancellation failed")
	}
	if !strings.Contains(w.timeoutMessage(true, nil), "PI_CURSOR_TURN_TIMEOUT") {
		t.Fatal("wall cap not distinguished")
	}
}

func TestToolSignatureIncludesSchemaContentsAndDescription(t *testing.T) {
	a := []ToolDef{{Name: "read", Schema: map[string]any{"type": "string", "enum": []string{"a"}}, Description: "first"}}
	b := []ToolDef{{Name: "read", Schema: map[string]any{"type": "string", "enum": []string{"b"}}, Description: "first"}}
	if toolSignature(a) == toolSignature(b) {
		t.Fatal("same-length schema changes were missed")
	}
	b[0] = a[0]
	b[0].Description = "other"
	if toolSignature(a) == toolSignature(b) {
		t.Fatal("description change was missed")
	}
	a = append(a, ToolDef{Name: "bash", Schema: map[string]any{"type": "object"}})
	if toolSignature(a) != toolSignature([]ToolDef{a[1], a[0]}) {
		t.Fatal("order should not recreate the agent")
	}
}

func TestReplayCarriesHostInstructionsAndToolPreference(t *testing.T) {
	c := PiContext{SystemPrompt: "Use the host's file permissions.", Tools: []ToolDef{{Name: "bash"}}}
	prompt := c.ReplayPrompt(2000)
	if !strings.Contains(prompt, c.SystemPrompt) || !strings.Contains(prompt, "Prefer the advertised pig custom tools") {
		t.Fatal("host tool guidance is missing")
	}
	c.Tools = nil
	if strings.Contains(c.ReplayPrompt(2000), "TOOL ROUTING:") {
		t.Fatal("must not instruct no-tools mode to call missing host tools")
	}
}

func TestMixedInternalAndHostToolsKeepNativePath(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.hostToolSet = map[string]bool{"bash": true}
	em.begin()
	em.add(thinking("Use tools."))
	em.add(internalCall("internal", "echo own", cursor.EventCursorToolStarted))
	em.add(internalCall("internal", "echo own", cursor.EventCursorToolCompleted))
	if !em.add(hostCall("host", "bash", map[string]any{"command": "echo native"})) {
		t.Fatal("host call did not yield to pig")
	}
	if got := em.finish("stop", nil); got != "toolUse" {
		t.Fatalf("host stop reason=%s", got)
	}
	if len(em.tools) != 1 || firstString(em.tools[0], "name") != "bash" || len(em.pending) != 1 {
		t.Fatal("native host path was converted/suppressed")
	}
	if len(annotationBlocks(em)) != 1 || strings.Contains(annotationBlocks(em)[0], "echo native") {
		t.Fatal("host call got internal styling")
	}
}

func TestTerminalRecoveryAddsOnlyMissingSuffix(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	em.add(text("Hello "))
	em.add(internalCall("own", "ls", cursor.EventCursorToolStarted))
	em.add(internalCall("own", "ls", cursor.EventCursorToolCompleted))
	em.finish("stop", &cursor.RunResult{Text: "Hello world", Status: "FINISHED"})
	if em.p.modelText() != "Hello world" {
		t.Fatalf("terminal suffix duplicated or lost: %q", em.p.modelText())
	}
}
