# Display rules

One contract: **what the model owns is text, thinking, and tool calls pig itself
can run. What Cursor did on its own is an annotation the extension draws. Nothing
else, and nothing of Cursor's protocol in either.**

`internal/provider/emit.go` is the single object that turns Cursor run events
into pig provider events, so the rules are one place, testable without a bridge.
`internal/provider/cursortools.go` owns the one exception below.

## The rules

1. **Only three kinds of content reach the transcript.** Text, thinking, and
   `toolCall` blocks. Any other pig provider event type is a bug.
2. **A tool call is a real pig tool call.** The block carries the host's own tool
   name and the parsed argument object, so pig draws its native card and runs its
   own hooks, permissions, and result rendering. The `id` is Cursor's call id,
   which is what the resumed run matches its tool result against.
3. **The tool channel is never faked.** Cursor's shell/read/write activity, step
   frames, lifecycle chatter, and protocol payloads never enter text or thinking,
   and never become a `toolCall` block: there is no host tool behind them, so a
   card would promise a tool call pig cannot run. What Cursor ran instead is one
   annotation line per call — see *Cursor's own tool calls* below. While a call is
   still running the footer says `Cursor running shell · ls -1`, and it clears when
   the line replaces it.
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
| 1, 3 | `TestEmitsOnlyTextThinkingAndHostToolCalls` — a Cursor-side call produces no provider event at all, even if the run stream ever fails to tag one |
| 2 | `TestHostToolCardCarriesRealNameAndArgs` |
| 5 | `TestStreamedTextIsNeverDuplicated` |
| 6 | `TestResumedTurnStillDeliversTheFinalAnswer` |
| 8 | `TestStatusFailureBecomesAReadableError` |
| 1, 3, 7 | `TestNoProviderEventCarriesRawBridgeJSON` — runs 218 frames captured from a real bridge through decoder and emitter, then rejects any event carrying `providerIdentifier`, `custom-user-tools`, `tool-call-started`, `RUN_LIFECYCLE_STATUS`, `modelCallId`, or `agent_id` |

Exactly-once and duplicate-callback behaviour (rule 4) is covered in
`internal/cursor/run_test.go`.

## Cursor's own tool calls

A Cursor agent has built-in tools that were never advertised to it — its own
shell, reader, search. pig cannot run them and has no card to attach to, so for a
long time this extension printed nothing, which read as the agent stalling.
Silence was the wrong answer: what Cursor ran is a fact about the turn.

It is shown as **one custom session entry per call**, rendered by the extension
(`e.EntryRenderer(provider.CursorToolEntryType, …)`):

```text
 ◇ cursor: shell · ls -1 · 2.6s          finished; the duration says it ran
 ◇ cursor: shell · cargo test · failed   Cursor reported an error
 ◇ cursor: read · src/x.go · no result   the turn ended while it was running
 ◇ cursor: grep · deferPlanWork          no duration: it was under 0.1s
```

with ctrl+o expanding the arguments it ran with:

```text
 ◇ cursor: shell · ls -1 · 2.6s
   command: ls -1
   timeout: 30000
```

### It is drawn as a card, in the host's own colours

Each call gets the background pig gives a tool card that ended the same way —
`toolSuccessBg`, `toolErrorBg`, `toolPendingBg` — and the card is a **band of three
painted rows**: an empty row, the line, an empty row, all in that colour, so the
text sits inside the colour instead of floating in a stripe. Padding outside the
colour would just be space; padding inside it is what makes one call read as one
card, which matters most when a turn fires a burst of them.

A theme is allowed to map those tokens to the terminal default, in which case
there is no band to draw and nothing else changes; the extension logs that once per
process (`cursor tool card has no background in this theme`) because the difference
between "no colour" and "no card" is otherwise invisible.

### It is written after the message it belongs to

The entries are appended once the turn's assistant message has been committed,
never while it streams. pig writes an entry the moment it is asked to and commits
the message at the end of the turn, so anything written mid-turn lands **above** the
thinking and text it was interleaved with. In a real session that showed fourteen
shell calls stacked over a message describing them, which reads as tools firing
inside a thought. Below the message they read as what they are: that turn's
activity, in call order. A turn that ends before retrying writes nothing — its
retry reports its own calls.

Why an entry and not a provider event or a message:

- a custom entry is **not model context** (session-format: "It does not become
  model context"), so neither pig's next turn nor the transcript this extension
  replays to Cursor ever contains it. Injecting tool traces into the assistant
  message is exactly what put 60% of the thinking characters in the old extension.
- it persists, so a resumed session redraws the same line from stored data — with
  no live run, and with whatever theme the user has now.
- the summary is chosen per kind of call (command for a shell, pattern for a
  grep, path for a read/edit), because a grep line without its pattern is noise.
- one line, never two: the live stream reports the same call twice through two
  envelopes, so calls are keyed by Cursor's id, and unfinished calls are written
  at turn end as `no result` rather than vanishing.

Nothing of Cursor's tool *output* is stored or shown: measured on a real account,
a built-in call reports no result text at all, and a second agent's tool dump in
the transcript is what this document exists to forbid.

`PI_CURSOR_SHOW_TOOLS=0` turns the lines off; the footer hint stays, so the tool
activity is never silently invisible.

| rule | test |
| --- | --- |
| one line per call, unfinished calls say so | `TestTrackerWritesOneEntryPerCall`, `TestCursorToolLineNeverClaimsWhatDidNotHappen` |
| what the summary says | `TestCursorToolSummaryNamesWhatTheCallDid` |
| one line, never wrapped, outcome survives a narrow terminal | `TestCursorToolLineStaysOneLineAndKeepsItsOutcome` |
| expanded view adds the arguments | `TestCursorToolExpandedAddsArguments` |
| a rejected entry must not fail the turn | `TestTrackerSurvivesARejectedEntry` |
| nothing is written until the message is committed | `TestTrackerWritesNothingUntilTheTurnIsCommitted` |
| the band is painted blank row, line, blank row | `TestCursorToolCardPadsInsideTheBackground`, `TestPaintCursorToolLineFillsTheWidth` |
| the band colour follows the outcome | `TestCursorToolBackgroundMatchesOutcome` |

Live proof: `dev/tty-cursor-tools.py` — runs a turn with no host tools advertised
(so Cursor can only use its own), asserts the line is on the real screen, that the
three rows of the card share a background **as cells on the emulated screen** (not
as escape codes that were merely written), that ctrl+o expands it, that the entry
sits below its message in the session file, and that reopening the session draws it
again from the entry.

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
