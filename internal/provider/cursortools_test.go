package provider

import (
	"strings"
	"testing"
	"time"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func colorIdentity(_ string, text string) string { return text }

func cardOf(tool, summary, status string, seconds int) cursorToolCard {
	return cursorToolCard{tool: tool, summary: summary, status: status, seconds: seconds, detail: "command: " + summary}
}

// The whole point of the line is that a reader can see what Cursor ran without
// expanding anything, so the summary is the argument that says it, not the first
// key Cursor happened to serialise.
func TestCursorToolSummaryNamesWhatTheCallDid(t *testing.T) {
	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"shell", map[string]any{"timeout": 30000.0, "command": "ls -1"}, "ls -1"},
		{"read", map[string]any{"path": "src/main.go"}, "src/main.go"},
		{"grep", map[string]any{"pattern": "deferPlanWork", "path": "internal"}, "deferPlanWork"},
		{"edit", map[string]any{"file_path": "/a/b.go", "diff": "@@ huge patch @@"}, "/a/b.go"},
		{"web_search", map[string]any{"queries": []any{"one", "two"}}, `["one","two"]`},
		// A name nobody has seen still has to say something.
		{"frobnicate", map[string]any{"widget_id": 42}, "42"},
	} {
		got := cursorToolSummary(c.name, c.args)
		if c.want != "" && got != c.want {
			t.Errorf("%s: summary = %q, want %q", c.name, got, c.want)
		}
		if got == "" {
			t.Errorf("%s: summary is empty; every call must say something", c.name)
		}
		if strings.ContainsAny(got, "\n") {
			t.Errorf("%s: summary contains a newline: %q", c.name, got)
		}
		if len([]rune(got)) > 400 {
			t.Errorf("%s: summary is unbounded: %d runes", c.name, len([]rune(got)))
		}
	}
}

// A collapsed line that wraps, or that pushes its outcome off the right edge, is
// worse than no line: the transcript becomes unreadable at exactly the moment the
// outcome matters. So the middle truncates and the ends never do.
func TestCursorToolLineStaysOneLineAndKeepsItsOutcome(t *testing.T) {
	long := cardOf("shell", "curl -s https://example.com/api/v1/very/long/path?query="+strings.Repeat("x", 300), "failed", 41)
	for _, width := range []int{20, 40, 80, 110} {
		lines := cursorToolLines(long, colorIdentity, false, width)
		if len(lines) != 1 {
			t.Fatalf("width %d: %d collapsed lines, want 1", width, len(lines))
		}
		if n := len([]rune(lines[0])); n > width {
			t.Fatalf("width %d: line is %d cells: %q", width, n, lines[0])
		}
		if !strings.Contains(lines[0], "failed") {
			t.Fatalf("width %d: outcome missing from %q", width, lines[0])
		}
		if !strings.Contains(lines[0], "cursor") {
			t.Fatalf("width %d: who is missing from %q", width, lines[0])
		}
		if width >= 40 && !strings.Contains(lines[0], "shell") {
			t.Fatalf("width %d: which tool is missing from %q", width, lines[0])
		}
	}
}

func TestCursorToolLineReadsAsAToolCallNotAsNoise(t *testing.T) {
	lines := cursorToolLines(cardOf("shell", "ls -1", "completed", 26), colorIdentity, false, 110)
	got := lines[0]
	for _, want := range []string{"◇ cursor: shell", "\u00b7 ls -1", "2.6s"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "its own tool") {
		t.Errorf("line %q still talks about tools the host cannot see", got)
	}
}

// An unfinished call must look unfinished, never like a success: a line that says
// "cursor: shell · ls -1" with no status is read as "it ran fine".
func TestCursorToolLineNeverClaimsWhatDidNotHappen(t *testing.T) {
	got := cursorToolLines(cardOf("shell", "ls -1", "unfinished", 0), colorIdentity, false, 110)[0]
	if !strings.Contains(got, "no result") {
		t.Errorf("unfinished line = %q, want it to say no result", got)
	}
	if strings.Contains(got, "completed") {
		t.Errorf("unfinished line = %q, must not claim completion", got)
	}
}

func TestCursorToolExpandedAddsArguments(t *testing.T) {
	card := cardOf("shell", "ls -1", "completed", 5)
	card.detail = "command: ls -1\ntimeout: 30000"
	collapsed := cursorToolLines(card, colorIdentity, false, 110)
	expanded := cursorToolLines(card, colorIdentity, true, 110)
	if len(expanded) <= len(collapsed) {
		t.Fatalf("expanded view (%v) adds nothing over the collapsed one (%v)", expanded, collapsed)
	}
	for _, want := range []string{"timeout: 30000", "command: ls -1"} {
		found := false
		for _, l := range expanded {
			if strings.Contains(l, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expanded view is missing %q: %v", want, expanded)
		}
	}
}

// A tool line carries its outcome's background, filled to the full width like a
// host tool card.
func TestPaintCursorToolLineFillsTheWidth(t *testing.T) {
	paint := func(text string) string { return "[" + text + "]" }
	got := paintCursorToolLine("ab", 10, paint)
	if got != "[ab        ]" {
		t.Fatalf("line should be padded to width and painted, got %q", got)
	}
	if got := paintCursorToolLine("exactly-1234", 12, paint); got != "[exactly-1234]" {
		t.Fatalf("exact width = %q", got)
	}
	if got := paintCursorToolLine("way-too-long-for-this-terminal", 10, paint); got != "[way-too-long-for-this-terminal]" {
		t.Fatalf("overlong line = %q", got)
	}
	if got := paintCursorToolLine("x", 10, nil); got != "x" {
		t.Fatalf("no painter should leave the line alone, got %q", got)
	}
	// The card's empty rows carry the colour too, so the band is what has height.
	if got := paintCursorToolLine("", 6, paint); got != "[      ]" {
		t.Fatalf("empty row should be painted to the full width, got %q", got)
	}
}

// The card is drawn as a band: painted blank row, line, painted blank row — the
// padding is inside the colour, not outside it.
func TestCursorToolCardPadsInsideTheBackground(t *testing.T) {
	lines := withCardPadding(cursorToolLines(cursorToolCard{tool: "shell", status: "completed"}, colorIdentity, false, 40))
	if len(lines) != 3 || lines[0] != "" || lines[2] != "" {
		t.Fatalf("want blank, line, blank before painting, got %q", lines)
	}
	painted := make([]string, 0, len(lines))
	for _, line := range lines {
		painted = append(painted, paintCursorToolLine(line, 40, func(text string) string { return "<" + text + ">" }))
	}
	for i, line := range painted {
		if !strings.HasPrefix(line, "<") || !strings.HasSuffix(line, ">") {
			t.Fatalf("row %d is outside the colour band: %q", i, line)
		}
	}
}

// A card sits alone: an empty line above and below, so nine calls in a row still
// read as nine tool calls.
func TestCursorToolCardIsPadded(t *testing.T) {
	got := withCardPadding([]string{"◇ cursor: shell · ls -1"})
	if len(got) != 3 || got[0] != "" || got[2] != "" || got[1] == "" {
		t.Fatalf("want blank line, card, blank line, got %q", got)
	}
	if empty := withCardPadding(nil); len(empty) != 0 {
		t.Fatalf("nothing to draw should stay nothing, got %q", empty)
	}
}

// Each outcome takes the colour the host gives a tool card that ended that way.
func TestCursorToolBackgroundMatchesOutcome(t *testing.T) {
	for status, want := range map[string]string{
		"completed":  "toolSuccessBg",
		"failed":     "toolErrorBg",
		"unfinished": "toolPendingBg",
	} {
		if got := (cursorToolCard{status: status}).backgroundToken(); got != want {
			t.Errorf("status %q -> %q, want %q", status, got, want)
		}
	}
}

// A duration is only shown when it was actually measured between two events.
func TestCursorToolDurationOnlyWhenMeasured(t *testing.T) {
	call := &cursorToolCall{id: "x", tool: "shell", startedAt: time.Now().Add(-2600 * time.Millisecond), endedAt: time.Now(), finished: true}
	if got := call.entry().Seconds; got != 26 {
		t.Errorf("Seconds = %d, want 26 (deciseconds)", got)
	}
	fresh := &cursorToolCall{id: "y", tool: "shell", startedAt: time.Now(), finished: false}
	if got := fresh.entry().Seconds; got != 0 {
		t.Errorf("an instant call should show no duration, got %d", got)
	}
}

// Only the newest unfinished call is a live hint. Completed calls never form a
// list near the usage bar; all history stays inline in the chat.
func TestLiveToolSnapshotOnlyRunning(t *testing.T) {
	tr := &cursorToolTracker{calls: map[string]*cursorToolCall{}}
	note := func(id string, kind cursor.EventKind) {
		tr.note(cursor.RunEvent{Event: cursor.Event{Kind: kind, ToolCall: &cursor.ToolCallEvent{
			ID: id, ToolName: "shell", Args: map[string]any{"command": "echo " + id},
		}}})
	}
	note("a", cursor.EventCursorToolStarted)
	note("b", cursor.EventCursorToolStarted)
	if got := tr.snapshot(); len(got) != 1 || got[0].Summary != "echo b" || got[0].Status != "running" {
		t.Fatalf("want only newest running call, got %+v", got)
	}
	note("b", cursor.EventCursorToolCompleted)
	if got := tr.snapshot(); len(got) != 1 || got[0].Summary != "echo a" {
		t.Fatalf("older concurrent call should become the hint: %+v", got)
	}
	note("a", cursor.EventCursorToolCompleted)
	if got := tr.snapshot(); len(got) != 0 {
		t.Fatalf("completed calls must clear the hint: %+v", got)
	}
	lines := RenderLiveToolCards([]LiveToolView{{Tool: "shell", Summary: "echo a", Status: "running"}}, sdk.UITheme{}, 100)
	if len(lines) != 1 || !strings.Contains(lines[0], "running") {
		t.Fatalf("want one running line: %q", lines)
	}
}
