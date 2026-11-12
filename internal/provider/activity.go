package provider

import (
	"fmt"
	"os"
	"strings"
	"time"

	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func durationEnv(key string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return fallback
}

type activityWatch struct {
	idle, tool time.Duration
	last       time.Time
	active     map[string]time.Time
}

func newActivityWatch(now time.Time) *activityWatch {
	return &activityWatch{idle: durationEnv("PI_CURSOR_IDLE_TIMEOUT", 5*time.Minute),
		tool: durationEnv("PI_CURSOR_TOOL_TIMEOUT", 30*time.Minute), last: now, active: map[string]time.Time{}}
}

// Repeated status/usage chatter is not progress. A known running internal tool
// gets a larger but absolute grace budget: a lost completion cannot extend it.
func (w *activityWatch) note(ev cursor.RunEvent, now time.Time) {
	switch ev.Kind {
	case cursor.EventCursorToolStarted:
		if ev.ToolCall != nil {
			id := ev.ToolCall.ID
			if id == "" {
				id = ev.ToolCall.ToolName
			}
			if _, ok := w.active[id]; !ok {
				w.active[id] = now
				w.last = now
			}
		}
	case cursor.EventCursorToolCompleted:
		if ev.ToolCall != nil {
			id := ev.ToolCall.ID
			if id == "" {
				id = ev.ToolCall.ToolName
			}
			if _, ok := w.active[id]; ok {
				delete(w.active, id)
				w.last = now
			}
		}
	case cursor.EventTextDelta, cursor.EventThinkingDelta, cursor.EventTask:
		if strings.TrimSpace(ev.Text) != "" {
			w.last = now
		}
	case cursor.EventToolCallStarted, cursor.EventResult:
		w.last = now
	}
}

func (w *activityWatch) wait(now time.Time) time.Duration {
	deadline := w.last.Add(w.idle)
	if len(w.active) > 0 {
		first := true
		for _, started := range w.active {
			d := started.Add(w.tool)
			if first || d.Before(deadline) {
				deadline = d
				first = false
			}
		}
	}
	d := deadline.Sub(now)
	if d <= 0 {
		return time.Nanosecond
	}
	return d
}

func (w *activityWatch) timeoutMessage(wall bool, releaseErr error) string {
	msg := fmt.Sprintf("Cursor sent no progress for %s", w.idle)
	if len(w.active) > 0 {
		msg = fmt.Sprintf("Cursor's internal tool did not finish within %s", w.tool)
	}
	if wall {
		msg = "Cursor reached the configured PI_CURSOR_TURN_TIMEOUT limit"
	}
	if releaseErr != nil {
		return msg + ". Local waiting stopped, but upstream cancellation was not confirmed; use /cursor-abort before retrying."
	}
	return msg + ". Local waiting stopped and cancellation was requested. You can retry."
}
