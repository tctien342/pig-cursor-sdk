# Benchmarks

`./dev/bench.sh [go|npm|both] [runs]` — same pig build, same prompt
(`Say exactly: bench ok`), same model (`cursor/grok-4.7`), each line of the host's
JSON event stream timestamped as it arrives, so the numbers are wall-clock from
process start rather than host-internal bookkeeping. Run on Apple Silicon macOS,
November 2026, pig `0.7.0+1.5.0`.

| metric | pig-cursor-sdk (Go) | pi-cursor-sdk 0.5.26 (TS) |
| --- | --- | --- |
| `pig --list-models cursor` (extension load + catalog, no network) | 0.29 / 0.30 / 0.29 s | 0.44 / 0.44 / 0.44 s |
| first host event | 0.30 / 0.30 / 0.33 s | 0.47 / 0.45 / 0.52 s |
| first streamed token | 9.42 / 8.05 / 10.07 s | 10.54 / 10.43 / 9.80 s |
| full non-interactive turn | 12.15 / 10.91 / 13.10 s | 12.51 / 12.86 / 13.05 s |
| bridge process spawn (from the extension's own log) | ~145–230 ms | n/a (in-process JS SDK) |

Reading them: extension startup is about a third faster, and the per-turn cost is
the same shape because first token is dominated by Cursor's own round trip plus
the bridge's model selection. The Go win is in work done per turn, not in the
network.

What was removed from the hot path by the rewrite: no Node.js extension runtime or
`@cursor/sdk` object graph per turn, no TypeScript normalizer/ledger passes over
every stream frame, no injected display messages to render, and no re-parse of
tool payloads for display purposes — the decoder produces typed events once and
the emitter maps them straight onto host events.
