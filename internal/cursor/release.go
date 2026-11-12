package cursor

import (
	"context"

	"connectrpc.com/connect"
	sdkpb "github.com/tctien342/pig-cursor-sdk/gen/sdk/v1"
)

// ReleaseStaleRuns cancels runs that are live on this agent but were not started
// by this process. That is what a killed session leaves behind: Cursor keeps the
// run open, and every later send to the agent fails with "already has active
// run", which the caller could only escape by discarding the agent and replaying
// the whole conversation. Cancelling the leftover run keeps the agent — and
// Cursor's side of the conversation — usable.
//
// It returns how many runs it cancelled. A bridge that does not implement
// ListRuns reports an error, which the caller treats as "could not release", not
// as a failure of the turn.
func (a *Agent) ReleaseStaleRuns(ctx context.Context) (int, error) {
	agentID := a.ID()
	cancelled := 0
	pageCursor := ""
	for page := 0; page < 5; page++ {
		resp, err := a.client.Agent.ListRuns(ctx, connect.NewRequest(&sdkpb.ListRunsRequest{
			AgentId: agentID,
			Options: &sdkpb.ListRunsOptions{Limit: 50, Cursor: pageCursor},
		}))
		if err != nil {
			return cancelled, wrapErr("ListRuns", err)
		}
		for _, run := range resp.Msg.GetItems() {
			switch run.GetStatus() {
			case sdkpb.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_CREATING,
				sdkpb.RunLifecycleStatus_RUN_LIFECYCLE_STATUS_RUNNING:
			default:
				continue // finished runs hold nothing
			}
			req := &sdkpb.CancelRunRequest{RunId: run.GetRunId()}
			if agentID != "" {
				req.AgentId = &agentID
			}
			if _, err := a.client.Agent.CancelRun(ctx, connect.NewRequest(req)); err != nil {
				return cancelled, wrapErr("CancelRun", err)
			}
			cancelled++
		}
		next := resp.Msg.GetNextCursor()
		if next == "" || next == pageCursor {
			break
		}
		pageCursor = next
	}
	return cancelled, nil
}
