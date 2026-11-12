package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"connectrpc.com/connect"
	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
)

const maxStreamRecoveries = 2

// IsInterruptedStream recognizes transport/framing failures, not model refusals,
// cancellation, authentication failures, or arbitrary malformed protocol data.
func IsInterruptedStream(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "incomplete envelope") || strings.Contains(msg, "unexpected eof") ||
		strings.Contains(msg, "connection reset by peer") || strings.Contains(msg, "broken pipe")
}

// reconnect observes the SAME run; it never resends the prompt or creates a run.
// It executes in the drain goroutine, leaving the event queue and callback routes
// intact. Only durable ObserveRun offsets can be reused; Send offsets cannot.
func (r *Run) reconnect(err error) (*connect.ServerStreamForClient[sdkpb.RunStreamMessage], bool) {
	if !IsInterruptedStream(err) {
		return nil, false
	}
	r.mu.Lock()
	if r.fin || r.terminal != nil || r.paused || r.ctx.Err() != nil || r.runID == "" || r.recoveries >= maxStreamRecoveries {
		r.mu.Unlock()
		return nil, false
	}
	rid, off := r.runID, r.observeOffset
	r.recoveries++
	attempt := r.recoveries
	if off == "" {
		r.replayText = r.deliveredText.String()
		r.replayThinking = r.deliveredThinking.String()
	} else {
		r.replayText = ""
		r.replayThinking = ""
	}
	r.recovering = true
	r.resumed = true
	r.mu.Unlock()
	if r.agent.client.log != nil {
		r.agent.client.log.Info("Cursor stream interrupted; reattaching to the existing run", "attempt", attempt, "run", rid)
	}
	req := &sdkpb.ObserveRunRequest{RunId: rid}
	if off != "" {
		req.AfterOffset = &off
	}
	ctx, cancel := context.WithCancel(r.ctx)
	stream, observeErr := r.agent.client.Agent.ObserveRun(ctx, connect.NewRequest(req))
	if observeErr != nil {
		cancel()
		return nil, false
	}
	r.mu.Lock()
	old := r.cancelStream
	r.cancelStream = cancel
	r.mu.Unlock()
	if old != nil {
		old()
	}
	return stream, true
}

func replaySuffix(remaining *string, delta string) (string, error) {
	if *remaining == "" {
		return delta, nil
	}
	if strings.HasPrefix(*remaining, delta) {
		*remaining = (*remaining)[len(delta):]
		return "", nil
	}
	if strings.HasPrefix(delta, *remaining) {
		delta = delta[len(*remaining):]
		*remaining = ""
		return delta, nil
	}
	return "", errors.New("cursor: stream replay changed already displayed text; stopped to avoid duplicate output")
}

// replayEvent supports different delta chunk boundaries but requires the same
// exact prefix. Never guess if upstream replays different content.
func (r *Run) replayEvent(ev Event) (Event, bool, error) {
	internal := ev.ToolCall != nil && !r.agent.IsHostTool(ev.ToolCall.ToolName)
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if ev.Kind == EventTextDelta || ev.Kind == EventThinkingDelta {
		if r.recovering {
			if ev.Kind == EventTextDelta {
				ev.Text, err = replaySuffix(&r.replayText, ev.Text)
			} else {
				ev.Text, err = replaySuffix(&r.replayThinking, ev.Text)
			}
			if err != nil {
				return ev, false, err
			}
		}
		if ev.Text == "" {
			return ev, false, nil
		}
		if ev.Kind == EventTextDelta {
			r.deliveredText.WriteString(ev.Text)
		} else {
			r.deliveredThinking.WriteString(ev.Text)
		}
	}
	if ev.Kind == EventResult && ev.Result != nil && r.recovering {
		if r.replayThinking != "" || (r.replayText != "" && !strings.HasPrefix(ev.Result.Text, r.deliveredText.String())) {
			return ev, false, errors.New("cursor: recovered run did not confirm the displayed prefix; partial output was kept")
		}
		r.replayText = "" // the full terminal answer confirmed its displayed prefix
	}
	if ev.Kind == EventDone && r.recovering && (r.replayText != "" || r.replayThinking != "") {
		return ev, false, errors.New("cursor: recovered stream ended before confirming its replay prefix")
	}
	if internal {
		key := string(ev.Kind) + "\x00" + ev.ToolCall.ID
		if r.recovering && ev.ToolCall.ID == "" {
			return ev, false, errors.New("cursor: cannot safely replay an internal tool without a call ID")
		}
		if r.internalSeen == nil {
			r.internalSeen = map[string]bool{}
		}
		if ev.ToolCall.ID != "" {
			if r.internalSeen[key] {
				return ev, false, nil
			}
			r.internalSeen[key] = true
		}
	}
	return ev, true, nil
}

func sameCallback(previous *ParkedCall, req ToolCallRequest) bool {
	if previous.ToolName != req.ToolName {
		return false
	}
	if len(previous.Args) == 0 && len(req.Args) == 0 {
		return true
	}
	a, errA := json.Marshal(previous.Args)
	b, errB := json.Marshal(req.Args)
	return errA == nil && errB == nil && string(a) == string(b)
}

func (r *Run) queueEvent(ev Event) bool {
	if r.ctx.Err() != nil {
		return true
	}
	if r.suppressed(ev.Kind) {
		return true
	}
	ev, emit, err := r.replayEvent(ev)
	if err != nil {
		r.mu.Lock()
		r.drainErr = err
		r.mu.Unlock()
		r.abandon(err)
		return false
	}
	if !emit {
		return true
	}
	if !r.queueRef().push(RunEvent{Event: ev}) {
		r.overflow()
		return false
	}
	return true
}
