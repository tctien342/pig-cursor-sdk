package provider

import (
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/MichaelKinsy/PiG/extensions/sdk/tui"
)

const inlineToolMarker = "<!--pig-cursor-sdk:internal-tool:v1-->"

// StyleInternalToolMarkdown is display-only. Only our standalone marked assistant
// blocks are styled; ordinary assistant/user text and native tool cards pass
// through unchanged. The stored text stays Unicode/Markdown, never ANSI.
func StyleInternalToolMarkdown(markdown string, ctx sdk.MarkdownTransformContext, theme sdk.UITheme) string {
	if ctx.MessageType != "assistant" || ctx.AvailableWidth < 4 {
		return markdown
	}
	trimmed := strings.TrimSpace(markdown)
	if !strings.HasPrefix(trimmed, inlineToolMarker+"\n") {
		return markdown
	}
	line := strings.TrimSpace(strings.TrimPrefix(trimmed, inlineToolMarker))
	if !strings.HasPrefix(line, "◇ cursor: ") || strings.ContainsAny(line, "\n\r\x1b") {
		return markdown
	}
	token := "toolSuccessBg"
	if strings.HasSuffix(line, " · failed") {
		token = "toolErrorBg"
	}
	if strings.HasSuffix(line, " · no result") {
		token = "toolPendingBg"
	}
	if _, err := theme.GetBgAnsi(token); err != nil {
		return markdown
	}
	width := ctx.AvailableWidth
	blank := strings.Repeat(" ", width)
	line = tui.TruncateToWidth(" "+line, width, "…", true)
	// Paint padding and content together. Explicit foreground prevents a stale
	// thinking/dim style from leaking into the tool card.
	paint := func(s string) string { return theme.Bg(token, theme.Fg("toolTitle", s)) }
	return paint(blank) + "\n" + paint(line) + "\n" + paint(blank)
}
