package provider

import (
	"reflect"
	"strings"
	"testing"
)

func TestCursorTurnPromptPrioritizesPigOnFreshAndAppendedTurns(t *testing.T) {
	request := "  Inspect the files, then run the tests. 漢🙂\n"
	tools := []ToolDef{{Name: "bash", Schema: map[string]any{"type": "object"}}}
	c := PiContext{SystemPrompt: "Respect project permissions.", Tools: tools, Messages: []Message{
		{Role: "user", Blocks: []Block{{Type: "text", Text: "Previous request."}}},
	}}
	before := fingerprints(c)
	for _, replay := range []bool{true, false} {
		prompt := cursorTurnPrompt(c, request, tools, replay)
		if strings.Count(prompt, "TOOL ROUTING:") != 1 {
			t.Fatalf("replay=%v: guidance missing or duplicated", replay)
		}
		for _, rule := range []string{"Prefer the advertised pig custom tools", "exact registered tool name", "no suitable pig tool", "user explicitly requests", "Never bypass a permission denial", "while its callback is pending"} {
			if !strings.Contains(prompt, rule) {
				t.Errorf("replay=%v: missing rule %q", replay, rule)
			}
		}
		if !strings.HasSuffix(prompt, request) {
			t.Fatalf("replay=%v: raw request changed", replay)
		}
		if replay && !strings.Contains(prompt, c.SystemPrompt) {
			t.Fatal("lost session instructions on replay")
		}
	}
	if !reflect.DeepEqual(before, fingerprints(c)) || c.Tools[0].Name != "bash" || c.Messages[0].Blocks[0].Text != "Previous request." {
		t.Fatal("prompt tuning mutated host context or continuity fingerprints")
	}
}

func TestCursorTurnPromptUsesEffectiveToolsAndNoToolsFallback(t *testing.T) {
	c := PiContext{Tools: []ToolDef{{Name: "bash"}}}
	// The original context may have tools even when DisableHostTools filtered them.
	for _, replay := range []bool{true, false} {
		prompt := cursorTurnPrompt(c, "actual user request", nil, replay)
		if strings.Contains(prompt, "TOOL ROUTING:") {
			t.Fatalf("replay=%v: instructed agent to call disabled tools", replay)
		}
	}
	if len(c.Tools) != 1 {
		t.Fatal("effective-tools copy mutated original context")
	}
	for _, request := range []string{"", " \n", "Use your own Cursor tool here."} {
		if got := cursorTurnPrompt(c, request, nil, false); got != request {
			t.Fatalf("no-tools fallback changed request: %q", got)
		}
	}
	if got := cursorTurnPrompt(c, "", c.Tools, false); got != "" {
		t.Fatal("guidance created a phantom user request")
	}
	// No hardcoded aliases or tool-name guesses for a custom-only loadout.
	prompt := cursorTurnPrompt(c, "Inspect the resource.", []ToolDef{{Name: "company.lookup"}}, false)
	if strings.Contains(prompt, "call bash") || strings.Contains(prompt, "call read") {
		t.Fatal("invented a tool absent from the loadout")
	}
}
