package provider

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func colorIdentity(_ string, text string) string { return text }

// discardLogger keeps the tests' output to the assertions they make.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// errRejected stands in for the host refusing an entry.
var errRejected = errors.New("host refused the entry")

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

// The entry is written once per call and survives the JSON round trip through the
// session file, because a resumed session re-renders it from stored data alone.
func TestTrackerWritesOneEntryPerCall(t *testing.T) {
	var written []map[string]any
	tr := &cursorToolTracker{
		appendEntry: func(data map[string]any) error { written = append(written, data); return nil },
		log:         discardLogger(),
		calls:       map[string]*cursorToolCall{},
		wrote:       map[string]bool{},
	}
	start := func(id, tool string) cursor.RunEvent {
		return cursor.RunEvent{Event: cursor.Event{Kind: cursor.EventCursorToolStarted,
			ToolCall: &cursor.ToolCallEvent{ID: id, ToolName: tool, Args: map[string]any{"command": "ls -1"}, Status: "running"}}}
	}
	done := func(id, tool, status string, isError bool) cursor.RunEvent {
		return cursor.RunEvent{Event: cursor.Event{Kind: cursor.EventCursorToolCompleted,
			ToolCall: &cursor.ToolCallEvent{ID: id, ToolName: tool, Args: map[string]any{"command": "ls -1"}, Status: status, IsError: isError}}}
	}
	// The live stream reports the same call twice through two envelopes.
	tr.note(start("call_a", "shell"))
	tr.note(start("call_a", "shell"))
	tr.note(done("call_a", "shell", "completed", false))
	tr.note(done("call_a", "shell", "completed", false))
	// And this one never came back.
	tr.note(start("call_b", "read"))
	tr.finish()

	if len(written) != 2 {
		t.Fatalf("wrote %d entries, want one per call: %v", len(written), written)
	}
	if got := firstString(written[0], "status"); got != "completed" {
		t.Errorf("first entry status = %q, want completed", got)
	}
	if got := firstString(written[1], "status"); got != "unfinished" {
		t.Errorf("second entry status = %q, want unfinished", got)
	}
	if got := firstString(written[0], "tool"); got != "shell" {
		t.Errorf("entry tool = %q, want shell", got)
	}

	// A failed call is stored as failed and renders as failed.
	written = nil
	tr2 := &cursorToolTracker{
		appendEntry: func(data map[string]any) error { written = append(written, data); return nil },
		log:         discardLogger(),
		calls:       map[string]*cursorToolCall{},
		wrote:       map[string]bool{},
	}
	tr2.note(start("call_c", "shell"))
	tr2.note(done("call_c", "shell", "error", true))
	if len(written) != 1 || firstString(written[0], "status") != "failed" {
		t.Fatalf("failed call stored as %v, want status failed", written)
	}
	card, ok := parseCursorToolEntry(map[string]any{"data": written[0]})
	if !ok {
		t.Fatal("a persisted entry must parse back into a card")
	}
	line := cursorToolLines(card, colorIdentity, false, 110)[0]
	if !strings.Contains(line, "failed") {
		t.Errorf("rendered failed call = %q", line)
	}
}

// A rejected host call must not take the turn down with it.
func TestTrackerSurvivesARejectedEntry(t *testing.T) {
	tr := &cursorToolTracker{
		appendEntry: func(map[string]any) error { return errRejected },
		log:         discardLogger(),
		calls:       map[string]*cursorToolCall{},
		wrote:       map[string]bool{},
	}
	tr.note(cursor.RunEvent{Event: cursor.Event{Kind: cursor.EventCursorToolCompleted,
		ToolCall: &cursor.ToolCallEvent{ID: "x", ToolName: "shell", Status: "completed"}}})
	tr.finish()
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
