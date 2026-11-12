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
pig install .                 # or: pig install <path-to-this-repo>
pig --list-models cursor      # 124 selectable models, no network needed
```

Requirements: Go (only to build the extension; pig builds and caches it on first run)
and the Cursor bridge binary:

```bash
mkdir -p ~/.pig/agent/pig-cursor-sdk/bin
# download the standalone archive from the sdk-bridge repo, then:
cp cursor-sdk-bridge ~/.pig/agent/pig-cursor-sdk/bin/ && chmod +x $_
```

The bridge is a self-contained Bun single-file executable — **no Node.js and no
system Bun are used anywhere**. Verified by running a full agent turn with
`PATH=/usr/bin:/bin`: the bridge starts, authenticates, streams, and finishes.

An API key is required (Cursor Desktop's subscription login does not provide one):

```
/cursor-login        # stores the key under ~/.pig/agent/pig-cursor-sdk/
```

`CURSOR_API_KEY` and pig's own stored credential for `cursor` also work.

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
| `/cursor-abort` | release this session's live Cursor run (completed host work is not undone) |
| `/cursor-status` | bridge pid, catalog size, mode, key presence, tool mode |
| `/cursor-login` | store an API key |

Environment: `CURSOR_SDK_BRIDGE_BIN` (bridge path), `PI_CURSOR_TURN_TIMEOUT`
(default `5m`), `PI_CURSOR_GO_DEBUG=1` (debug logging to
`~/.pig/agent/pig-cursor-sdk/agent.log`), `PI_CURSOR_SETTING_SOURCES`,
`PI_CURSOR_FOOTER=0`.

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
```

## Known limitations

- Model aliases the TypeScript extension invented (`auto`, `composer`,
  `fable-5-2@1m`) are not part of `sdk.v1`; only canonical Cursor ids are offered.
- If Cursor answers with one of *its own* built-in tools instead of a host tool,
  nothing renders — there is no host tool to attach a card to. Say the word and a
  footer notice can be added; injecting a transcript line for it is deliberately
  not the default.
- Cost is reported as an estimate of zero with `totalFromProvider: false`: Cursor
  publishes token counts, not a per-run dollar amount.
- Cursor's own account usage/quota is not exposed by `sdk.v1`, so there is no
  quota readout to show.
