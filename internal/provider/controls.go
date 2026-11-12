package provider

import (
	"encoding/json"
	"sort"

	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
)

// thinkingControls keeps display mappings separate from actual parameter edits:
// a model can expose both an effort selector and a boolean thinking switch.
func thinkingControls(m *sdkpb.SdkModel) (map[string]string, string, map[string][]Param) {
	var chosen *sdkpb.ModelParameterDefinition
	var id string
	for _, candidate := range []string{"effort", "reasoning", "reasoning_effort", "thinking"} {
		if p := parameter(m, candidate); p != nil {
			chosen, id = p, candidate
			break
		}
	}
	if chosen == nil {
		return nil, "", nil
	}
	levels := map[string]string{}
	edits := map[string][]Param{}
	boolean := parameter(m, "thinking")
	if !hasBooleanValues(boolean) {
		boolean = nil
	}
	has := func(p *sdkpb.ModelParameterDefinition, value string) bool {
		for _, v := range p.GetValues() {
			if v.GetValue() == value {
				return true
			}
		}
		return false
	}
	for _, v := range chosen.GetValues() {
		native := v.GetValue()
		level := native
		switch native {
		case "extra-high":
			level = "xhigh"
		case "none", "off":
			level = "off"
		case "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			continue
		}
		levels[level] = native
		changes := []Param{{ID: id, Value: native}}
		if boolean != nil && id != "thinking" && level != "off" && has(boolean, "true") {
			changes = append(changes, Param{ID: "thinking", Value: "true"})
		}
		edits[level] = changes
	}
	if boolean != nil {
		if has(boolean, "false") {
			levels["off"] = "false"
			edits["off"] = []Param{{ID: "thinking", Value: "false"}}
		}
		if id == "thinking" && has(boolean, "true") {
			levels["high"] = "true"
			edits["high"] = []Param{{ID: "thinking", Value: "true"}}
		}
	}
	if len(levels) == 0 {
		return nil, id, nil
	}
	return levels, id, edits
}

// resolvedParams returns independent selections, preserving native defaults for
// unsupported controls. ThinkingLevels is retained as a legacy-entry fallback.
func resolvedParams(entry *ModelEntry, ov ModelOverride) []Param {
	params := append([]Param(nil), entry.Params...)
	if ov.HasFast && entry.SupportsFast {
		value := "false"
		if ov.Fast {
			value = "true"
		}
		params = replaceParam(params, "fast", value)
	}
	level := normalizedThinking(ov.Thinking)
	if entry.ThinkingEdits != nil {
		for _, p := range entry.ThinkingEdits[level] {
			params = replaceParam(params, p.ID, p.Value)
		}
	} else if entry.ThinkingParamID != "" {
		if value, ok := entry.ThinkingLevels[level]; ok {
			params = replaceParam(params, entry.ThinkingParamID, value)
		}
	}
	return params
}

// canonicalParamSignature ignores ordering without delimiter collisions or
// mutation of the caller's slice. Duplicate selections remain significant.
func canonicalParamSignature(params []Param) string {
	ordered := append([]Param{}, params...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].ID == ordered[j].ID {
			return ordered[i].Value < ordered[j].Value
		}
		return ordered[i].ID < ordered[j].ID
	})
	raw, _ := json.Marshal(ordered)
	return string(raw)
}
