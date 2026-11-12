#!/usr/bin/env bash
# Two turns in one session: the second must continue the same Cursor run state,
# not replay the transcript. Usage: dev/run-multiturn.sh [model]
set -uo pipefail
cd "$(dirname "$0")/.."
MODEL="${1:-cursor/grok-4.7}"
KEY=$(python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.pig/agent/auth.json')))['cursor']['key'])")
export CURSOR_API_KEY="$KEY" PI_CURSOR_GO_DEBUG=1
DIR=$(mktemp -d /tmp/cursor-go-sess.XXXX)
STAMP=$(date +%H%M%S)
echo "== turn 1 (session dir $DIR)"
pig -e . --model "$MODEL" --mode json --session-dir "$DIR" -p "Remember this exact codeword: banana-blue. Reply with just OK." >"docs/research/mt1-$STAMP.jsonl" 2>>~/.pig/agent/pig-cursor-sdk/agent.log
python3 - "docs/research/mt1-$STAMP.jsonl" <<'PY'
import json,sys
for l in open(sys.argv[1]):
    l=l.strip()
    if not l.startswith('{'): continue
    d=json.loads(l)
    if d.get('type')=='message_end' and d.get('message',{}).get('role')=='assistant':
        print('turn1 assistant:', ' '.join(b.get('text','') for b in d['message'].get('content',[]) if b.get('type')=='text')[:120])
PY
echo "== turn 2 (--continue, same session)"
pig -e . --model "$MODEL" --mode json --session-dir "$DIR" --continue -p "What codeword did I ask you to remember? Answer with the word only." >"docs/research/mt2-$STAMP.jsonl" 2>>~/.pig/agent/pig-cursor-sdk/agent.log
python3 - "docs/research/mt2-$STAMP.jsonl" <<'PY'
import json,sys
for l in open(sys.argv[1]):
    l=l.strip()
    if not l.startswith('{'): continue
    d=json.loads(l)
    if d.get('type')=='message_end' and d.get('message',{}).get('role')=='assistant':
        print('turn2 assistant:', ' '.join(b.get('text','') for b in d['message'].get('content',[]) if b.get('type')=='text')[:160])
PY
echo "== agent.log tail"; tail -6 ~/.pig/agent/pig-cursor-sdk/agent.log
