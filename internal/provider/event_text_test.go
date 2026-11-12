package provider

import (
	"testing"
	"unicode/utf8"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func TestEmitterRepairsMalformedEventText(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	em.add(thinking("think\xff"))
	em.add(text("reply\xc3"))
	em.finish("stop", nil)
	if got := thinkingText(t, em.p.msg); got != "think�" || !utf8.ValidString(got) {
		t.Fatalf("thinking fallback=%q", got)
	}
	if got := em.text(); got != "reply�" || !utf8.ValidString(got) {
		t.Fatalf("text fallback=%q", got)
	}
	msg, failed := em.statusFailure(cursor.RunEvent{Event: cursor.Event{Kind: cursor.EventStatus, Status: &cursor.StatusEvent{Status: "ERROR", Message: "error\xff"}}})
	if !failed || msg != "error�" {
		t.Fatalf("status fallback=%q, failed=%v", msg, failed)
	}
}

func TestEmitterRepairsMalformedTerminalText(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	terminal := &cursor.RunResult{Text: "terminal\xff", Status: "FINISHED"}
	em.finish("stop", terminal)
	if got := em.text(); got != "terminal�" {
		t.Fatalf("terminal fallback=%q", got)
	}
	if terminal.Text != "terminal\xff" {
		t.Fatal("terminal source was mutated")
	}
}
