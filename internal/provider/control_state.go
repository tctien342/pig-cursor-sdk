package provider

import (
	"sort"
	"strings"
)

func normalizedThinking(level string) string {
	switch level = strings.ToLower(strings.TrimSpace(level)); level {
	case "none", "false":
		return "off"
	case "true":
		return "high"
	case "extra-high":
		return "xhigh"
	default:
		return level
	}
}

// Per-call options win over installed defaults; host context is only a fallback.
func OverrideForOptions(base ModelOverride, options map[string]any) ModelOverride {
	if value := firstString(options, "reasoning", "thinkingLevel"); value != "" {
		base.Thinking = value
	}
	if value, ok := options["fast"].(bool); ok {
		base.Fast = value
		base.HasFast = true
	}
	base.Thinking = normalizedThinking(base.Thinking)
	return base
}
func withContextThinking(override ModelOverride, contextLevel string) ModelOverride {
	if override.Thinking == "" {
		override.Thinking = contextLevel
	}
	override.Thinking = normalizedThinking(override.Thinking)
	return override
}

type ControlReport struct {
	ModelID       string
	ContextWindow int
	Requested     ModelOverride
	Params        []Param
	Warnings      []string
}

func reportControls(entry *ModelEntry, override ModelOverride) ControlReport {
	report := ControlReport{ModelID: entry.ID, ContextWindow: entry.ContextWindow, Requested: override, Params: resolvedParams(entry, override)}
	level := normalizedThinking(override.Thinking)
	supported := entry.ThinkingLevels
	if level != "" && !(level == "off" && !entry.Reasoning) {
		if _, ok := supported[level]; !ok {
			levels := make([]string, 0, len(supported))
			for key := range supported {
				levels = append(levels, key)
			}
			sort.Strings(levels)
			available := strings.Join(levels, ", ")
			if available == "" {
				available = "none"
			}
			report.Warnings = append(report.Warnings, "thinking selection is unsupported; Cursor defaults retained (supported: "+available+")")
		}
	}
	if override.HasFast && !entry.SupportsFast {
		report.Warnings = append(report.Warnings, "fast mode is unsupported; Cursor defaults retained")
	}
	return report
}

func (s *Session) recordControls(report ControlReport) bool {
	s.gate.Lock()
	defer s.gate.Unlock()
	changed := s.controlReport == nil || strings.Join(s.controlReport.Warnings, "\n") != strings.Join(report.Warnings, "\n")
	s.controlReport = &report
	return changed && len(report.Warnings) > 0
}
func (s *Session) controls() *ControlReport {
	s.gate.Lock()
	defer s.gate.Unlock()
	if s.controlReport == nil {
		return nil
	}
	copy := *s.controlReport
	copy.Params = append([]Param(nil), copy.Params...)
	copy.Warnings = append([]string(nil), copy.Warnings...)
	return &copy
}

// FastSelection changes model identity as well as wire parameters, keeping pig's
// registered context-window metadata consistent with the selected fast variant.
func (c *Catalog) FastSelection(id, mode string) (*ModelEntry, string) {
	current := c.Lookup(id)
	if current == nil {
		return nil, "select a Cursor model first"
	}
	if !current.SupportsFast {
		return nil, "this model does not advertise fast mode"
	}
	base := strings.TrimSuffix(strings.TrimSuffix(current.ID, ":fast"), ":slow")
	switch mode {
	case "on", "fast":
		base += ":fast"
	case "off", "slow":
		base += ":slow"
	case "default":
	default:
		return nil, "use /cursor-fast on, off, or default"
	}
	next := c.Lookup(base)
	if next == nil || next.ID != base {
		return nil, "requested fast selection is unavailable"
	}
	return next, ""
}
