# Display rules

One contract: **the transcript contains assistant text, assistant thinking, and
tool calls pig itself can run. Nothing else.**

`internal/provider/emit.go` is the single object that turns Cursor run events
into pig provider events, so the rules are one place, testable without a bridge.

## The rules

1. **Only three kinds of content reach the transcript.** Text, thinking, and
   `toolCall` blocks. Any other pig provider event type is a bug.
2. **A tool call is a real pig tool call.** The block carries the host's own tool
   name and the parsed argument object, so pig draws its native card and runs its
   own hooks, permissions, and result rendering. The `id` is Cursor's call id,
   which is what the resumed run matches its tool result against.
3. **The tool channel is never faked.** Cursor's shell/read/write activity, step
   frames, lifecycle chatter, and protocol payloads never enter text or thinking.
   If a call is not a host tool there is no card the host could attach to, so it
   produces no transcript content — and it is not silently invisible either: the
   footer status line says `Cursor ran Shell (its own tool)`, once per tool.
4. **One card per call, resolved exactly once.** The bridge callback is the
   authoritative record of a host tool call; the stream's own `tool_call` frame
   for the same id is bookkeeping only. A duplicate callback for the same id is
   rejected instead of running the tool twice.
5. **Streamed text is never duplicated.** With deltas enabled, the `sdk_message`
   copies of the same text are dropped in the decoder, so a delta appears once.
6. **A resumed turn re-emits nothing already shown.** After a park the run is
   continued on the durable `ObserveRun` stream, which replays from the beginning
   because a live `Send` offset must never be passed as `after_offset`. Text and
   thinking stay suppressed until the parked call reports completion; the run's
   answer arrives from the result frame.
7. **Signals belong in the footer, not the transcript.** Mode, model, and bridge
   pid go to `ctx.SetStatus`; usage comes from Cursor's counters with
   `totalFromProvider: false`, since Cursor reports tokens, not dollars.
8. **Failures are one readable line.** A bridge or run failure ends the turn with
   a classified message ("Cursor did not respond within 5m; the run was released"),
   never a stack or a raw payload.

## Enforcement

`internal/provider/emit_test.go`:

| rule | test |
| --- | --- |
| 1, 3 | `TestEmitsOnlyTextThinkingAndHostToolCalls` (also asserts the one footer notice for Cursor's own tool) |
| 2 | `TestHostToolCardCarriesRealNameAndArgs` |
| 5 | `TestStreamedTextIsNeverDuplicated` |
| 6 | `TestResumedTurnStillDeliversTheFinalAnswer` |
| 8 | `TestStatusFailureBecomesAReadableError` |
| 1, 3, 7 | `TestNoProviderEventCarriesRawBridgeJSON` — runs 218 frames captured from a real bridge through decoder and emitter, then rejects any event carrying `providerIdentifier`, `custom-user-tools`, `tool-call-started`, `RUN_LIFECYCLE_STATUS`, `modelCallId`, or `agent_id` |

Exactly-once and duplicate-callback behaviour (rule 4) is covered in
`internal/cursor/run_test.go`.

## A batch of tool calls is one turn

Cursor can ask for several host tools in a single turn, and pig runs every call
we hand back. So the turn does not end at the first call: after one arrives the
stream is read for a short quiet window (`PI_CURSOR_TOOL_BATCH_GRACE_MS`,
default 900 ms) or until Cursor reports `tool_pending`, whichever comes first,
and the run parks with the whole batch outstanding. Handing back only the first
call left Cursor waiting for a result that never arrived — measured in
`dev/run-edge.sh` section 1 before the fix, where two calls were parked and one
was answered.

Live proof after the fix: `docs/evidence/live-batch-tool-turn.jsonl` (host
executed `echo one` and `echo two`), and `dev/toolcalls.py` over that run's log
slice reports one park and one delivery per call id.

## Ids are one token

Cursor publishes tool-call ids that contain a literal newline:

```text
call-dc6de823-12c8-4268-b573-5c28e0ae464f-1\nfc_00b4b5ed-..._0
```

Passing that through puts a newline into the host's session transcript and breaks
one-line-per-event log parsing. So an id has two forms: Cursor's raw id stays the
internal key (parked ledger, replay dedup), and `cursor.DisplayCallID` produces the
form the host and the log see, which percent-encodes whitespace
(`…-1%0Afc_…_0`). It is not a lossy collapse — `a\nb`, `a b` and `a_b` must stay
distinguishable, because the value keys calls — so it is reversible
(`cursor.DecodeCallID`), idempotent, and applied at both points an id enters: the
tool-call frame and the tool callback. Same function on both paths means the
ledger key, the card id the host holds, and the log line always agree.

`TestDisplayCallIDSplitsWhitespaceWithoutLosingIt` pins reversibility,
idempotence, and the collision cases;
`TestParkedCallCarriesHostSafeID` pins that the ledger keeps the raw id while the
host gets the encoded one.

## A late tail of a batch is still a call

The batch window is a window, not a guarantee: the run can park on the first call,
and Cursor can send the rest of the turn afterwards. `park` then registers the late
call against the same run, so the next provider call hands it over and resolves it
like any other. `TestLateCallbackAfterFirstParkStillResolves` drives exactly that:
park, pause the stream, park again, then assert both calls are pending in arrival
order, each accepts exactly one result (the second resolve of the same id is
refused), both callbacks return, and nothing is left pending.

`dev/run-edge.sh` also checks that a `SIGTERM`'d host leaves no orphaned bridge:
bridges before = 0, after = 0.

## What it replaced

`docs/evidence/current-display-audit.md` measured the previous extension across
132 retained sessions: 60.7% of thinking characters in Cursor turns were injected
tool traces, and no assistant message persisted a tool card. The command and its
output survived only as fake thinking text, merged with the model's reasoning.

Live proof of the current behaviour: `docs/evidence/tty-session.txt` (terminal
capture) and `docs/evidence/live-tool-turn.jsonl` (host event stream).
