package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
)

// Catalog defaults, kept identical to the rules the working extension uses so
// model selection behaves the same after the rewrite.
const (
	fallbackContextWindow = 128000
	defaultMaxTokens      = 16384
)

// ModelEntry is one selectable pig model derived from a Cursor catalog entry.
type ModelEntry struct {
	// ID is the pig model id, e.g. "claude-fable-5-2@1m:fast".
	ID string
	// BaseID is the Cursor model id, e.g. "claude-fable-5-2".
	BaseID string
	Name   string
	// Params are the model parameter selections sent to Cursor.
	Params        []Param
	ContextWindow int
	MaxTokens     int
	Reasoning     bool
	// ThinkingLevels maps a pig thinking level to the Cursor parameter value.
	ThinkingLevels  map[string]string
	ThinkingParamID string
	// ThinkingEdits contains parameter-aware edits for each supported host level.
	ThinkingEdits map[string][]Param
	SupportsFast  bool
	DefaultFast   bool
}

// Param is one model parameter selection.
type Param struct{ ID, Value string }

// Catalog is the derived model table, indexed by pig model id.
type Catalog struct {
	Entries []ModelEntry
	byID    map[string]*ModelEntry
}

// Lookup finds a pig model id such as "gpt-5.5@1m:fast" or an alias.
func (c *Catalog) Lookup(id string) *ModelEntry {
	if c == nil {
		return nil
	}
	if e, ok := c.byID[id]; ok {
		return e
	}
	// pig may pass the bare selection id; tolerate an unknown suffix.
	if e, ok := c.byID[stripFast(id)]; ok {
		return e
	}
	return nil
}

// Size reports the entry count.
func (c *Catalog) Size() int { return len(c.Entries) }

// PigModels renders the catalog as the model maps pig's provider registry wants.
func (c *Catalog) PigModels() []map[string]any {
	out := make([]map[string]any, 0, len(c.Entries))
	for _, e := range c.Entries {
		m := map[string]any{
			"id":            e.ID,
			"name":          e.Name,
			"reasoning":     e.Reasoning,
			"input":         []string{"text", "image"},
			"contextWindow": e.ContextWindow,
			"maxTokens":     e.MaxTokens,
			"cost":          map[string]any{"input": 0.0, "output": 0.0, "cacheRead": 0.0, "cacheWrite": 0.0},
		}
		if len(e.ThinkingLevels) > 0 {
			m["thinkingLevelMap"] = e.ThinkingLevels
		}
		out = append(out, m)
	}
	return out
}

// BuildCatalog derives pig model entries from Cursor's catalog. Every `context`
// value becomes a `@<context>` selection and every model with a `fast`
// parameter gets `:fast` / `:slow` selections, which is how the extension has
// always exposed Cursor's controls.
func BuildCatalog(items []*sdkpb.SdkModel, contextWindows map[string]int) *Catalog {
	sorted := append([]*sdkpb.SdkModel(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetId() < sorted[j].GetId() })

	c := &Catalog{byID: map[string]*ModelEntry{}}
	ids := make([]string, 0, len(sorted))
	for _, m := range sorted {
		ids = append(ids, m.GetId())
	}

	for _, m := range sorted {
		base := m.GetId()
		contexts := []string{""}
		if p := parameter(m, "context"); p != nil {
			var cs []string
			for _, v := range p.GetValues() {
				cs = append(cs, v.GetValue())
			}
			if len(cs) > 0 {
				contexts = cs
			}
		}
		fasts := []string{""}
		if p := parameter(m, "fast"); p != nil {
			fasts = []string{"", "true", "false"}
		}
		defaultFast := paramValue(defaultParams(m), "fast")

		selectionIDs := []string{m.GetId()}
		_ = ids
		for _, sel := range selectionIDs {
			for _, ctxv := range contexts {
				for _, fast := range fasts {
					entry := &ModelEntry{
						BaseID:        base,
						Params:        defaultParams(m),
						SupportsFast:  parameter(m, "fast") != nil,
						DefaultFast:   strings.EqualFold(defaultFast, "true"),
						ContextWindow: fallbackContextWindow,
						MaxTokens:     defaultMaxTokens,
					}
					// fast == "" keeps the catalog default; true/false override it.
					if fast != "" {
						entry.Params = replaceParam(entry.Params, "fast", fast)
					}
					if ctxv != "" {
						entry.Params = replaceParam(entry.Params, "context", ctxv)
					}
					entry.ID = encodeID(sel, ctxv, fast)
					if _, dup := c.byID[entry.ID]; dup {
						continue
					}
					entry.Name = displayName(m, sel, ctxv, fast)
					if levels, pid := thinkingLevels(m); len(levels) > 0 {
						entry.ThinkingLevels = levels
						entry.ThinkingParamID = pid
						_, _, entry.ThinkingEdits = thinkingControls(m)
						entry.Reasoning = true
					}
					entry.ContextWindow = contextWindowFor(contextWindows, entry, ctxv, base, sel, fast, defaultFast)
					c.Entries = append(c.Entries, *entry)
					c.byID[entry.ID] = &c.Entries[len(c.Entries)-1]
				}
			}
		}
	}
	// Reindex after appending so lookups point into the final Entries slice.
	for i := range c.Entries {
		c.byID[c.Entries[i].ID] = &c.Entries[i]
	}
	// Bare native IDs are lookup-only aliases, not additional menu entries.
	for _, m := range sorted {
		ctxv := paramValue(defaultParams(m), "context")
		for _, fast := range []string{"", "true", "false"} {
			alias := encodeID(m.GetId(), "", fast)
			if c.byID[alias] == nil {
				if entry := c.byID[encodeID(m.GetId(), ctxv, fast)]; entry != nil {
					c.byID[alias] = entry
				}
			}
		}
	}
	return c
}

// contextWindowFor prefers a learned/cached window, then the advertised context
// parameter, then the model's own cached window, then the fallback.
func contextWindowFor(cache map[string]int, e *ModelEntry, ctxv, base, sel, fast, defaultFast string) int {
	keys := []string{e.ID}
	// A fast override that equals the catalog default shares the plain window.
	defaultEquivalent := (fast == "true" && strings.EqualFold(defaultFast, "true")) ||
		(fast == "false" && strings.EqualFold(defaultFast, "false"))
	if defaultEquivalent {
		keys = append(keys, encodeID(sel, ctxv, ""))
	}
	for _, k := range keys {
		if v, ok := cache[k]; ok && v > 0 {
			return v
		}
	}
	if ctxv != "" {
		if n, ok := parseContextAmount(ctxv); ok {
			return n
		}
	}
	// Broad measurements must not override an explicit advertised context.
	keys = []string{"default"}
	if fast == "" || defaultEquivalent {
		keys = append([]string{base}, keys...)
	}
	for _, k := range keys {
		if v := cache[k]; v > 0 {
			return v
		}
	}
	return fallbackContextWindow
}

var contextAmountRe = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)([km])$`)

// parseContextAmount turns Cursor's "272k" / "1m" into token counts.
func parseContextAmount(v string) (int, bool) {
	m := contextAmountRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, false
	}
	amount, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	if m[2] == "m" || m[2] == "M" {
		amount *= 1_000_000
	} else {
		amount *= 1_000
	}
	return int(amount + 0.5), true
}

func parameter(m *sdkpb.SdkModel, id string) *sdkpb.ModelParameterDefinition {
	for _, p := range m.GetParameters() {
		if p.GetId() == id {
			return p
		}
	}
	return nil
}

func hasBooleanValues(p *sdkpb.ModelParameterDefinition) bool {
	for _, v := range p.GetValues() {
		switch v.GetValue() {
		case "true", "false":
		default:
			return false
		}
	}
	return len(p.GetValues()) > 0
}

func defaultParams(m *sdkpb.SdkModel) []Param {
	var out []Param
	for _, v := range m.GetVariants() {
		if !v.GetIsDefault() {
			continue
		}
		for _, p := range v.GetParams() {
			out = append(out, Param{ID: p.GetId(), Value: p.GetValue()})
		}
		return out
	}
	if len(m.GetVariants()) > 0 {
		for _, p := range m.GetVariants()[0].GetParams() {
			out = append(out, Param{ID: p.GetId(), Value: p.GetValue()})
		}
	}
	return out
}

func paramValue(params []Param, id string) string {
	for _, p := range params {
		if p.ID == id {
			return p.Value
		}
	}
	return ""
}

func replaceParam(params []Param, id, value string) []Param {
	out := make([]Param, 0, len(params)+1)
	replaced := false
	for _, p := range params {
		if p.ID == id {
			out = append(out, Param{ID: id, Value: value})
			replaced = true
			continue
		}
		out = append(out, p)
	}
	if !replaced {
		out = append(out, Param{ID: id, Value: value})
	}
	return out
}

// thinkingLevels maps pig's thinking levels onto the model's reasoning
// parameter, which differs per model family (reasoning, effort, thinking,
// reasoning_effort).
func thinkingLevels(m *sdkpb.SdkModel) (map[string]string, string) {
	levels, id, _ := thinkingControls(m)
	return levels, id
}

// encodeID builds the pig model id: base[@context][:fast|:slow].
func encodeID(base, context, fast string) string {
	id := base
	if context != "" {
		id += "@" + context
	}
	switch fast {
	case "true":
		id += ":fast"
	case "false":
		id += ":slow"
	}
	return id
}

func stripFast(id string) string {
	if i := strings.LastIndex(id, ":"); i > 0 && strings.LastIndex(i2s(id[:i]), "@") <= strings.LastIndex(id[:i], ":") {
		return id[:i]
	}
	if i := strings.LastIndex(id, ":"); i > 0 {
		return id[:i]
	}
	return id
}

func i2s(s string) string { return s }

func displayName(m *sdkpb.SdkModel, sel, context, fast string) string {
	name := m.GetDisplayName()
	if name == "" {
		name = m.GetId()
	}
	var quals []string
	if sel != m.GetId() {
		quals = append(quals, sel)
	}
	switch fast {
	case "true":
		quals = append(quals, "fast")
	case "false":
		quals = append(quals, "slow")
	}
	if len(quals) > 0 {
		name = fmt.Sprintf("%s (%s)", name, strings.Join(quals, ", "))
	}
	if context != "" {
		name = fmt.Sprintf("%s @ %s", name, context)
	}
	return name
}

// LoadContextWindows reads the context-window cache written by Cursor tooling
// (best effort; a missing file is normal on a fresh machine).
func LoadContextWindows(configHome string) map[string]int {
	out := map[string]int{}
	for _, p := range []string{
		filepath.Join(configHome, "agent", "cursor-sdk-context-windows.json"),
		filepath.Join(configHome, "agent", "cursor-sdk-go-context-windows.json"),
	} {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var doc struct {
			ContextWindows map[string]int `json:"contextWindows"`
		}
		if err := jsonUnmarshal(raw, &doc); err != nil {
			continue
		}
		for k, v := range doc.ContextWindows {
			if v > 0 {
				out[k] = v
			}
		}
	}
	return out
}
