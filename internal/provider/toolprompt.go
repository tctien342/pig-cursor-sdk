package provider

import "strings"

// toolRoutingGuidance is a prompt-level preference, not enforced routing. It is
// present only when pig actually advertises tools. Names/capabilities come from
// their schemas rather than guessed aliases for Cursor's own tools.
func toolRoutingGuidance(tools []ToolDef) string {
	if len(tools) == 0 {
		return ""
	}
	return "TOOL ROUTING: Prefer the advertised pig custom tools for operations they support. " +
		"Choose the exact registered tool name and follow its schema. " +
		"Use pig tools for equivalent operations instead of Cursor's built-in shell, file, edit or search tools; do not switch to a built-in merely for convenience. " +
		"Use a Cursor built-in only if no suitable pig tool is available, or the user explicitly requests Cursor-specific behavior. " +
		"For ordinary pig-tool errors, use an appropriate pig-tool retry or report the failure. Never bypass a permission denial with a Cursor built-in. " +
		"Wait for the real pig callback result; do not fabricate results or repeat the same operation through a built-in while its callback is pending."
}

// cursorTurnPrompt is used only for new user requests, not tool-result
// continuations or stream reattachment. The context is copied so effective tool
// filtering cannot change the raw transcript, tools or continuity fingerprints.
func cursorTurnPrompt(c PiContext, request string, effectiveTools []ToolDef, replay bool) string {
	if replay {
		c.Tools = effectiveTools
		prompt := c.ReplayPrompt(2000)
		if strings.TrimSpace(request) != "" {
			prompt += "\n\nMOST RECENT USER REQUEST (act on this):\n" + request
		}
		return prompt
	}
	guidance := toolRoutingGuidance(effectiveTools)
	if guidance == "" || strings.TrimSpace(request) == "" {
		return request
	}
	return guidance + "\n\nCURRENT USER REQUEST:\n" + request
}
