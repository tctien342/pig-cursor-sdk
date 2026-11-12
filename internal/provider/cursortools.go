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

// Cursor's own tools, shown honestly.
//
// A Cursor agent can run tools that were never advertised to it — its built-in
// shell, reader, web search. pig cannot execute or render those, so there is no
// host card for them, and for a long time this extension printed nothing at all,
// which read as the agent stalling. Silence is the wrong default: what Cursor did
// is a fact about the turn. What was wrong before is *where* it was put — a
// provider event of some invented type, or text in the thinking channel, both of
// which land in the model's transcript.
//
// So it goes into a custom session entry with an extension renderer: a real
// transcript line, in the transcript's own grammar, that pig does not send back
// to any model and Cursor never sees. The line is written when the call finishes,
// because a line about a tool that never completed is a lie, and one line per
// call, because Cursor runs these in bursts.

// CursorToolEntryType is the customType of the transcript entry, and the key the
// renderer is registered under.
const CursorToolEntryType = "cursor-tool"

// cursorToolTracker owns one turn's Cursor-side tool calls.
type cursorToolTracker struct {
	// appendEntry is the host call, held as a function so the display rules can be
	// tested without a host.
	appendEntry func(map[string]any) error
	log         *slog.Logger
	calls       map[string]*cursorToolCall
	order       []string
	wrote       map[string]bool
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

// newCursorToolTracker returns nil when there is no transcript to write to: with
// no UI there is nobody to read the line, and a headless host would only pay for
// the entry. Callers may call its methods on a nil tracker.
func newCursorToolTracker(ctx sdk.Context, log *slog.Logger) *cursorToolTracker {
	if cursorToolsDisabled() || !ctx.HasUI() {
		return nil
	}
	return &cursorToolTracker{
		appendEntry: func(data map[string]any) error { return ctx.AppendEntry(CursorToolEntryType, data) },
		log:         log,
		calls:       map[string]*cursorToolCall{},
		wrote:       map[string]bool{},
	}
}

// note records one Cursor-side tool event, and writes the transcript line when
// the call is over. The same call arrives twice on a live stream (once as an
// sdk_message, once as an interaction_update), so every step is keyed by id.
func (t *cursorToolTracker) note(ev cursor.RunEvent) {
	if t == nil || ev.ToolCall == nil {
		return
	}
	id := ev.ToolCall.ID
	if id == "" {
		id = ev.ToolCall.ToolName + "\x00" + fmt.Sprint(len(t.order))
	}
	call, ok := t.calls[id]
	if !ok {
		call = &cursorToolCall{id: id, tool: ev.ToolCall.ToolName, args: ev.ToolCall.Args, startedAt: time.Now()}
		t.calls[id] = call
		t.order = append(t.order, id)
	}
	if ev.Kind == cursor.EventCursorToolCompleted {
		call.finished = true
		call.isError = ev.ToolCall.IsError || ev.ToolCall.Status == "error"
		call.endedAt = time.Now()
		if len(call.args) == 0 {
			call.args = ev.ToolCall.Args
		}
	}
}

// done counts the calls of this turn that have already reported a result.
func (t *cursorToolTracker) done() int {
	if t == nil {
		return 0
	}
	n := 0
	for _, call := range t.calls {
		if call.finished {
			n++
		}
	}
	return n
}

// finish writes the transcript lines for the turn, in call order.
//
// It is called once, after the assistant message has been committed — never while
// the turn is still streaming. pig appends a session entry the moment it is asked
// to, and commits the turn's message at the end of the turn, so an entry written
// mid-turn lands *above* the thinking and text it was interleaved with. A real
// session showed nine shell calls stacked over a message that described them,
// which reads as tools running in the middle of a thought. Below the message they
// read as what they are: the activity of that turn, in order.
//
// Calls that never reported a result are written as "no result" rather than
// dropped: an interrupted turn is exactly the case worth seeing.
func (t *cursorToolTracker) finish() {
	if t == nil {
		return
	}
	for _, id := range t.order {
		call := t.calls[id]
		if !call.finished && call.endedAt.IsZero() {
			call.endedAt = time.Now()
		}
		t.write(call)
	}
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

func (t *cursorToolTracker) write(call *cursorToolCall) {
	if t == nil || t.wrote[call.id] {
		return
	}
	t.wrote[call.id] = true
	entry := call.entry()
	data, err := json.Marshal(entry)
	if err == nil {
		var doc map[string]any
		err = json.Unmarshal(data, &doc)
		if err == nil {
			err = t.appendEntry(doc)
		}
	}
	if err != nil {
		// A missing line is not worth failing a turn over; the log says which one.
		t.log.Debug("cursor tool entry rejected", "tool", call.tool, "err", err.Error())
	}
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

// LiveToolView is one Cursor-side call as it stands right now, for a host UI that
// shows it while the turn streams. Status is "running", "completed" or "failed".
type LiveToolView struct {
	Tool    string
	Summary string
	Status  string
	Seconds int
}

// liveToolLimit is how many cards the live widget shows. A turn can run dozens of
// calls; the widget is a glance, not a log — the transcript keeps the whole list.
const liveToolLimit = 6

// snapshot returns this turn's calls in order, as the live widget should draw them.
// Running calls are last, because that is where the reader's eye needs to be.
func (t *cursorToolTracker) snapshot() []LiveToolView {
	if t == nil {
		return nil
	}
	out := make([]LiveToolView, 0, len(t.order))
	for _, id := range t.order {
		call := t.calls[id]
		view := LiveToolView{Tool: call.tool, Summary: cursorToolSummary(call.tool, call.args)}
		switch {
		case !call.finished:
			view.Status = "running"
		case call.isError:
			view.Status = "failed"
			view.Seconds = call.entry().Seconds
		default:
			view.Status = "completed"
			view.Seconds = call.entry().Seconds
		}
		out = append(out, view)
	}
	if len(out) > liveToolLimit {
		out = out[len(out)-liveToolLimit:]
	}
	return out
}

// RenderLiveToolCards draws those calls for a widget above the editor: one card
// row each, in call order, painted with the same background a host tool card gets
// for that outcome. A finished call turns from the pending colour to the success
// colour where it stands, which is what makes the widget read as tools running
// rather than as a status line.
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
	for _, view := range calls {
		card := cursorToolCard{tool: view.Tool, summary: view.Summary, status: view.Status, seconds: view.Seconds}
		lines := cursorToolLines(card, color, false, width)
		token := card.backgroundToken()
		if ansi, err := theme.GetBgAnsi(token); err == nil && strings.Contains(ansi, "48") {
			for i, line := range lines {
				lines[i] = paintCursorToolLine(line, width, func(text string) string { return theme.Bg(token, text) })
			}
		}
		out = append(out, lines...)
	}
	return out
}

// cursorToolHint is the live footer line for a call still running. It is the only
// sign of Cursor-side work while the turn streams: the transcript lines are
// written when the turn ends, because anything appended mid-turn would land above
// the message it belongs to. So the footer also counts what has already finished,
// which is what tells a reader the turn is making progress and not stuck.
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
