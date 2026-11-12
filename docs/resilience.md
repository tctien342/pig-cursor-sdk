# Cursor stream resilience and native tools

## Timeouts

The default is an inactivity budget, not a five-minute total run limit:

- `PI_CURSOR_IDLE_TIMEOUT` defaults to `5m`. Text/thinking deltas and meaningful
  tool activity refresh it; status/usage chatter does not.
- A known running internal tool gets `PI_CURSOR_TOOL_TIMEOUT` (default `30m`)
  from its first start event. Duplicate starts and unrelated text cannot extend
  this absolute tool budget. A missing completion cannot wait forever.
- Setting `PI_CURSOR_TURN_TIMEOUT` explicitly retains the old wall-clock cap.
  Host cancellation always wins.

A timeout stops local waiting and requests upstream cancellation with a separate
five-second RPC bound. The error distinguishes a cancellation request from failure
to confirm cancellation; it no longer asserts that a remote run was released.

## Interrupted response streams

An incomplete envelope, unexpected EOF or connection reset may reattach with
`ObserveRun`, at most twice per run. The original prompt is **not sent again**.
The same event queue, agent and host callback ledger remain in use.

Only durable ObserveRun offsets are reused. The initial Send stream's offset is
not a durable cursor. Without a durable offset, text/thinking replay must match
the exact already-displayed prefix (delta chunk sizes may differ). Repeated
internal calls are identified by call ID. A different prefix, missing required
identity, exhausted attempts, auth errors or user cancellation are not blindly
retried. Partial output remains available with a readable recovery error.

During an active run, replayed callbacks for already-settled host calls receive the stored result,
never another native execution. A terminal answer containing the displayed prefix
contributes only its missing suffix.

This does not make malformed framing valid, nor guarantee that an upstream run
can always be resumed. Bytes are never stripped or rewritten to bypass parsing.

## Native pig tools versus Cursor's own tools

Advertised pig tools still use real `toolCall` blocks, execute in pig, and return
real `toolResult` messages. Cursor-internal calls never become executable pig
calls and are never renamed from `shell` into `bash`.

Tool-schema changes previously updated only the local tool-name set. That did not
re-register schemas with Cursor. The provider now hashes sorted names, complete
schemas and descriptions, and recreates/replays the agent at a safe new-turn
boundary when that signature changes. Parked host calls are resolved first.

Pig's session instructions and a preference for advertised host tools are forwarded
through Cursor's available prompt surface (not a native system-role API, which
this bridge schema does not expose). Cursor may still choose built-ins where no
host tool fits; this is a preference, not a promise of model compliance.

Internal calls remain inline between surrounding text/thinking. A display-only
transform paints standalone marked internal blocks using pig's tool background
and foreground, including blank painted padding above/below. It does not change
stored text or the native host tool renderer.

## Ownership of stream messages

Provider events are frozen before asynchronous SDK delivery. Sharing a live
partial-message map with the SDK caused a real native-tool round trip to crash
with `concurrent map iteration and map write`. Earlier events, nested arguments,
and final/error messages now have independent snapshots. Live emitters do not
retain a quadratic history of those snapshots.

## Focused checks

- `go test ./internal/cursor ./internal/provider`
- Race checks for recovery, settled callback replay and SDK event handoff.
- `dev/.venv/bin/python dev/tty-tool-style.py`: offline persisted internal card
  beside a native bash card/output; ordinary unmarked text remains unstyled.
- `dev/.venv/bin/python dev/tty-host-tool.py`: live pig bash execution, matching
  successful result and assistant continuation; no conversion to internal text.

Tests include genuinely truncated Connect envelopes, rechunked replay, durable
cursor reuse, divergence rejection and bounded retry exhaustion. They do not
claim to reproduce every network failure or every model's tool-selection behavior.
