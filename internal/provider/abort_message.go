package provider

import (
	"context"
	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
	cursor "github.com/tctien342/pig-cursor-sdk/internal/cursor"
)

func channelClosed(done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func cancelInterruptedTurn(ctx context.Context, run *cursor.Run) bool {
	if ctx.Err() != context.Canceled && !cursor.IsCancellation(run.DrainErr()) {
		return false
	}
	// Run.CancelRun uses a fresh bounded context and records unconfirmed failures.
	_ = run.CancelRun("host cancelled the turn")
	return true
}

func endCancelled(stream *sdk.ModelEventStream, p *partial) {
	p.setStop("aborted", p.usage)
	stream.Push(snapshotMap(map[string]any{"type": "done", "reason": "aborted", "message": p.msg}))
	stream.End(snapshotMap(p.msg))
}
