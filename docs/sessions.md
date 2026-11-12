# Two sessions in one directory

A Cursor agent belongs to one conversation, and Cursor keys its own agent store by
directory: `~/.cursor/projects/<cwd>/sdk-agent-store`. This extension adds
`~/.pig/agent/pig-cursor-sdk/agents.json`, mapping a conversation to the agent id
that holds it, so a resumed session does not have to replay its transcript.

That is two kinds of sharing at once: durable identity (useful) and live execution
(not shareable). Nothing about an agent tolerates two drivers — Cursor answers the
second one with `already has active run`, and whichever process exits calls
`CloseAgent` on an agent the other is still using.

## Measured, before any of this

Two `pig --no-session` runs in one directory, both asked for a turn:

```
level=INFO msg="restored cursor agent still holds an upstream run; starting a fresh one"
  err="cursor run stream: Agent c8c7d38a-… already has active run (unknown)"
```

Same agent id in both processes. Six of these in one afternoon's log, and the same
agent id twice in a row — closing it did not release the run. The session that
survived was fine, but only after throwing away Cursor's side of the conversation
and replaying every message.

Three causes, three fixes.

## 1. An ephemeral session has no identity to share

With `--no-session` there is no session file, so the conversation key was
`("" , cwd, model)` — identical for every throwaway run in that directory. The key
now falls back to a per-process nonce: two unrelated runs cannot land on one agent,
and one run keeps its own agent across its turns.

`conversationKey` in `internal/provider/engine.go`; `TestConversationKeyKeepsUnrelatedSessionsApart`.

## 2. An agent is claimed by the process that holds it

`agents.json` records the owner's pid **and the start time `ps` gave it**, because
a pid is a recycled number: a bare liveness check would credit some unrelated
process with owning the agent, or let us signal it.

- owner alive and the same process → the record is a claim. A second session on the
  same session file logs `another live session owns this conversation's cursor
  agent; using a separate one` and builds its own. Losing resume is cheap; one of
  the two sessions breaking is not.
- owner gone, or its pid recycled → the record is free, and resuming is the right
  thing.
- records written before claims existed stay resumable, so existing installs do not
  regress.
- the process's own pid never counts as an owner: `/reload` closes the running
  engine's agents and builds a new engine in the same process, which must still be
  able to resume.

`internal/provider/state.go`; `TestAgentRegistryRefusesAnAgentAnotherProcessHolds`.

## 3. A killed session's bridge is reaped, not inherited

`Handle.Close` shuts a bridge down properly, but SIGKILL runs no handler. The
bridge survives — its port, its memory, and upstream a run that keeps the agent
busy for whoever comes next. Every spawn now records the bridge in
`bridges.json` with its owner's pid and start time; the next spawn (and only then)
kills the bridges whose owner is provably gone:

```
level=INFO msg="reaping a cursor bridge whose session is gone" bridge=47735 session=47763
level=INFO msg="reaped bridges left by dead sessions" count=2
```

Ownership is proven the same way as above, and a pid whose command line no longer
matches the bridge binary is dropped from the file rather than touched. On hosts
without `ps`, nothing is reaped and nothing is signalled.

`internal/bridge/reap.go`; `TestReapPlanOnlyKillsBridgesWhoseOwnerIsGone`.

## A run left active on your own agent

A turn can end while Cursor is still working — the user sends another message, the
turn is interrupted, or a deadline passes. The run object stays alive, so the next
`Send` on that agent used to fail immediately:

```
cursor: run  is still active for this agent
```

An error, no answer, and nothing to retry: the turn was dead before it started.
Sending is now preceded by releasing that leftover run
(`Agent.supersedeActiveRun`), which is the only outcome that can help — the bridge
would refuse the send anyway, and the user's new message is the newer intent. Both
wordings of the blockage (`already has active run` from the bridge, `is still
active for this agent` from this package) take the same recovery path: cancel what
is live on the agent, then try the same agent again, so Cursor's side of the
conversation survives; only if that fails is the agent rebuilt and the transcript
replayed.

A brand-new agent whose first send fails is dropped rather than kept: it never
received the conversation, and keeping it would make the retry send only the newest
message into an empty agent.

`dev/tty-interrupt.py` sends a second message while Cursor is running a 30-second
shell command and asserts the answer arrives with no error shown.

## When the leftover run is still there anyway

If a resumed agent still answers `already has active run`, the extension asks
Cursor for the agent's runs and cancels the ones still live
(`Agent.ReleaseStaleRuns`), then sends again on the **same** agent — so the
conversation Cursor holds survives. Only when that is impossible (no `ListRuns`,
nothing live to cancel) does it fall back to a fresh agent and a replay.

This is safe precisely because of fix 2: this process only ever resumes an agent
nobody is holding, so a run on it belongs to a dead session, never to a live turn
of another window.

## Check

```
dev/.venv/bin/python dev/tty-two-sessions.py
```

Three scenarios in a real terminal, with a session that stays open throughout: the
other one exits cleanly, the other one is killed mid-tool-call, and the other one
opens the same session file. It fails if the open session stops answering, and
prints what every process logged.
