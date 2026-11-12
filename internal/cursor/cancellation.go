package cursor

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
)

func (r *Run) queueRef() *eventQueue { r.mu.Lock(); defer r.mu.Unlock(); return r.queue }

func IsCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || connect.CodeOf(err) == connect.CodeCanceled
}

type RunStatus struct {
	ID              string
	Finished        bool
	TerminalStatus  string
	Paused          bool
	Pending         int
	Recoveries      int
	CancelRequested bool
	CancelConfirmed bool
	CancelFailed    bool
	StreamFailed    bool
}

// Status is safe for commands/diagnostics: no prompt, arguments or tool output.
func (r *Run) Status() RunStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := RunStatus{ID: r.runID, Finished: r.fin, Paused: r.paused, Pending: len(r.parked), Recoveries: r.recoveries,
		CancelRequested: r.cancelRequested, CancelConfirmed: r.cancelConfirmed, CancelFailed: r.cancelError != nil, StreamFailed: r.drainErr != nil && !IsCancellation(r.drainErr)}
	if r.terminal != nil {
		status.TerminalStatus = r.terminal.Status
	}
	return status
}

// CancelRunContext always releases local waiters, even when upstream cancellation
// fails. Failed requests may be retried by /cursor-abort; successful ones are
// idempotent. Caller cancellation cannot make this RPC run beyond five seconds.
func (r *Run) CancelRunContext(parent context.Context, reason string) error {
	r.abandon(errors.New(orString(reason, "cancelled")))
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	r.mu.Lock()
	if r.cancelConfirmed {
		r.mu.Unlock()
		return nil
	}
	if r.terminal != nil {
		r.mu.Unlock()
		return nil
	}
	r.cancelRequested = true
	id := r.runID
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	var err error
	switch {
	case id == "":
		err = errors.New("cursor: local run stopped, but upstream cancellation is unavailable without a run ID")
	case r.agent == nil || r.agent.client == nil:
		err = errors.New("cursor: local run stopped, but the bridge is unavailable")
	case ctx.Err() != nil:
		err = ctx.Err()
	default:
		request := &sdkpb.CancelRunRequest{RunId: id}
		if r.agent.id != "" {
			agentID := r.agent.id
			request.AgentId = &agentID
		}
		_, err = r.agent.client.Agent.CancelRun(ctx, connect.NewRequest(request))
		err = wrapErr("CancelRun", err)
	}
	r.mu.Lock()
	r.cancelError = err
	r.cancelConfirmed = err == nil
	r.mu.Unlock()
	return err
}
