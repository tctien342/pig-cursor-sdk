#!/usr/bin/env bash
# Remaining end-to-end checks: several host tool calls in one turn, and whether
# a killed host leaves an orphan bridge behind.
set -uo pipefail
cd "$(dirname "$0")/.."
KEY=$(python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.pig/agent/auth.json')))['cursor']['key'])")
export CURSOR_API_KEY="$KEY"
OUT=/tmp/pig-cursor-edge
TOOLCALLS="$(cd "$(dirname "$0")" && pwd)/toolcalls.py"
mkdir -p "$OUT"

echo "== 1. two host tool calls in one turn"
LOG=~/.pig/agent/pig-cursor-sdk/agent.log
mark_before=$(wc -l < "$LOG" | tr -d ' ')
pig --model cursor/grok-4.7 --mode json --no-session -p \
  "Call the bash tool twice, both calls in the same response: run 'echo one' and run 'echo two'. Then tell me both outputs." \
  >"$OUT/two-calls.jsonl" 2>"$OUT/two-calls.err"
python3 - "$OUT/two-calls.jsonl" <<'PY'
import json, sys, collections
calls = collections.Counter(); ids = []
execs = []
results = 0
turns = 0
for line in open(sys.argv[1]):
    line = line.strip()
    if not line.startswith("{"):
        continue
    d = json.loads(line)
    if d.get("type") == "turn_start":
        turns += 1
    if d.get("type") == "tool_execution_start":
        execs.append(json.dumps(d.get("args"), ensure_ascii=False)[:40])
    m = d.get("message") or {}
    for b in (m.get("content") or []):
        if b.get("type") == "toolCall":
            calls[b.get("name")] += 1
            ids.append(b.get("id"))
        if b.get("type") == "text" and m.get("role") == "assistant" and d.get("type") == "message_end":
            print("   final text:", b.get("text", "")[:160].replace("\n", " "))
print("   toolCall blocks:", dict(calls), "distinct ids:", len(set(ids)))
print("   host executions:", len(execs), execs)
print("   turns:", turns)
PY
echo "   exactly-once log, this run only (one park and one delivery per id):"
tail -n +$((mark_before + 1)) "$LOG" > "$OUT/turn.log"
python3 "$TOOLCALLS" "$OUT/turn.log"

echo
echo "== 2. killed host must not orphan the bridge"
before=$(pgrep -x cursor-sdk-bridge | wc -l | tr -d ' ')
pig --model cursor/grok-4.7 --mode text --no-session -p \
  "Use the bash tool to run: sleep 25 && echo late. Then summarise." >"$OUT/killed.out" 2>&1 &
hostpid=$!
sleep 12
bridges_live=$(pgrep -x cursor-sdk-bridge | wc -l | tr -d ' ')
echo "   bridges alive mid-run: $bridges_live (expect >=1)"
kill -TERM "$hostpid" 2>/dev/null
wait "$hostpid" 2>/dev/null
sleep 6
after=$(pgrep -x cursor-sdk-bridge | wc -l | tr -d ' ')
echo "   bridges before=$before after=$after (expect equal)"
pgrep -lx cursor-sdk-bridge | sed 's/^/     still running: /' || true

echo
echo "== 3. state directory contents"
ls -la ~/.pig/agent/pig-cursor-sdk | tail -n +2 | awk '{print "    ", $5, $9}'
