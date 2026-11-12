package provider

import (
	"reflect"
	"testing"
)

func TestExplicitControlsWinOverContextAndAreRequestLocal(t *testing.T) {
	defaults := ModelOverride{Thinking: "off"}
	first := OverrideForOptions(defaults, map[string]any{"reasoning": "extra-high", "fast": true})
	first = withContextThinking(first, "low")
	if first.Thinking != "xhigh" || !first.HasFast || !first.Fast {
		t.Fatalf("explicit options were overwritten: %+v", first)
	}
	second := withContextThinking(OverrideForOptions(ModelOverride{}, nil), "low")
	if second.Thinking != "low" || second.HasFast || defaults.Thinking != "off" {
		t.Fatal("overrides leaked between requests")
	}
	if got := withContextThinking(OverrideForOptions(ModelOverride{}, map[string]any{"reasoning": "none"}), "max"); got.Thinking != "off" {
		t.Fatal("native none/off mapping failed")
	}
}
func TestAgentSpecUsesResolvedThinkingParameters(t *testing.T) {
	entry := &ModelEntry{BaseID: "test", Params: []Param{{ID: "thinking", Value: "true"}, {ID: "effort", Value: "medium"}}, Reasoning: true, ThinkingLevels: map[string]string{"off": "false", "high": "high"}, ThinkingEdits: map[string][]Param{
		"off": {{ID: "thinking", Value: "false"}}, "high": {{ID: "thinking", Value: "true"}, {ID: "effort", Value: "high"}},
	}}
	before := append([]Param(nil), entry.Params...)
	spec := AgentSpecFor(entry, Config{}, "", nil, ModelOverride{Thinking: "off"})
	params := []Param{}
	for _, p := range spec.ModelParams {
		params = append(params, Param{ID: p.ID, Value: p.Value})
	}
	if paramValue(params, "thinking") != "false" || paramValue(params, "effort") != "medium" {
		t.Fatalf("off sent to wrong wire parameter: %+v", params)
	}
	if !reflect.DeepEqual(before, entry.Params) {
		t.Fatal("model defaults were mutated")
	}
	if overrideSignature(entry, ModelOverride{Thinking: "off"}) == overrideSignature(entry, ModelOverride{Thinking: "high"}) {
		t.Fatal("different effective controls share a signature")
	}
	report := reportControls(entry, ModelOverride{Thinking: "unsupported", HasFast: true, Fast: true})
	if len(report.Warnings) != 2 {
		t.Fatalf("unsupported selections were silently accepted: %+v", report)
	}
}
func TestFastSelectionKeepsRegisteredMetadata(t *testing.T) {
	catalog := &Catalog{byID: map[string]*ModelEntry{}}
	for _, entry := range []ModelEntry{
		{ID: "model@500k", SupportsFast: true, ContextWindow: 256000},
		{ID: "model@500k:fast", SupportsFast: true, ContextWindow: 256000},
		{ID: "model@500k:slow", SupportsFast: true, ContextWindow: 500000},
	} {
		e := entry
		catalog.byID[e.ID] = &e
	}
	selected, err := catalog.FastSelection("model@500k:fast", "off")
	if err != "" || selected.ID != "model@500k:slow" || selected.ContextWindow != 500000 {
		t.Fatalf("fast toggle lost actual model metadata: %+v %s", selected, err)
	}
	selected, err = catalog.FastSelection(selected.ID, "default")
	if err != "" || selected.ID != "model@500k" {
		t.Fatal("could not restore native default mode")
	}
	if _, err = catalog.FastSelection("missing", "on"); err == "" {
		t.Fatal("invalid model silently selected")
	}
}
