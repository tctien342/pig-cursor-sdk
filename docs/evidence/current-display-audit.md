# Current display audit: why Cursor tool activity is noisy in the pig terminal

Read-only audit of the **TypeScript** extension installed at
`~/.pig/agent/npm/node_modules/pi-cursor-sdk` (`pi-cursor-sdk@0.5.26`),
plus the real session corpus in `~/.pig/agent/sessions/**/*.jsonl`.

Purpose: give the Go rewrite a precise list of what the TS extension puts on screen, which
code produces it, and which payload shapes pig's renderer turns into clutter. No network was
used and no repository file other than this one was written.

Paths below are relative to the extension root unless they are session paths or pig docs.

---

## 0. Evidence base and method

Extension sources read:

- `src/cursor-tool-presentation-registry.ts`
- `src/cursor-transcript-tool-formatters.ts`
- `src/cursor-transcript-tool-specs.ts`
- `src/cursor-native-tool-display-replay.ts`
- `src/cursor-native-tool-display-tools.ts`
- `src/cursor-provider-turn-display-router.ts`
- `src/cursor-provider-turn-tool-ledger.ts`
- `src/cursor-compact-tool-summary.ts`
- `src/cursor-display-text.ts`
- `docs/cursor-native-tool-replay.md`, `docs/cursor-tool-surfaces.md`, `docs/cursor-native-tool-visual-audit.md`
- supporting: `cursor-native-replay-routing.ts`, `cursor-native-replay-trace.ts`, `cursor-incomplete-tool-visibility.ts`,
  `cursor-tool-lifecycle.ts`, `cursor-provider-turn-coordinator.ts`, `cursor-provider-turn-sdk-normalizer.ts`,
  `cursor-provider-live-run-drain.ts`, `cursor-partial-content-emitter.ts`, `cursor-native-tool-display-registration.ts`,
  `cursor-native-tool-display-state.ts`, `cursor-replay-tool-details.ts`, `cursor-transcript-utils.ts`,
  `cursor-usage-ledger.ts`, `cursor-tool-manifest.ts`

Pig docs used for renderer semantics: `message-types.md` (thinking / toolCall / toolResult blocks),
`session-format.md` (custom entries).

Session corpus measurement (all `**/*.jsonl`, 132 files):

| Measure | Count |
| --- | --- |
| Files containing a `cursor` provider `model_change` | 9 |
| Assistant messages with `provider: "cursor"` | 13 |
| `thinking` parts inside those messages | 18 |
| `text` parts inside those messages | 10 |
| `toolCall` parts inside those messages | **0** |
| Thinking characters that are injected tool traces (start with `Cursor ` and contain a `$ ` echo / multi-line body) | 10,318 of 16,991 (**60.7 %**) |

That last row is the headline: in every retained Cursor turn where a shell call actually ran,
**no tool card was persisted at all**. The command and its output exist only as `thinking`
text, merged with the model's own reasoning.

---

## 1. Content the extension injects that pig renders

| # | Payload | Producer | Lands in |
| --- | --- | --- | --- |
| 1 | `thinking` part — lifecycle pending line, e.g. `Cursor shell: <cmd>\n` | `src/cursor-tool-lifecycle.ts:87-94` via `cursor-provider-turn-coordinator.ts:186-193` | assistant message content, thinking channel |
| 2 | `thinking` part — live shell stdout/stderr progress, `Cursor shell stdout: <160 chars>\n` per chunk | `cursor-provider-turn-shell-output.ts:45-52`, emitted at `cursor-provider-turn-coordinator.ts:228-241` | thinking channel |
| 3 | `thinking` part — Cursor run summary, `Cursor summary: <240 chars>\n` | `cursor-provider-turn-coordinator.ts:243-249` | thinking channel |
| 4 | `thinking` part — completed tool transcript (whole `$ cmd` + output + `Took Ns`) when a card is not queued | `cursor-provider-turn-display-router.ts:100-104`, `:216-224` | thinking channel |
| 5 | `thinking` part — inactive replay one-liner `<title>: <summary>\n` | `cursor-native-replay-trace.ts:17-29`, routed at `cursor-provider-turn-display-router.ts:102-104` | thinking channel |
| 6 | `thinking` part — incomplete tool line / `<title> did not complete` | `cursor-incomplete-tool-visibility.ts:81-134` | thinking channel |
| 7 | `toolCall` part named `read`/`bash`/`grep`/`find`/`ls`/`edit`/`write` with **display-rewritten** arguments | `cursor-transcript-tool-specs.ts:302-372`; emitted at `cursor-provider-live-run-drain.ts:190-235` | assistant tool card |
| 8 | `toolCall` part named `cursor` with `activityTitle` / `activitySummary` arguments | `cursor-transcript-tool-specs.ts:84-96`, `:119-158`, `:161-183` | assistant tool card (neutral) |
| 9 | `toolResult` message: `content[0].text` = human transcript, `details` = replay variant (`nativeEdit`, `nativeWrite`, `activity`, `generateImage`, `genericFallback`) | `cursor-transcript-tool-specs.ts:97-117`; `cursor-replay-tool-details.ts:251-276` | tool card body |
| 10 | Session `custom` records `pi-cursor-sdk:usage-v1` (agent id, model selection, store identity, token events) | `cursor-usage-ledger.ts:13,44-52` | session JSONL only — pig does not render custom state entries (`session-format.md:140-143,193`) |
| 11 | System-prompt additions: Cursor SDK tool-boundary block + `Callable tool surfaces this run:` manifest listing every `pi__*` name | `cursor-tool-manifest.ts:34-47`; wired at `cursor-provider-turn-prepare.ts:325-336` | model context, not the TUI |
| 12 | `ui.notify` warning when replay names are owned by another extension | `cursor-native-tool-display-registration.ts:78-84` | transient notification |

Pig's message model gives the extension only three visible channels: assistant **text**,
assistant **thinking**, and **tool cards** (`message-types.md:47-56,61-66,174-186`). Everything
in rows 1-6 is therefore indistinguishable, at render time, from the model's own reasoning.

---

## 2. Concrete noise sources

### N1 — When a card is not queued, the whole tool transcript is written into thinking

`CursorTurnDisplayRouter.routeCompletedToolCall` computes the full transcript
(`src/cursor-provider-turn-display-router.ts:75`) and, unless the disposition is
`queue_replay`, emits it as thinking text (`:100-104`, `:216-224`). Dispositions are decided in
`src/cursor-native-replay-routing.ts:20-31`, and two of the three outcomes are text:

- `transcript_trace` → the complete `formatCursorToolTranscript` output (multi-line, header +
  body + timing).
- `inactive_trace` → one `<title>: <summary>` line.
- `queue_replay` → the only card path.

`transcript_trace` fires when native replay is not requested/registered, or when there is no
live run, or when the tool is not in the active context tool set. Retained evidence shows the
result: 0 persisted tool cards and 60.7 % of Cursor thinking characters being tool output.

### N2 — The lifecycle line is never retracted, so every call has a double header

`formatCursorToolLifecycleProgressText` returns `"Cursor shell: <cmd>\n"`
(`src/cursor-tool-lifecycle.ts:88-94`). Completion only cancels the scheduled lifecycle line
(`cursor-provider-turn-coordinator.ts:200-205`, `cursor-provider-turn-sdk-normalizer.ts:57-92`);
if that correlation misses, the pending line stays in the thinking block. The completed
transcript then adds its own header, producing literal two-line duplication:

```
Cursor shell: which hc && hc --version 2>&1 | head -5
$ which hc && hc --version 2>&1 | head -5
```

Same command, twice, adjacent (real example E1 below).

### N3 — Transcript formatters are multi-view by construction

Every formatter prepends a synthetic header that repeats what the card title already says, then
the body, then a footer:

- `formatShell` → `$ <cmd>` + stdout/stderr + `Command exited with code N` + `Took N.Ns`
  (`src/cursor-transcript-tool-formatters.ts:221-232`; `getShellOutput` `:195-209`).
- `formatRead` → `read <path>` header (`:71-84`).
- `formatGrep` → synthesized `$ grep "<pat>" <path>` (`:403-407`, built by `:334-338`).
- `formatGlob` → synthesized `$ glob <pat> in <dir>` (`:257-262`).
- `formatWrite` → `Created N lines` + `File size: N bytes` + full file content (`:417-431`).
- `formatEdit` → `+N -N` stats + unified diff (`:433-447`).

Used as thinking text (N1) this is a log dump; used as card content it fights the card's own
title and summary.

### N4 — Shell progress and run summaries are additional thinking lines

`cursor-provider-turn-coordinator.ts:228-241` appends `Cursor shell stdout: …\n` for every
delta chunk, and `:243-249` appends `Cursor summary: …\n`. A long command therefore produces an
unbounded number of separate one-line thinking fragments, each truncated at 160/240 chars.

### N5 — Titles and summaries are flattened to one line

`truncateCursorDisplayLine` sanitizes first: it replaces all control characters and whitespace
runs with a single space (`src/cursor-display-text.ts:3-8`) before truncating at 240
(`:10-16`). The inactive and incomplete one-liners (`cursor-native-replay-trace.ts:26`,
`cursor-incomplete-tool-visibility.ts:134`) therefore destroy any multi-line shell command into
one 240-char line — unreadable, and not copy-pasteable.

### N6 — Display arguments are not the wire arguments

Card arguments are rewritten before they are persisted
(`cursor-provider-live-run-drain.ts:205-210` keeps exactly `tool.args`):

- `buildShellDisplayArgs` converts `timeout` ms → seconds
  (`src/cursor-transcript-tool-formatters.ts:211-219`).
- `buildGrepDisplayArgs` renames `caseInsensitive`→`ignoreCase`,
  `contextBefore`/`contextAfter`→`context`, `headLimit`→`limit`, and relativizes `path`
  (`:341-359`).
- `buildFindDisplayArgs` renames `globPattern`→`pattern`, `targetDirectory`→`path`
  (`:373-380`).
- `buildReadDisplayArgs` / `buildWriteDisplayArgs` relativize and inject `content`
  (`:86-108`).

Consequence beyond display: the dedupe fingerprint is `JSON.stringify({toolName,args})` over
these objects (`src/cursor-provider-turn-tool-ledger.ts:124-130`), so a started event and a
completed event whose display args differ in a cosmetic field do not match.

### N7 — The same text is stored twice, and the same metadata three times

`buildReplaySummaryDisplay` sets `result.content[0].text = contentText` **and**
`details.expandedText = details.expandedText ?? contentText`
(`src/cursor-transcript-tool-specs.ts:97-117`). `assembleCursorReplayActivityDetails` repeats the
fallback again (`src/cursor-replay-tool-details.ts:251-268`). The neutral card additionally
carries `activityTitle`/`activitySummary` **inside `args`** (`cursor-transcript-tool-specs.ts:84-96`)
*and* `title`/`summary` inside `details`. One logical string, four homes.

### N8 — A single tool name (`cursor`) multiplexes 14 activities, and unknown detail shapes fall back to raw text

`CURSOR_REPLAY_ACTIVITY_TOOL_NAME = "cursor"` (`src/cursor-tool-presentation-registry.ts:49`) is
the pi-facing name for delete, readLints, updateTodos, createPlan, task, generateImage, mcp,
semSearch, recordScreen, webSearch, webFetch, edit/write fallbacks, computerUse, and unknown
tools (`:176-300`). The renderer must dispatch on `details.variant`
(`src/cursor-native-tool-display-replay.ts:506-531`). When details are missing or unparseable the
renderer prints the raw content text (`:547`), i.e. the whole transcript as a card body.

### N9 — Collapsed activity previews are on by default and are large

`CURSOR_REPLAY_COLLAPSED_PREVIEW_LINES = 8`, 4000 chars, 240 chars per line
(`src/cursor-native-tool-display-replay.ts:41-42`), rendered whenever
`options.expanded || !details.collapseDetailsByDefault`
(`:425-456`, slicing at `:283-303`). Only web search/fetch opt out of preview
(`cursor-tool-presentation-registry.ts:289`, `:305`). So every plan/todo/MCP/subagent
card reaches into the transcript.

### N10 — Unknown and future Cursor tools dump scrubbed arguments

`formatFallback` prints `key=value, …` for up to 8 arguments, then truncates result text to 240
chars / 6 lines (`src/cursor-transcript-tool-formatters.ts:637-663`), and
`buildGenericPiToolDisplay` wraps that in a `cursor` card
(`src/cursor-transcript-tool-specs.ts:161-183`). For an unknown tool this is a raw blob by design.

### N11 — Incomplete starts become red error cards

`buildIncompleteCursorToolDisplay` builds `<title> did not complete` with `isError: true`
(`src/cursor-incomplete-tool-visibility.ts:81-96`), using the per-tool titles
`Cursor read` / `Cursor grep` / `Cursor find` / `Cursor ls` / `Cursor shell`
(`cursor-tool-presentation-registry.ts:120,126,132,138,145`). Fast local discovery is only
demoted to debug when the run also produced assistant text
(`cursor-incomplete-tool-visibility.ts:48-61`); otherwise a stale SDK start paints a red card
after a successful answer.

### N12 — Read replay hides the result and re-labels itself

If Cursor reports no content, the renderer substitutes a local file preview
(`src/cursor-transcript-tool-formatters.ts:41-55`, `:176-193`) and the card replaces its body
with a warning notice (`src/cursor-native-tool-display-replay.ts:551-575`). The call title gets a
suffix by re-rendering the base component at a **hardcoded width of 120**
(`src/cursor-native-tool-display-tools.ts:64-74`: `rendered.render(120)`), so in a narrow
terminal the suffix wraps away from the line it belongs to.

### N13 — Edit/write have two visually different detail variants for the same event

`buildEditPiToolDisplay` emits `variant: "nativeEdit"` when the recorded args satisfy pi's edit
schema and a `cursor` activity card otherwise (`src/cursor-transcript-tool-specs.ts:205-256`);
`buildWritePiToolDisplay` does the same (`:257-300`). Both branches duplicate the diff into
`diffString` and `diff` and the text into `expandedText`, and both are rendered by different
code paths (`cursor-native-tool-display-replay.ts:458-486` vs `:226-281`). The same edit can look
like a native card in one turn and a grey activity blob in the next.

### N14 — `summarizeCursorCompactToolCall` puts stdout/stderr into a "compact" summary

`src/cursor-compact-tool-summary.ts:47-51` returns `"$ cmd\n<stdout>\n<stderr>"`, so a
"compact" single-line summary is a multi-line shell dump wherever it is used (subagent
`conversationSteps`, compaction).

---

## 3. Why native Cursor tool activity can duplicate or appear as a raw blob

**D1 — There are two independent producers of the same pi-visible event.**
Cursor host tools are replayed by synthesizing `toolcall_*` events
(`src/cursor-provider-live-run-drain.ts:190-235`); pi bridge calls (`pi__*`) are executed as real
pi tools (`cursor-provider-live-run-drain.ts:252-285`). Suppressing the Cursor-side mirror of a
bridge call depends on recognizing it: `resolveCursorToolCompletion` only returns
`ignore-bridge` if the call id was previously marked by `markBridgeStarted`
(`cursor-provider-turn-coordinator.ts:189-193`) or if `run.bridgeRun.isBridgeMcpToolCall(...)`
matches (`cursor-provider-turn-sdk-normalizer.ts:95-100`). When either check misses — different
call id, resumed run, bridge not attached to this live run — the bridge call gets a **second**
card (`Cursor MCP`) on top of the real pi card.

**D2 — Correlation of started/completed pairs is fingerprint-based and refuses ambiguity.**
`removeStartedToolCallForStep` matches on `JSON.stringify({toolName,args})` and explicitly
refuses to join identical concurrent shells (`src/cursor-provider-turn-tool-ledger.ts:57-59`),
because a wrong join would be worse. The unjoined start then remains, and per N11 becomes a
`did not complete` card (or a trace), while the completion renders its own card: one real call =>
two visible entries.

**D3 — Dedupe keys are non-canonical.**
`shouldSkipDuplicateCompletion` dedupes on `identity` (a call-id string) or on the *display*
fingerprint (`cursor-provider-turn-tool-ledger.ts:69-92`). Because display args are rewritten
(N6), an event pair that is the same call can differ in `timeout`/`ignoreCase`/`path` form and
avoid dedupe; conversely two genuinely distinct calls with the same display args dedupe to one.

**D4 — Registration conflicts silently downgrade a name to a text trace.**
`registerNativeCursorToolsFromSet` skips any name another extension already owns and notifies
once (`src/cursor-native-tool-display-registration.ts:66-84`). For the skipped name,
`canRenderCursorToolNatively` is false, so `resolveNativeReplayDisposition` returns
`transcript_trace` (`cursor-native-replay-routing.ts:24-26`) — within one turn `read` renders as
a card and `bash` as a wall of thinking text.

**D5 — Disabling pi tools turns card rendering off wholesale.**
`syncRegisteredNativeCursorToolsForModel` returns early when pi tools are disabled
(`src/cursor-native-tool-display-registration.ts:111`), so the replay tools are registered but
never activated; the context tool set no longer contains them and every call routes to
`inactive_trace`/`transcript_trace` (`cursor-native-replay-routing.ts:27-31`). `--no-tools`,
`--tools`, and `--exclude-tools` are ordinary user actions — the extension's visual quality is
coupled to them even though it is documented as display-only.

**D6 — The raw blob is the documented fallback, not a bug.**
`docs/cursor-native-tool-replay.md` ("Unknown/future fallback path", "SDK reporting limits")
specifies bounded scrubbed argument/result text for unknown tools and `transcript_trace` for
everything the wrapper cannot render. Retained sessions show the extreme case: 13 Cursor
assistant messages, 0 `toolCall` parts, and the shell history living inside `thinking`.

**D7 — `pi__*` compounds the confusion.**
The bootstrap manifest enumerates every exposed bridge name
(`src/cursor-tool-manifest.ts:34-47`, wired at `cursor-provider-turn-prepare.ts:325-336`), and the
model then repeats those names in its prose — e.g. a bullet list of
`pi__TaskCreate`/`pi__TaskExecute`/`pi__bg_run` (real example E5). Those are model-authored text,
never cards (pi renders the real bridge tool name, `cursor-provider-live-run-drain.ts:252-285`),
but a reader sees tool-looking identifiers in the transcript with no card attached.

**D8 — Multi-surface width of one SDK callback.**
A single completed shell call can simultaneously produce: a pending thinking line (N2), streamed
progress thinking lines (N4), a summary thinking line (N4), a replay card, and a transcript
trace. Each producer has its own defer/scrub/truncate policy
(`cursor-tool-lifecycle.ts:10`, `cursor-provider-turn-display-router.ts:75`,
`cursor-transcript-utils.ts:35-40`), and none of them knows what the others emitted.

---

## 4. What pig's own renderer does with each payload shape

| Payload shape | pig behavior |
| --- | --- |
| assistant `text` | Rendered as assistant text, in content order. |
| assistant `thinking` | Rendered as thinking. The schema has only `thinking`, `thinkingSignature`, `redacted` — there is no "tool origin" marker (`message-types.md:47-56`), so extension-injected tool output and real reasoning share one visual channel and one collapse state. |
| assistant `toolCall` | Looked up by `name` in the registered tool set, then `renderCall(args, theme, context)`. Arguments are exactly what was persisted; the drain deliberately keeps the JSON identical to the wire delta (`cursor-provider-live-run-drain.ts:205-210`). |
| `toolCall` with `id` starting `cursor-replay-` | Intercepted by the wrapper (`src/cursor-native-tool-display-tools.ts:187-201`); for `read`/`edit`/`write` a custom call renderer replaces the base one; for `bash`/`grep`/`find`/`ls` the base builtin renderer is used unchanged. |
| `toolResult.content` | The only part the model sees; base builtin renderers draw it (path header, output body, diff). |
| `toolResult.details` | Renderer-only, model never sees it (`message-types.md:182`). For name `cursor` the renderer switches on `details.variant` (`src/cursor-native-tool-display-replay.ts:506-548`); `nativeEdit`/`nativeWrite` get bespoke summaries; `activity` gets title + summary + up to 8 preview lines; missing details print the raw text. |
| `isError: true` | Error styling on the card. The extension sets it for replay failures and for every `did not complete` card (`cursor-incomplete-tool-visibility.ts:96`), so SDK bookkeeping failures look like tool failures. |
| empty assistant message (`content: []`) | Renders nothing, but still occupies a turn boundary (observed shape in Cursor runs, e.g. `2026-11-11T14-56-38-261Z_01a1cb1c…jsonl` record 12, an error turn with 0 parts). |
| session `custom` entries | Excluded from model context and not rendered (`session-format.md:140-143,193`); they only inflate JSONL. |
| `ui.notify` warnings | Transient banner, unrelated to the card that follows. |

The practical consequence: pig is a faithful renderer. It draws exactly one card per
`toolCall` + `toolResult` pair and one thinking block per `thinking` part. **All reported noise
originates in what the extension chooses to emit, not in how pig draws it.** A rewrite that emits
fewer, cleaner parts gets a clean terminal for free.

---

## 5. Minimal rendering rules for the rewrite

Rules are ordered by impact. Each one removes a specific source above.

1. **One host tool call = one card, and nothing else.** Never mirror a tool call into thinking.
   Removes N1, N3, D6.
2. **Thinking is model reasoning only.** No lifecycle lines, no progress lines, no run summaries,
   no `Cursor activity trace` channels, no 50 000-char cap because there is nothing to cap.
   Removes N2, N4, D8.
3. **Drop the lifecycle prefix entirely.** If a pending indicator is required, it is the card's
   own title with a spinner, not an extra message part. Removes N2.
4. **No synthetic headers in card bodies.** The card title already carries the tool and its
   subject; the body is stdout/stderr, file content, or the diff — nothing else. No `$ cmd`
   echo, no `Took N.Ns`, no `Command exited with code N` chrome beyond a one-word error state,
   no `read <path>` / `write <path>` header repetition. Removes N3.
5. **Persist display args equal to wire args.** If a renderer needs a different unit or a
   derived field, derive it at render time. Removes N6 and half of D3.
6. **Store each string once.** `content` is what the model saw; `details` carries only
   structured, non-textual fields (`path`, `diff`, `linesAdded`, `linesRemoved`, `imagePath`).
   No `expandedText` that duplicates `content`, no title/summary in both `args` and `details`.
   Removes N7.
7. **Prefer host tool names over a neutral catch-all.** Emit `read`/`bash`/`grep`/`find`/`ls`/
   `edit`/`write` when the recorded args satisfy the host schema, and emit **nothing** when they
   do not (or a single `cursor-activity` line). Do not invent a `cursor` tool that multiplexes 14
   shapes. Removes N8, N12, N13.
8. **No collapsed preview body by default.** Summary line only; the body appears on expand.
   Removes N9.
9. **No argument dumps for unknown tools.** Emit a one-line `<name>` activity with no args and no
   result text, or suppress. Removes N10.
10. **Incomplete starts are not errors.** Suppress them, or aggregate them into one muted note
    at the end of the turn. Never `isError: true` unless Cursor reported an error. Removes N11,
    D2's visible half.
11. **Bridge calls produce exactly one card, keyed by call id.** Correlate by the id Cursor
    supplies, carry it through, and drop the Cursor-side `mcp` completion unconditionally when the
    id matches a bridge tool call — do not depend on name lists or fingerprints. If the id is
    unknown, drop the `mcp` card rather than risk a duplicate. Removes D1.
12. **One producer per visual event.** Pick either the card or the trace, decided once per call,
    before any text is appended. The current design decides late, per event type, in five
    different modules. Removes D8.
13. **Render at the width pig passes in.** No `render(120)` re-rendering to append a suffix
    (`src/cursor-native-tool-display-tools.ts:67`). Removes N12.
14. **Do not couple display to tool-registry state.** Rendering must not silently degrade because
    `--no-tools` is set, another extension owns a name, or the context tool set excludes it. If a
    card cannot be drawn, draw the same compact line — never a transcript dump. Removes D4, D5.
15. **Keep `pi__*` out of prose.** Do not enumerate bridge names in the system prompt unless the
    model must call them; the manifest is a display/readability cost paid on every turn. Removes
    D7.

### What not to carry over

- The three-way disposition machine (`queue_replay` / `inactive_trace` / `transcript_trace`).
- The `cursor` catch-all tool name.
- `expandedText`, `activityTitle`/`activitySummary` inside arguments, and `diff` + `diffString`
  duplication.
- The lifecycle/progress/summary thinking emitters.
- `summarizeCursorCompactToolCall`'s multi-line "compact" shell summary.
- Fingerprint-based dedupe.

---

## Appendix — real session excerpts

Corpus: `~/.pig/agent/sessions/`. Record numbers are 0-based line indices in each
JSONL file. Strings truncated at ~200 chars.

**E1 — lifecycle line + transcript in one thinking block.**
`--Users-saintno-.pig-agent--/2026-11-11T11-23-58-813Z_01a1ca59-b3dd-779f-8aad-8dd13f48ffb3.jsonl`,
record 40, `content[1].type == "thinking"`, provider `cursor`:

```
Cursor shell: which hc && hc --version 2>&1 | head -5
$ which hc && hc --version 2>&1 | head -5

~/.local/bin/hc
hc 0.1.20

Took 0.4s
```

The command appears twice, the body and timing are tool output, and all of it sits in the
thinking channel. No `toolCall` or `toolResult` record exists for this call.

**E2 — redacted environment dump as thinking.**
Same file, record 49, `content[1]`:

```
Cursor shell: env | grep -i '^PI_' | sed 's/\(TOKEN\|KEY\|SECRET\)=.*/\1=<redacted>/' | sort
$ env | grep -i '^PI_' | sed 's/\(TOKEN\|KEY\|SECRET\)=.*/\1=<redacted>/' | sort

PI_CODING_AGENT=true

Took 1.0s
```

**E3 — 9 298-character raw blob.**
Same file, record 49, `content[5]`, one thinking part containing a full process-tree dump:

```
Cursor shell: ps -o pid,ppid,command -p $$ ; echo '--- parents ---'; p=$$; for i in 1 2 3 4 5; do p=$(ps -o ppid= -p $p | tr -d ' '); ps -o pid,ppid,command -p $p | tail -1; done
$ ps -o pid,ppid,command -p $$ ; echo '--- parents ---'; …

  PID  PPID COMMAND
88410 86896 /bin/zsh -c IFS= builtin read -r -d '' __cursor_command <&5 2>/dev/null || { …
```

**E4 — one assistant message, eight parts, traces interleaved with reasoning.**
Same file, record 49 (`provider: cursor`) has eight content parts:
`thinking(reasoning, 269)` → `thinking(Cursor shell env, 207)` → `thinking(reasoning, 398)` →
`thinking(Cursor shell ps, 666)` → `thinking(reasoning, 219)` → `thinking(Cursor shell ps tree,
9298)` → `thinking(reasoning, 1440)` → `text(answer)`. Seven thinking blocks, three of them tool
output, alternating with reasoning, for three shell commands in one turn.

**E5 — `pi__*` names inside model prose.**
Same file, record 31, `content[2].type == "text"` (truncated):

```
Short answer: no — not all of pig's tools, but nearly all of the ones pig exposes through this run's bridge.

**Callable right now**

*Pi bridge (`pi__*`):*
- Tasks: `TaskCreate`, `TaskList`, `TaskGet`, `TaskUpdate`, `TaskExecute`, `TaskOutput`, `TaskStop`
- Background/long work: `bg_run`, `bg_status`, `bg_logs`, `bg_result`, `bg_kill`, `bg_delegate`…
```

Tool-shaped identifiers with no cards attached — an artifact of the bootstrap manifest.

**E6 — a healthy Cursor turn that persists no cards at all.**
`--Users-saintno-Sources-Lab-pi-cursor-sdk-go--/2026-11-11T14-57-27-757Z_01a1cb1d-26cd-7784-96b2-919117fdc414.jsonl`,
record 13 (provider `cursor`, `api: cursor-sdk`), content = `[thinking, text]` only, while record 7
is a `custom` entry of type `pi-cursor-sdk:usage-v1` carrying `agentId`, `modelSelection`,
`storeIdentity`. Text-only turns are clean; every turn that touches a tool produces the shapes in
E1-E4 instead of a card.

**E7 — corpus totals from §0, restated for the record.**
13 `provider: cursor` assistant messages across 9 sessions; 18 thinking parts; **0 toolCall
parts**; 10 318 / 16 991 thinking characters (60.7 %) are injected tool traces.
