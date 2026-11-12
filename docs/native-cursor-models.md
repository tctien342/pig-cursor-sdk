# Native-model behavior

Cursor remains an external agent behind a bridge. These changes preserve pig's
conversation, controls and tool boundary without claiming identical backends.

## Conversation continuity

Within an owned, live session, accepted transcript prefixes are compared using
complete text, image data, tool arguments and error state. New image-only user
messages append normally. Assistant/tool-only suffixes do not resend the previous
user request. Edits and branches invalidate the prefix rather than silently
continuing a different conversation.

Bookkeeping advances only after a successful Send. Ambiguous delivery errors do
not trigger automatic prompt resends. Fresh processes replay conservatively when
there is no trustworthy continuity metadata; this is not a persistent server-side
conversation-ID guarantee.

## Native pig tools

Advertised pig tools remain real pig `toolCall` / `toolResult` executions, with
pig's native cards, permissions, transcript and expansion details. Cursor's own
tools remain display-only inline annotations.

The installed bridge recognizes an SDK result envelope when `content` is an
array. The adapter now sends text/image content, `isError` and optional explicit
`structuredContent` through that envelope. Ordinary tool errors are results, not
callback transport failures. Empty successful text remains empty. Completed
callbacks, including failures, remain cached for safe same-run reconnects.

Inline image data plus MIME type are supported. Unsupported content-block types
become text containing their JSON representation; an image missing inline data
or MIME type gets an explanatory text placeholder. Native pig `details` stay in
pig's transcript/UI, not in Cursor's model input. This is not support for every
pig-specific output or attachment feature.

## Escape and `/cursor-abort`

Escape propagates caller cancellation. `/cursor-abort` also asks the host to abort
its current Cursor request and cancels only this logical session's owned model
branches. Scope includes session file and cwd; ephemeral sessions additionally
use a host identity/process nonce. Unknown identity never broadens cancellation
to other sessions in the same directory.

Local waits are released before cancellation RPCs. Upstream cancellation has a
five-second bound, is idempotent after acknowledgment, and may be retried after
failure. The command distinguishes acknowledged cancellation, already-finished
work and an unconfirmed upstream result. Completed host work is not undone, and
an acknowledgment is not proof that every external child process was killed.
If startup is interrupted before the SDK exposes a run ID, remote cancellation
cannot be confirmed; local waiting stops without automatically resending the
prompt. A closed response stream alone is never treated as a cancellation
acknowledgment.

The current legacy Go-provider/host boundary records an interrupted request as
`stopReason: error` / `context canceled`, even when the extension emits an aborted
outcome and the upstream cancellation is acknowledged. Live checks separately
verify the acknowledgment and successful next turn; this host-side error record
is not rewritten or presented as full native-provider parity.

Turns serialize per session while cancellation uses a separate, short state gate.
Agent publication checks the abort generation. Shared bridge initialization is
single-flight and engine-owned: cancelling one waiter does not cancel another
session's startup or cache a permanent cancellation error. Initialization is
bounded to 30 seconds. Stop cancels it without waiting for readiness; an initializer
that returns a late handle closes that handle outside the engine mutex. Ready
agents and bridge resources have bounded cleanup. No engine registry mutex is
held across host calls, bridge startup or cancellation RPCs.

## Thinking, context and fast mode

Parameter resolution is pure and does not mutate catalog defaults. `off` targets
boolean thinking separately from effort; `none`, `false` and `extra-high` aliases
are normalized. Explicit request options win over installed defaults and host
context. Unsupported selections retain defaults and produce a warning instead
of pretending a setting was applied. Effective parameter sets determine agent
reuse, not just the requested label.

Context variants remain registered model IDs. Explicit context selections take
precedence over generic cached windows. Fast variants share cached windows only
when they are equivalent to the native default fast value. Bare lookup aliases
resolve to the native default context.

Use `/cursor-fast on`, `/cursor-fast off` or `/cursor-fast default` to select the
registered model variant, including its context-window metadata. A change applies
to the next new request, not a parked tool continuation. Catalog and cached
window information may still be incomplete; this does not discover unpublished
server limits.

## `/cursor-doctor`

The default command reads a passive, privacy-limited snapshot:

- active pig tool names/count;
- selected model, context metadata and resolved controls;
- bridge not-started/initializing/client-present state and cached protocol;
- this logical session's run state, pending/acknowledged/unconfirmed cancellation,
  pending tool count and actual last-sent controls;
- bounded recent recovery/cancellation events and classified process failures.

It does not start a bridge, create an agent, inspect credentials, dump private
paths, or include prompts, tool arguments or tool output. Unknown control
parameters and raw provider errors are excluded. A client-present snapshot is
not a liveness claim. Authentication failures are classified only after an
actual request; diagnosis itself does not verify a credential.

`/cursor-doctor --probe` performs one bounded two-second ping of an existing
bridge. It skips an absent bridge rather than starting one. Neither mode makes
a model request. Existing startup allowance/account workers are separate from
this command.

## Focused checks

```bash
go test . ./internal/cursor ./internal/provider
go test -race ./internal/cursor ./internal/provider -run 'Test(BridgeStartup|Cancellation|QueueCancellation|CancelledWait|LateBatched|AbortScope|AbortGeneration|HostResultReplay|HostToolResultEnvelope|OrdinaryToolFailure|Doctor|ExplicitControls|AgentSpecUses|ImageOnly|Continuity)' -count=1
go vet ./internal/cursor ./internal/provider
go build .
pig install . --validate-only --json
dev/.venv/bin/python dev/tty-cursor-doctor.py       # offline commands; no model turn
dev/.venv/bin/python dev/tty-host-tool.py --error  # one live native-tool failure
dev/.venv/bin/python dev/tty-cancel-check.py       # live Escape + abort; four model turns
```

See the dated evidence file for checks actually run. Unit/Connect tests cover
images, callback replay and isolation; the selected PTY paths do not prove every
model, image format, cancellation timing or remote service behavior.
