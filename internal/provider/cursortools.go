package provider

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

// Cursor's internal calls are plain inline annotations in new turns. The custom
// entry renderer remains registered only to redraw older sessions.
const CursorToolEntryType = "cursor-tool"

// An exact namespaced marker lets Cursor replay omit generated display blocks
// without deleting model prose that happens to look like a tool annotation.
const cursorToolTextSignature = "pig-cursor-sdk:internal-tool:v1"

type cursorToolTracker struct {
	calls map[string]*cursorToolCall
	order []string
}

type cursorToolCall struct {
	id        string
	tool      string
	args      map[string]any
	startedAt time.Time
	endedAt   time.Time
	finished  bool
	isError   bool
}

// note deduplicates the SDK's two envelopes. Missing IDs use a deterministic
// name/arguments key: matching anonymous envelopes are treated as one call since
// the protocol supplies no way to distinguish them.
func (t *cursorToolTracker) note(ev cursor.RunEvent) (call *cursorToolCall, fresh, completed bool) {
	if t == nil || ev.ToolCall == nil {
		return nil, false, false
	}
	tc := ev.ToolCall
	id := tc.ID
	if id == "" {
		raw, _ := json.Marshal(tc.Args)
		id = tc.ToolName + "\x00" + string(raw)
		// A completion sometimes omits args. Prefer the latest matching open call.
		if ev.Kind == cursor.EventCursorToolCompleted {
			for i := len(t.order) - 1; i >= 0; i-- {
				prev := t.calls[t.order[i]]
				if prev.tool == tc.ToolName && (!prev.finished || len(tc.Args) == 0) {
					id = prev.id
					break
				}
			}
		}
	}
	call = t.calls[id]
	if call == nil {
		call = &cursorToolCall{id: id, tool: tc.ToolName, args: tc.Args}
		if ev.Kind == cursor.EventCursorToolStarted {
			call.startedAt = time.Now()
		}
		t.calls[id] = call
		t.order = append(t.order, id)
		fresh = true
	}
	if ev.Kind == cursor.EventCursorToolCompleted && !call.finished {
		call.finished = true
		call.isError = tc.IsError || tc.Status == "error"
		call.endedAt = time.Now()
		completed = true
	}
	if len(call.args) == 0 {
		call.args = tc.Args
	}
	return
}

// snapshot is a glance at the newest unfinished call, never a history list.
// When it finishes, another concurrent call can become the hint; none means clear.
func (t *cursorToolTracker) snapshot() []LiveToolView {
	if t != nil {
		for i := len(t.order) - 1; i >= 0; i-- {
			call := t.calls[t.order[i]]
			if !call.finished {
				return []LiveToolView{{Tool: call.tool,
					Summary: cursorToolSummary(call.tool, call.args), Status: "running"}}
			}
		}
	}
	return nil
}

// backgroundToken is the theme token whose colour the host gives a tool card with
// the same outcome, so a Cursor-side line is read as a tool call and not as prose.
func (c cursorToolCard) backgroundToken() string {
	switch c.status {
	case "running", "unfinished":
		return "toolPendingBg"
	case "failed":
		return "toolErrorBg"
	default:
		return "toolSuccessBg"
	}
}

// paintCursorToolLine fills the line's background across the full width, the way
// a host tool card does. bg paints one already-coloured run; a line wider than
// the terminal is passed through unpadded rather than truncated, because the
// layout rules above are what decide what a line contains.
func paintCursorToolLine(line string, width int, bg func(string) string) string {
	if bg == nil {
		return line
	}
	if room := width - runeWidth(line); room > 0 {
		line += strings.Repeat(" ", room)
	}
	return bg(line)
}

// cursorToolEntry is the persisted record: enough to re-render the line for any
// width, forever, with no live state. Kept flat and small on purpose — it is
// written to the session file once per Cursor-side tool call.
// Nothing of Cursor's tool *output* is stored: measured on a real account, a
// built-in call reports no result text at all, and the extension already refuses
// to put another agent's tool noise in the transcript.
type cursorToolEntry struct {
	Tool    string `json:"tool"`
	Summary string `json:"summary,omitempty"`
	Status  string `json:"status"` // completed | failed | unfinished
	Seconds int    `json:"seconds,omitempty"`
	Detail  string `json:"detail,omitempty"` // the arguments, for the expanded view
}

func (c *cursorToolCall) entry() cursorToolEntry {
	status := "unfinished"
	switch {
	case c.finished && c.isError:
		status = "failed"
	case c.finished:
		status = "completed"
	}
	secs := 0
	if !c.endedAt.IsZero() && !c.startedAt.IsZero() {
		if d := c.endedAt.Sub(c.startedAt); d >= 100*time.Millisecond {
			secs = int(d.Round(100*time.Millisecond).Seconds() * 10)
		}
	}
	return cursorToolEntry{
		Tool:    c.tool,
		Summary: cursorToolSummary(c.tool, c.args),
		Status:  status,
		Seconds: secs,
		Detail:  cursorToolDetail(c.args),
	}
}

// genericSummaryKeys is the fallback preference, for a tool name this extension
// has not seen.
var genericSummaryKeys = []string{
	"command", "cmd", "path", "file_path", "filePath", "target_directory", "targetDirectory",
	"pattern", "query", "url", "glob", "regex", "prompt", "description", "name",
}

// summaryOrder says which argument a reader wants to see for a kind of call. A
// grep line is useless without the pattern and a shell line useless without the
// command, and both often carry a second argument that means less. Cursor's
// built-in tool names are not a published contract, so this is a preference list
// with a fallback, never a schema.
func summaryOrder(tool string) []string {
	t := strings.ToLower(tool)
	has := func(names ...string) bool {
		for _, n := range names {
			if strings.Contains(t, n) {
				return true
			}
		}
		return false
	}
	switch {
	case has("grep", "glob", "search", "find"):
		return []string{"pattern", "query", "queries", "path", "glob", "regex"}
	case has("shell", "bash", "exec", "terminal", "run", "command"):
		return []string{"command", "cmd", "script"}
	case has("read", "edit", "write", "patch", "apply", "ls", "list", "file"):
		return []string{"path", "file_path", "filePath", "target_directory", "targetDirectory"}
	case has("web", "fetch", "browse", "http"):
		return []string{"url", "query", "queries"}
	}
	return genericSummaryKeys
}

// cursorToolSummary is the one thing a reader needs: the command, path, or
// pattern. Whitespace collapses to a single line, because this is rendered as one.
func cursorToolSummary(tool string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, want := range summaryOrder(tool) {
		for _, k := range keys {
			if !strings.EqualFold(k, want) {
				continue
			}
			if s := scalarText(args[k]); s != "" {
				return s
			}
		}
	}
	for _, k := range keys {
		if s := scalarText(args[k]); s != "" {
			return s
		}
	}
	return ""
}

// cursorToolDetail is the expanded view's argument listing: every scalar
// argument, one per line, so a reader can tell exactly what Cursor ran.
func cursorToolDetail(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		s := scalarText(args[k])
		if s == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s: %s", k, clip(oneLine(s), 400))
	}
	return b.String()
}

// scalarText renders a scalar argument as text; anything structured becomes
// compact JSON, clipped, so a patch body cannot fill the transcript.
func scalarText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return oneLine(x)
	case bool:
		return fmt.Sprint(x)
	case float64:
		return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprint(x), "0"), ".")
	case int, int64:
		return fmt.Sprint(x)
	case json.RawMessage:
		return clip(oneLine(string(x)), 120)
	default:
		if raw, err := json.Marshal(v); err == nil {
			return clip(oneLine(string(raw)), 120)
		}
		return clip(oneLine(fmt.Sprint(v)), 120)
	}
}

func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\t' || unicode.IsControl(r)
	}), " ")
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return ""
	}
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}

// ---- rendering --------------------------------------------------------------

// cursorToolCard is one entry as the renderer sees it.
type cursorToolCard struct {
	tool, summary, status, detail string
	seconds                       int
}

func parseCursorToolEntry(entry map[string]any) (cursorToolCard, bool) {
	raw, ok := entry["data"]
	if !ok {
		return cursorToolCard{}, false
	}
	// The entry arrives as decoded JSON, but a host that replays an entry may hand
	// over the raw object; accept either without a re-marshal on the hot path.
	var e cursorToolEntry
	switch v := raw.(type) {
	case cursorToolEntry:
		e = v
	case map[string]any:
		e.Tool = firstString(v, "tool")
		e.Summary = firstString(v, "summary")
		e.Status = firstString(v, "status")
		e.Detail = firstString(v, "detail")
		if n, ok := v["seconds"].(float64); ok {
			e.Seconds = int(n)
		}
	default:
		blob, err := json.Marshal(v)
		if err != nil || json.Unmarshal(blob, &e) != nil {
			return cursorToolCard{}, false
		}
	}
	if strings.TrimSpace(e.Tool) == "" {
		return cursorToolCard{}, false
	}
	return cursorToolCard{
		tool: e.Tool, summary: e.Summary, status: orNone(e.Status, "completed"),
		detail: e.Detail, seconds: e.Seconds,
	}, true
}

func orNone(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// cursorToolLines draws the card. color is the theme's foreground function, so
// the layout is testable without a host, and an unknown token can never be
// reached: every token used here is one of pig's 51 documented roles.
//
// Collapsed, one line, in the transcript's own voice:
//
//	◇ cursor: shell · ls -1 · 2.6s
//	◇ cursor: shell · cargo test · failed
//
// A finished call needs no "ok": the duration says it ran. A call that failed or
// never reported says so in words, in colour, and that word is the last thing any
// narrow terminal gives up.
//
// Expanded (ctrl+o) adds the arguments one per line — the same "expand to see the
// detail" bargain pig's own tool cards make.
func cursorToolLines(card cursorToolCard, color func(token, text string) string, expanded bool, width int) []string {
	if width < 12 {
		width = 12
	}
	const sep = " · "
	// Secondary text on a painted band has to stay readable: `dim` is a faint,
	// darker grey that all but disappears on toolSuccessBg, which is what made one
	// card look like it had dark text next to white. toolOutput is the token the
	// host uses for a tool card's supporting text, so it matches the band.
	marker := color("toolOutput", "◇")
	label := color("toolTitle", "cursor: "+card.tool)
	statusWord, statusToken := "", "toolOutput"
	switch card.status {
	case "running":
		statusWord = "running"
	case "failed":
		statusWord, statusToken = "failed", "error"
	case "unfinished":
		statusWord, statusToken = "no result", "warning"
	}
	status := color(statusToken, statusWord)
	duration := ""
	if card.seconds > 0 {
		duration = color("toolOutput", fmt.Sprintf("%.1fs", float64(card.seconds)/10))
	}

	// build lays out one shape of the line. The summary is the elastic part: it
	// takes what the fixed parts leave and truncates before it wraps, and is
	// dropped only when what is left of it would not be worth reading.
	build := func(withMarker, withSummary, withDuration bool) string {
		head := label
		if withMarker {
			// The marker belongs to the label, not to the list of facts: "◇ cursor:
			// shell" reads as one thing, "◇ · cursor: shell" reads as two.
			head = marker + " " + label
		}
		prefix := " " + head
		tail := ""
		if status != "" {
			tail += sep + status
		}
		if withDuration && duration != "" {
			tail += sep + duration
		}
		if withSummary && card.summary != "" {
			room := width - runeWidth(prefix) - runeWidth(tail) - runeWidth(sep)
			if room >= 8 {
				return prefix + sep + color("toolOutput", clip(card.summary, room)) + tail
			}
		}
		return prefix + tail
	}

	// Degradation order, cheapest to give up first: the duration, then the marker,
	// then part of the tool name. The summary outlives the duration (it says what
	// the call did) and the outcome outlives everything.
	shapes := []struct{ marker, summary, duration bool }{
		{true, true, true}, {true, true, false}, {false, true, true}, {false, true, false}, {false, false, false},
	}
	line := ""
	for _, shape := range shapes {
		line = build(shape.marker, shape.summary, shape.duration)
		if runeWidth(line) <= width {
			break
		}
	}
	if runeWidth(line) > width {
		// Not even "cursor: <tool> · failed" fits: shorten the tool name rather
		// than lose the outcome, and clip only as the last resort.
		budget := width - 1 - runeWidth(sep) - runeWidth(status)
		if budget < 4 {
			budget = 4
		}
		label = clipVisible(label, budget)
		line = build(false, false, false)
		if runeWidth(line) > width {
			line = clipVisible(line, width)
		}
	}

	lines := []string{line}
	if !expanded {
		return lines
	}
	for _, l := range strings.Split(card.detail, "\n") {
		if l == "" {
			continue
		}
		lines = append(lines, "   "+color("toolOutput", clip(l, width-3)))
	}
	return lines
}

// runeWidth counts the cells text occupies, ignoring the escape sequences that
// only set colour.
func runeWidth(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
}

// clipVisible cuts text to a visible width without cutting into a colour escape,
// and closes the styling it interrupted.
func clipVisible(s string, max int) string {
	if runeWidth(s) <= max || max < 1 {
		return s
	}
	var b strings.Builder
	cells := 0
	for i := 0; i < len(s) && cells < max-1; {
		if s[i] == 0x1b {
			j := i
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j >= len(s) {
				break
			}
			b.WriteString(s[i : j+1])
			i = j + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		i += size
		cells++
	}
	b.WriteString("…")
	if strings.ContainsRune(s, 0x1b) {
		b.WriteString("\x1b[0m") // close only styling that was actually opened
	}
	return b.String()
}

// toolEntryLog is set by the extension factory: the renderer is registered as a
// plain function and has no config of its own, so a question like "does this
// theme even define a background token" cannot be answered without it.
var toolEntryLog *slog.Logger

// SetToolEntryLogger installs that logger.
func SetToolEntryLogger(l *slog.Logger) { toolEntryLog = l }

var themeProbeOnce sync.Once

// probeThemeOnce logs, once per process, which background tokens the running
// theme defines. A missing token silently drops the card background, which is a
// display change nobody would notice until they asked why.
func probeThemeOnce(theme sdk.UITheme) {
	if toolEntryLog == nil {
		return
	}
	themeProbeOnce.Do(func() {
		var found []string
		for name := range theme.Colors() {
			if strings.HasSuffix(name, "Bg") {
				found = append(found, name)
			}
		}
		sort.Strings(found)
		sgr, err := theme.GetBgAnsi("toolSuccessBg")
		toolEntryLog.Debug("cursor tool theme probe", "mode", theme.GetColorMode(), "bgTokens", strings.Join(found, ","), "toolSuccessBg", sgr, "err", err)
	})
}

// RenderCursorToolEntry is the host-facing renderer for a Cursor-side tool line.
// It is registered by the extension factory, so a resumed session draws its
// history in the same shape it was written in.
func RenderCursorToolEntry(ctx sdk.Context, entry map[string]any, opts sdk.EntryRenderOptions, width int) ([]string, error) {
	card, ok := parseCursorToolEntry(entry)
	if !ok {
		return nil, nil
	}
	theme := ctx.UITheme()
	probeThemeOnce(theme)
	color := func(token, text string) string {
		if text == "" {
			return ""
		}
		return theme.Fg(token, text)
	}
	// Padded first, painted second: the empty rows are part of the card, so the
	// band of colour covers them too and the text sits inside it, the way a host
	// tool card does.
	lines := withCardPadding(cursorToolLines(card, color, opts.Expanded, width))
	token := card.backgroundToken()
	if ansi, err := theme.GetBgAnsi(token); err == nil && strings.Contains(ansi, "48") {
		paint := func(text string) string { return theme.Bg(token, text) }
		for i, line := range lines {
			lines[i] = paintCursorToolLine(line, width, paint)
		}
	} else if err == nil && toolEntryLog != nil {
		// The theme resolves this outcome's background to the terminal default, so
		// there is no band to draw. Worth one log line: the difference between "no
		// colour" and "no card" is otherwise invisible.
		toolEntryLog.Debug("cursor tool card has no background in this theme", "token", token, "ansi", ansi)
	}
	return lines, nil
}

// withCardPadding adds the empty rows a card is drawn with, above and below. They
// are painted like the rest of the card, so the colour band is three rows tall and
// the text is inside it rather than floating in a stripe. Between calls the padding
// is also what keeps a burst of Cursor tools from rendering as one dense block.
func withCardPadding(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	out := make([]string, 0, len(lines)+2)
	out = append(out, "")
	out = append(out, lines...)
	return append(out, "")
}

// LiveToolView describes the one running hint above the editor. Finished calls
// remain inline in the message and are excluded from the live snapshot.
type LiveToolView struct {
	Tool    string
	Summary string
	Status  string
	Seconds int
}

// RenderLiveToolCards draws only the current running hint, never call history.
// The transcript already contains each call at its start boundary.
func RenderLiveToolCards(calls []LiveToolView, theme sdk.UITheme, width int) []string {
	if len(calls) == 0 {
		return nil
	}
	color := func(token, text string) string {
		if text == "" {
			return ""
		}
		return theme.Fg(token, text)
	}
	out := make([]string, 0, len(calls))
	for i := len(calls) - 1; i >= 0; i-- {
		view := calls[i]
		if view.Status != "running" {
			continue
		}
		card := cursorToolCard{tool: oneLine(view.Tool), summary: oneLine(view.Summary), status: view.Status, seconds: view.Seconds}
		lines := cursorToolLines(card, color, false, width)
		token := card.backgroundToken()
		if ansi, err := theme.GetBgAnsi(token); err == nil && strings.Contains(ansi, "48") {
			for i, line := range lines {
				lines[i] = paintCursorToolLine(line, width, func(text string) string { return theme.Bg(token, text) })
			}
		}
		out = append(out, lines...)
		break // a caller cannot turn this hint back into a history list
	}
	return out
}

// cursorToolHint mirrors the currently running call in the compact footer.
func cursorToolHint(tool, summary string, done int) string {
	head := "Cursor running " + tool
	if summary != "" {
		head += " · " + clip(oneLine(summary), 40)
	}
	if done > 0 {
		head = fmt.Sprintf("Cursor %d done · %s", done, head)
	}
	return head
}

func cursorToolsDisabled() bool { return os.Getenv("PI_CURSOR_SHOW_TOOLS") == "0" }
