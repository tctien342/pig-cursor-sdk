#!/usr/bin/env bash
# Live end-to-end check of the Go cursor provider inside the real pig host.
# Usage: dev/run-live.sh <model-id> <prompt> [extra pig flags...]
set -uo pipefail
cd "$(dirname "$0")/.."

MODEL="${1:-cursor/grok-4.7}"
PROMPT="${2:-Use the bash tool to run exactly: echo hello-go. Then tell me what it printed.}"
shift 2 2>/dev/null || true

KEY=$(python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.pig/agent/auth.json')))['cursor']['key'])" 2>/dev/null)
if [ -z "${KEY:-}" ]; then echo "no cursor key in auth.json" >&2; exit 2; fi
export CURSOR_API_KEY="$KEY"
export PI_CURSOR_GO_DEBUG=1

OUT="docs/research/live-$(date +%H%M%S)"
mkdir -p docs/research
echo "== model=$MODEL out=$OUT.jsonl"
time pig -e . --model "$MODEL" --mode json --no-session -p "$PROMPT" "$@" \
  >"$OUT.jsonl" 2>"$OUT.log"
echo "EXIT=$?"
echo "-- events:"
python3 - "$OUT.jsonl" <<'PY'
import json,sys,collections
kinds=collections.Counter()
tools=[]
for line in open(sys.argv[1]):
    line=line.strip()
    if not line.startswith('{'): continue
    try: d=json.loads(line)
    except Exception: continue
    t=d.get('type','?')
    kinds[t]+=1
    if t=='assistant' or d.get('message',{}).get('role')=='assistant':
        m=d.get('message') or d
        for b in (m.get('content') or []):
            if isinstance(b,dict) and b.get('type') in ('toolCall','tool_use'):
                tools.append((b.get('name'), json.dumps(b.get('arguments') or b.get('input'))[:120]))
print(dict(kinds))
print('tool calls:', tools)
PY
echo "-- stderr tail:"
tail -20 "$OUT.log"
