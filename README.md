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
`PI_CURSOR_SETTING_SOURCES`, `PI_CURSOR_FOOTER=0`.

## Usage in the footer

The status line carries what the account has spent, refreshed when a turn ends
(never polled, and never on the request path — a slow usage RPC cannot delay an
answer):

```text
cursor · grok-4.7 · 24.1k tok · $0.31 · pid 41821
```

Numbers come from Cursor's own `GetUsage` for the agents this extension created:
token totals plus cost in cents, shown as charged when Cursor reports a charged
amount and as `… raw` when it only reports undiscounted model cost (plan-included,
BYOK, and grant accounts report no charged figure, so the label matters).
`/cursor-usage` gives the in/out/reasoning split and the run count.

Scope, stated plainly because the protocol sets the limit: `sdk.v1` exposes usage
per agent and no account-wide usage or plan-limit RPC (`SdkCursorService` has only
`Me`, `ListModels`, `ListRepositories`). So the footer reads *what these agents
consumed*, not *what the plan allows*. A fallback covers the case where the usage
RPC is unavailable: the run's own usage frame, which has tokens but no cost.

## How tool calls are displayed

The host owns tools. This extension advertises pig's **own active tools and
schemas** to Cursor, so when Cursor calls `bash`, pig executes `bash` and renders
its own card, with its own hooks and permissions. Nothing about a tool call is
ever written into the thinking channel, and Cursor's internal tool activity never
appears in the transcript.

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
proto/, gen/               sdk.v1 protos (vendored) and their generated Go
docs/display-rules.md      the display contract, enforced by tests
docs/evidence/             audit, live event capture, terminal capture, bridge resume log
dev/                       live/multi-turn/PTY runners, bridge probes, benchmark
scripts/gen-fallback-catalog.py  refresh the embedded offline catalog snapshot
```

## Verification

```bash
go test ./...                     # 22 tests: coordinator, decoder against real frames, display rules
pig install . --validate-only --json
./dev/run-live.sh cursor/grok-4.7 "Use the bash tool to run exactly: echo hello-go. Then tell me what it printed."
python3 dev/tty-check.py          # interactive PTY capture -> docs/evidence/tty-session.txt
./dev/run-edge.sh                 # several tool calls in one turn; no orphan bridge after a killed host
```

## Known limitations

- Model aliases the TypeScript extension invented (`auto`, `composer`,
  `fable-5-2@1m`) are not part of `sdk.v1`; only canonical Cursor ids are offered.
- `/cursor-abort` releases the run this pig process is holding. A run left active
  by a process that died is not cancelled eagerly: the next send notices the agent
  still owns an upstream run, releases it, and starts a clean one (logged at INFO).
- If Cursor answers with one of *its own* built-in tools instead of a host tool,
  no card renders — there is no host tool to attach one to. The footer says
  `Cursor ran Shell (its own tool)` instead; a transcript line for it would be
  exactly the noise this extension exists to remove. Disable with
  `PI_CURSOR_FOOTER=0`.
- Cost is reported as an estimate of zero with `totalFromProvider: false`: Cursor
  publishes token counts, not a per-run dollar amount.
- Cursor's own account usage/quota is not exposed by `sdk.v1`, so there is no
  quota readout to show.
