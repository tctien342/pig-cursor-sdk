# Native Cursor Model Feel Implementation Plan

> **For agentic workers:** Use todo tools to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement continuity, native tool fidelity, scoped cancellation, model controls, and /cursor-doctor diagnostics with focused evidence.

**Architecture:** Preserve the existing callback/native pig tool boundary. Use complete transcript identities and per-session turn serialization; cancellation addresses an owned run, never every engine branch. Keep model parameter resolution pure and parameter-aware. Diagnostics read safe snapshots and probe only an already-running bridge when explicitly requested.

**Tech Stack:** Go, pig extension SDK, Connect local fake services, Python PTY/pyte.

## Global Constraints
- Run only tests relevant to the changes, never an unrelated full-repository suite.
- Do not fake Cursor-internal activity as executable pig tools.
- Do not resend a prompt after ambiguous delivery or claim remote cancellation without evidence.
- No locks across network or host calls; no credentials/prompts/tool output in diagnostics.
- Mark each user-requested task complete only against concrete implementation and tests.

### Task 1: Continuity
**Files:** provider picontext.go, engine.go, new continuity_test.go.
**Interfaces:** Session.appendedText remains (string,bool); full Message.Fingerprint; transient session turn lock.
- [x] Hash complete canonical content, including images, arguments and error state, without prefix truncation.
- [x] Treat image-only new user messages as append-only, but do not mistake the previous user request for a new one.
- [x] Preserve rollback bookkeeping on immediate send failure without automatically resending ambiguous requests.
- [x] Test image-only, changed image tail, text edits beyond 4096 bytes, rewritten branch and unchanged-prefix turns.

### Task 2: Cancellation
**Files:** cursor run.go/new cancellation helpers+tests; provider scoped cancellation helpers; extension command.
**Interfaces:** context-aware per-run cancellation, run status snapshot, AbortScope by session file+cwd+ephemeral nonce, not cwd alone.
- [ ] Make /cursor-abort operate only on the current logical session and report local stop versus upstream failure accurately.
- [ ] Serialize session pumps while allowing cancellation to reach the active owned run without waiting for the pump lock.
- [ ] Stop/Close release active work with bounded RPCs outside engine locks; propagate Escape/caller cancellation.
- [ ] Test two sessions in one cwd, failed cancellation/retry, already completed runs, callbacks released and concurrent teardown with race checks.

### Task 3: Tool fidelity
**Files:** cursor callback result conversion and provider tool-outcome parsing/tests.
- [ ] Preserve actual native pig tool text and isError, including empty successful results, without turning an ordinary tool error into a callback transport failure.
- [ ] Verify bridge interpretation of result fields before emitting new structured/image shapes; explicitly report unsupported features rather than pretending parity.
- [ ] Keep native cards and result/expansion transcript data untouched; test error, cancellation and replay paths.

### Task 4: Model controls
**Files:** provider catalog.go, controls.go, controls_test.go; engine wrapper integration; extension option precedence.
**Interfaces:** resolvedParams(entry,override) and canonicalParamSignature(params), explicit per-level parameter changes.
- [ ] Fix off across boolean thinking+effort, extra-high aliases, none/off, and explicit option precedence.
- [ ] Fix explicit context versus generic cache precedence, fast-mode window equivalence, canonical signatures and default-context aliases.
- [ ] Assert actual outgoing params for embedded/synthetic models; preserve defaults and clearly report unsupported controls.

### Task 5: Diagnostics
**Files:** provider doctor.go/tests; extension /cursor-doctor; docs/evidence.
**Interfaces:** passive diagnostic snapshot plus opt-in bounded probe of an existing client.
- [ ] Report active tools/count, bridge state, selected model/effective controls, current scope run state, recent reconnect/cancel failures and actionable next steps.
- [ ] Default diagnosis must not start a bridge, create agents, read credentials or emit prompts/private tool arguments.
- [ ] Test idle/missing bridge, live run, failure states, redaction and zero-start behavior.
- [ ] Run focused suites, selected race checks, build/vet and scoped CLI/PTY checks, review and commit each completed area.
