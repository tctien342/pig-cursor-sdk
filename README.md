# pig-cursor-sdk

Cursor models inside [pig](https://github.com/MichaelKinsy/PiG), driven by a pure-Go
provider that talks to the official [Cursor SDK bridge](https://github.com/cursor/sdk-bridge)
(`sdk.v1`) over Connect. It replaces the TypeScript `pi-cursor-sdk` extension: same
capabilities, no JavaScript in the per-turn path, and a tool-call display that reads
like the rest of pig instead of a protocol dump.

```
pig ── streamSimple ──►  pig-cursor-sdk (Go)  ── Connect ──►  cursor-sdk-bridge (Bun) ──► Cursor
                              ▲                                      │
                              └──── tool callback (parked) ──────────┘
```

## Install

```bash
pig install git:github.com/tctien342/pig-cursor-sdk
```

pig clones the repo and builds the Go extension. It needs a Go toolchain; if
`go` is missing or older than 1.26, `pig setup go` installs one into
`~/.pig/toolchains/go` and pig uses it automatically. From a checkout, the
source is the package: `pig install .`.

The bridge binary is Cursor's, not ours, so it is fetched separately — platform
detected, latest [`cursor/sdk-bridge`](https://github.com/cursor/sdk-bridge/releases/latest)
release, SHA-256 verified against that release's `SHA256SUMS.txt` (it refuses an
unverified binary):

```bash
curl -fsSL https://raw.githubusercontent.com/tctien342/pig-cursor-sdk/main/scripts/install-bridge.sh | bash
```

which installs to the path the extension looks in:

```text
~/.pig/agent/pig-cursor-sdk/bin/cursor-sdk-bridge
```

Search order is `CURSOR_SDK_BRIDGE_BIN` → that path → next to the extension
binary → `PATH`, so a bridge already present from Cursor's Python wheel is found
too. It is a self-contained Bun single-file executable — **no Node.js and no
system Bun are used anywhere**; verified by running a full agent turn with
`PATH=/usr/bin:/bin`.

Then give it a key and switch:

```text
pig
/cursor-login        # paste a key from cursor.com/dashboard -> API Keys
/model               # pick cursor/grok-4.7
/cursor-status       # bridge pid + protocol, catalog age, tool mode
```

`CURSOR_API_KEY` works as well, and a key stored by `pig login --provider cursor`
is used automatically. Cursor Desktop's subscription login does **not** provide a
key usable here. Missing pieces say so plainly: no key → run `/cursor-login`; no
bridge → the exact path to put it at.

Verified from a clean clone: `pig install --validate-only
git:github.com/tctien342/pig-cursor-sdk` → valid, provider `cursor`, five
commands; `scripts/install-bridge.sh` → `v1.2.0`, checksum verified, and a real
turn served by that freshly downloaded bridge.

## Usage

```bash
pig --model cursor/grok-4.7 -p "…"
pig --model cursor/claude-opus-5-5@1m:fast -p "…"
```

Model ids are `cursor/<base>[@<context>][:fast|:slow]`, built from the live Cursor
catalog (`/cursor-refresh-models` re-reads it without restarting). Thinking level
maps onto the model's own reasoning parameter.

| command | what it does |
| --- | --- |
| `/cursor-refresh-models` | re-read the catalog through the bridge, cache it, re-register |
| `/cursor-mode plan\|agent` | Cursor plan mode vs agent mode |
| `/cursor-abort` | release the Cursor run live in *this* pig process (completed host work is not undone) |
| `/cursor-usage` | tokens and cost Cursor reports for this session's agents |
| `/cursor-status` | bridge pid, catalog size, mode, key presence, tool mode, usage |
| `/cursor-login` | store an API key |

Environment: `CURSOR_SDK_BRIDGE_BIN` (bridge path), `PI_CURSOR_TURN_TIMEOUT`
(default `5m`), `PI_CURSOR_TOOL_BATCH_GRACE_MS` (how long to keep reading after a
host tool call to catch a batched turn, default `900`), `PI_CURSOR_GO_DEBUG=1`
(debug logging to `~/.pig/agent/pig-cursor-sdk/agent.log`),
`PI_CURSOR_SETTING_SOURCES`, `PI_CURSOR_FOOTER=0`, `PI_CURSOR_SHOW_TOOLS=0`.

## Usage in the footer

The status line carries what the account has spent, refreshed when a turn ends
(never polled, and never on the request path — a slow usage RPC cannot delay an
answer):

```text
cursor · grok-4.7 · plan 7% · included used · 24.1k tok · pid 41821
```

Numbers come from Cursor's own `GetUsage` for the agents this extension created:
token totals plus cost in cents, shown as charged when Cursor reports a charged
amount and as `… raw` when it only reports undiscounted model cost (plan-included,
BYOK, and grant accounts report no charged figure, so the label matters).
`/cursor-usage` gives the in/out/reasoning split and the run count.

### Plan usage: "how much of my allowance is left"

This one needs Cursor Desktop, and it is worth being exact about why.

An API key cannot ask about limits. Measured on a real account:

| source | result |
| --- | --- |
| `GetUsage` for a local agent | `not_found: Unknown agent: agent-c8c7d38a-…` — the call is cloud-only |
| `ListAgents` for the key | 0 agents, because local agents live in the bridge store, not the cloud |
| `aiserver.v1.DashboardService/GetCurrentPeriodUsage` with the API key | `401 ERROR_NOT_LOGGED_IN` under every key-auth form |
| same call with Cursor Desktop\'s login token | `200` |

So when Cursor Desktop is installed **and logged in on this machine**, the
extension reads the plan through that login and the footer gains a real
allowance line:

```text
 cursor ▰▰▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱▱ 7.1% · included used · $55.13 bonus · resets 30 Nov
```

That row is a widget above the editor, so the built-in footer (model, tokens,
branch) is untouched. It fills in over ~0.8 s when a fresh reading lands and then
settles on the honest value; it never redraws on its own after that. The bar width
follows the terminal (6 cells at 40 columns, 20 at 110+), the row is truncated
rather than wrapped, and it appears only for `cursor/*` models — switching away
removes it. `PI_CURSOR_PLAN_BAR=0` keeps the plain status-line form instead
(`plan 7% · included used · $55.13 bonus`), and while the bar is on screen the
status line drops the duplicate text.

`/cursor-usage` prints the whole picture:

```text
plan: Team ($40/mo) · used $20.00 of $20.00 included + $55.13 bonus ($75.13 total)
cycle: 30 Oct → 30 Nov · 7.08% used (Cursor's own figure)
account: you@example.com (via Cursor Desktop login)
```

**It turns itself on and off.** On `turn_end` the extension checks locally — one
`stat` plus one indexed key read, measured at 3 ms — and only contacts Cursor if
Cursor Desktop is there with a token. No Cursor install, or a logged-out one, and
no plan line ever appears; `/cursor-usage` then says which case it is:

```text
plan: not read — Cursor Desktop is not installed on this machine
plan: not read — Cursor Desktop is installed but logged out (sign in inside the Cursor app)
```

Never a zero. An unknown is shown as an unknown, because "no data" and "you are
out of usage" must not look the same.

What it touches, plainly: one read-only SQLite query for
`cursorAuth/accessToken` inside Cursor\'s own `state.vscdb` (opened `mode=ro`,
`query_only`, 2 s busy timeout, so a running Cursor is unaffected), the token held
in memory only, never written to a log, the transcript, or any file. The limit
figures come from Cursor\'s app API — not a published contract, so fields can move;
a parse or HTTP failure means no line, not a wrong one.

| env | effect |
| --- | --- |
| `PI_CURSOR_PLAN_USAGE=0` | never read the plan, even with Cursor present |
| `PI_CURSOR_PLAN_TTL_S` | re-read interval, default `300` |
| `PI_CURSOR_STORE_PATH` | use a different Cursor store (tests, non-standard install) |

Cost: the two HTTP calls took 1.8 s end to end, off the request path — a turn is
never waiting on them. `cd dev && go run ./cmd/planprobe` prints the numbers and the timings
without starting pig.

### Limits from the protocol itself

`sdk.v1` carries limit data only inside a refusal: `SdkErrorDetails.rate_limit`
(`limit`, `remaining`, `reset_epoch_seconds`). That is captured the moment Cursor
reports one and kept until the window resets, footer and error text both:

```text
cursor run stream: usage limit exceeded — Cursor usage window: 0 of 500 left · resets in 2h05m (21:05)
```

An absent window means *Cursor has not told us*, never *you have headroom*.

## How tool calls are displayed

The host owns tools. This extension advertises pig's **own active tools and
schemas** to Cursor, so when Cursor calls `bash`, pig executes `bash` and renders
its own card, with its own hooks and permissions. Nothing about a tool call is
ever written into the thinking channel.

Cursor's **own** tools appear inline as plain text, exactly between the assistant
blocks surrounding the call's start event:

```text
I'll inspect the diff.

◇ cursor: shell · git diff --stat

Now I'll check the files.
```

Only the currently running call appears above the editor as a compact hint.
Completed calls stay inline, never as a list beside the usage bar. New inline
annotations are ordinary assistant text, without native card backgrounds or
ctrl+o expansion; they may be visible to other models and compaction. Generated
blocks have a namespaced text signature so Cursor's replay can omit them without
removing real assistant prose. Old custom-entry tool cards still redraw on resume.

The run parks on an open bridge callback while pig runs the tool; the callback is
resolved exactly once from the next request's `toolResult`, and the run continues
on the durable `ObserveRun` stream. Full rules and how they are enforced:
[docs/display-rules.md](docs/display-rules.md).

Before (measured over 132 retained sessions in `docs/evidence/current-display-audit.md`):
60.7% of Cursor thinking characters were injected tool traces and **zero** tool
cards were persisted. After: [docs/evidence/tty-session.txt](docs/evidence/tty-session.txt)
— one `$ echo …` card, real output, reasoning-only thinking.

## Benchmarks

Same pig build, same prompt, 3 runs each (see `dev/bench.sh`):

| metric | this extension | `pi-cursor-sdk` 0.5.26 |
| --- | --- | --- |
| `pig --list-models cursor` | 0.29s | 0.44s |
| first host event | 0.30–0.33s | 0.45–0.52s |
| first streamed token | 8.05–10.07s | 9.80–10.54s |

First token is dominated by Cursor's own round trip; the extension's own cost is
where the win sits.

## Layout

```
extension.go, support.go   pig factory: provider registration, auth, /cursor-* commands, catalog
internal/bridge            bridge spawn, ready-line handshake, Connect framing, shutdown
internal/cursor            typed sdk.v1 client, run coordinator, parked host tool callbacks, event decoder
internal/provider          model catalog, pig transcript parsing, turn engine, display emitter
internal/planusage         plan allowance through Cursor Desktop's login: read-only, opt-out
proto/, gen/               sdk.v1 protos (vendored) and their generated Go
docs/display-rules.md      the display contract, enforced by tests
internal/provider/bar.go   the plan bar: fill, animation cap, width fitting
internal/provider/cursortools.go  Cursor's own tool calls: one transcript line each
internal/bridge                    bridge spawn, shutdown, and the orphan sweep (reap.go)
docs/evidence/             audit, live event capture, terminal capture, bridge resume log
dev/                       live/multi-turn/PTY/reload/plan runners, probes, benchmark — its own Go module
scripts/gen-fallback-catalog.py  refresh the embedded offline catalog snapshot
```

## One rule about layout

Nothing under the extension root may declare `package main`. pig refuses to load a
directory that holds both a factory and a standalone main package
(*"contains both factory and standalone main packages; select one exact root"*), so
probes and one-off tools live in `dev/`, which is a separate module. The layout is
asserted by `TestNoMainPackageInExtensionRoot`, because otherwise the only symptom
is everyone else's extension failing to start.

## Verification

```bash
go test ./...                     # 45 tests: coordinator, decoder against real frames, display rules, state locking
pig install . --validate-only --json
./dev/run-live.sh cursor/grok-4.7 "Use the bash tool to run exactly: echo hello-go. Then tell me what it printed."
dev/.venv/bin/python dev/tty-reload-check.py   # chat, /reload, and a turn after reload — fails on a hang
./dev/bar-screen.sh               # the bar, read back from a terminal emulator's screen
python3 dev/tty-check.py          # interactive PTY capture -> docs/evidence/tty-session.txt
./dev/run-edge.sh                 # several tool calls in one turn; no orphan bridge after a killed host
dev/.venv/bin/python dev/tty-inline-tools.py  # inline order, one running hint, saved metadata and resume
dev/.venv/bin/python dev/tty-two-sessions.py  # sessions in one directory: one exits, the rest survive
dev/.venv/bin/python dev/tty-interrupt.py    # a message sent while Cursor is working still gets an answer
./dev/plan-check.sh               # plan readout with Cursor present, and with it absent
cd dev && go run ./cmd/planprobe  # plan numbers + timings, without starting pig
```

`tty-reload-check.py` exists because this extension's failure mode was silence: the
host waits on a handler forever, so a "did it work" check cannot be a sleep and a
grep. It waits on the extension's own log and on text in an emulated screen, and it
prints the final screen and a PASS/FAIL line. The evidence from the run that fixed
the hang is in [docs/evidence/tty-reload.txt](docs/evidence/tty-reload.txt).

## Known limitations

- Model aliases the TypeScript extension invented (`auto`, `composer`,
  `fable-5-2@1m`) are not part of `sdk.v1`; only canonical Cursor ids are offered.
- `/cursor-abort` releases the run this pig process is holding. A run left active
  by a process that died is not cancelled eagerly: the next send notices the agent
  still owns an upstream run, releases it, and starts a clean one (logged at INFO).
- Plan limits depend on Cursor Desktop: an API key gets no limit data at all, so
  the allowance line exists only where Cursor is installed and logged in, and
  comes from Cursor's app API rather than a published contract (see *Plan usage*).
  Rolling 5-hour windows are not exposed by either surface — only the billing
  cycle is.
- Cursor-internal tools are inline text annotations, not executable pig tools.
  They show the command at its event position, not tool output. Other providers
  can see those lines; Cursor replay omits exactly marked display blocks. Disable
  annotations and the single running hint with `PI_CURSOR_SHOW_TOOLS=0`.
- Several pig sessions in one directory each need their own Cursor agent — Cursor
  tolerates exactly one driver per agent — so only one of them resumes Cursor's
  side of a conversation and the others replay it. Windows on the *same* session
  file therefore have separate Cursor-side memories even though the transcript is
  shared. Why, and what gets reaped when a session is killed:
  [docs/sessions.md](docs/sessions.md).
- Cost is reported as an estimate of zero with `totalFromProvider: false`: Cursor
  publishes token counts, not a per-run dollar amount.
- Cursor's own account usage/quota is not exposed by `sdk.v1`, so there is no
  quota readout to show.

## The hang: one mutex, taken twice

pig froze on every chat and on `/reload` with this extension loaded. Nothing was
logged, nothing errored, and the model stayed selectable; the process just stopped
answering. Two goroutine dumps (`kill -QUIT`) named it exactly:

```
goroutine 102  turn_start handler
  syncPlanBar          extension.go:776   [sync.Mutex.Lock]     <- wants state.mu
goroutine  53  deferred worker
  setPlanPct           extension.go:950   [sync.Mutex.Lock]     <- wants state.mu
```

`syncPlanBar` took `state.mu` and then called `barShown()` and `engineRef()`, which
take `state.mu` themselves. Go's `sync.Mutex` is not reentrant, so the handler never
returned. pig was blocked in `subprocess.Conn.request` waiting for the handler's
result, the extension never answered another request, and every later turn and
`/reload` queued behind it. `loadCatalogLocked` had the same shape (it called the
locking `configHomeSDK`), hidden only because `session_start` fills `ConfigHome`
first.

Rules that came out of it, both enforced by tests in `state_lock_test.go`:

1. **Never take `state.mu` twice on one path.** A helper that assumes the caller
   holds the lock is named `...Locked` and takes no lock of its own; everything else
   locks. `TestPlanBarStateReadsDoNotSelfLock` and
   `TestLoadCatalogLockedResolvesHomeWithoutRelocking` run the state readers under a
   watchdog, because a self-deadlock never returns and never errors.
2. **Never hold `state.mu` across a host call.** A host call made from inside a
   handler did work on the host this was measured against (pig 0.7.0+1.5.0): the
   SDK reports `blocked/host_call` and pig serves the call. That is observed
   behaviour, not an SDK guarantee, so nothing here relies on it — but a mutex
   held across a host call turns any slow host into a wedged extension, and that
   failure mode has no error message at all.

Network reads still stay off handlers, for a different reason: a plan read or an
account fan-out inside `session_start`/`turn_end` makes pig wait on Cursor's HTTP
round trip before it can carry on. Those, and the pushes that show their results,
run on one single-flight worker (`state.deferUsageWork`), so a burst of turns
coalesces into one extra pass instead of stacking goroutines. Both background
goroutines (the worker and the bar's fill-in animation) outlive the handler that
started them and recover a panic, because a context goes stale when the session is
replaced and a read through it panics.

A second bug fell out of the same investigation: `ctx.Model()` is the **bare** model
id (`grok-4.7@500k:slow`), not `cursor/grok-4.7@500k:slow`, so the old
`strings.HasPrefix(model, "cursor/")` test never matched. The bar and the status
line were therefore dead even when nothing hung. `isCursorModel` now reads
`ctx.ModelProvider()` and falls back to the prefix, and the bar is verified on screen
by `dev/bar-screen.sh` and `dev/tty-reload-check.py`.
