# Stream Recovery and Tool Cards Implementation Plan

> **For agentic workers:** Use todo tools to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Avoid killing active turns, recover bounded stream interruptions without resending requests, keep native host tools and improve internal annotation styling.

**Architecture:** Separate default inactivity waits from explicit wall caps; longer bounded waits for a known running internal tool. Reattach failed streams with ObserveRun and exact-prefix replay checks, retaining callback identities and never sending the prompt twice. Recreate agents on schema changes at non-parked turn boundaries. Use display-only Markdown transformation for internal annotations if actual terminal checks establish it works.

**Tech Stack:** Go SDK, Connect streaming, Go httptest, PTY/pyte.

## Global Constraints
- Host calls remain executable pig tools rendered natively; internal calls must never be executed by pig.
- No byte rewriting of broken framing; only bounded ObserveRun reattachment.
- Reject replay divergence rather than guessing or duplicating output/actions.
- Preserve configured PI_CURSOR_TURN_TIMEOUT wall cap and caller cancellation.
- Validate only relevant tests and focused UI checks.

### Task 1: Timeouts and reconnect
**Files:** provider engine + new activity helpers/tests, cursor run + recovery helper/tests.
- [x] Reset default inactivity wait on decoded progress; use bounded tool-running wait; report cleanup accurately and bound its RPC.
- [x] Retry only classified stream truncation/framing failures, at most twice, with existing run ID and durable offsets where available.
- [x] Retain queue/callback ledger; exact-prefix suppress repeated text/thinking without assuming delta boundaries; dedupe internal IDs. On mismatches fail safely.
- [x] Fake Connect tests prove one Send, ObserveRun recovery, replay suppression and rejection, no unknown-run retries.

### Task 2: Native routing and display
**Files:** provider prompt/tool signature logic + tests; extension display hook + provider rendering tests and PTY script.
- [x] Hash complete sorted schemas/descriptions, recreate on tool list changes (after pending call resolution, never during callback execution).
- [x] Carry pig instructions and explicit host-tool preference into initial prompt using the bridge's available prompt surface; keep internal tools as fallback, no shell→bash aliases.
- [x] Probe display-only transform for padded native-theme internal annotations, leaving executable host cards alone.
- [x] Run focused regression/build/vet and actual UI checks. If hook cannot provide safe style, document remaining limitation rather than claiming parity.
