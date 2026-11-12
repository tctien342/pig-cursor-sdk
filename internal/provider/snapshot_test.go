package provider

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

func TestEmitterSnapshotsPartialAndToolArguments(t *testing.T) {
	em := newTurnEmitter(modelMap(), entryFor())
	em.begin()
	em.add(text("first"))
	early := em.events()[0]["partial"].(map[string]any)
	if len(early["content"].([]any)) != 0 {
		t.Fatal("start event was retroactively changed")
	}
	args := map[string]any{"nested": map[string]any{"items": []any{"original"}}}
	em.add(hostCall("native", "bash", args))
	args["nested"].(map[string]any)["items"].([]any)[0] = "changed"
	em.finish("stop", nil)
	for _, ev := range em.events() {
		if ev["type"] == "toolcall_end" {
			call := ev["toolCall"].(map[string]any)
			got := call["arguments"].(map[string]any)["nested"].(map[string]any)["items"].([]any)[0]
			if got != "original" {
				t.Fatalf("nested tool args were shared: %v", got)
			}
			if _, ok := ev["contentIndex"].(int); !ok {
				t.Fatal("snapshot coerced content index")
			}
		}
	}
	if firstString(early, "stopReason") != "" {
		t.Fatal("finish mutated old partial stop state")
	}
}

func TestProviderSDKHandoffIsRaceFree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := sdk.CreateAssistantMessageEventStream()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range stream.Events(ctx) {
			if _, err := json.Marshal(ev); err != nil {
				t.Error(err)
			}
		}
	}()
	em := newTurnEmitter(modelMap(), entryFor())
	em.sink = func(ev map[string]any) { stream.Push(ev) }
	em.begin()
	for i := 0; i < 300; i++ {
		em.add(text("piece "))
	}
	em.add(hostCall("host", "bash", map[string]any{"command": "echo native"}))
	em.finish("toolUse", nil)
	stream.End(snapshotMap(em.p.msg))
	<-done
	if ctx.Err() != nil {
		t.Fatal("SDK handoff did not finish")
	}
	if len(em.events()) != 0 {
		t.Fatal("live emitter retained full-message event history")
	}
}
