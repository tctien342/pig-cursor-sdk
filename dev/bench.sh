#!/usr/bin/env bash
# Startup and first-token comparison between the Go provider and the TypeScript
# extension it replaces. Same pig build, same prompt, same model; each line of
# the host's JSON event stream is timestamped as it arrives, so the numbers are
# wall-clock from process start, not host-internal bookkeeping.
#
#   dev/bench.sh                # both, 3 runs each
#   dev/bench.sh go 2           # Go only, 2 runs
set -uo pipefail
cd "$(dirname "$0")/.."

WHICH="${1:-both}"
RUNS="${2:-3}"
MODEL="${MODEL:-cursor/grok-4.7}"
PROMPT="${PROMPT:-Say exactly: bench ok}"

KEY=$(python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.pig/agent/auth.json')))['cursor']['key'])")
export CURSOR_API_KEY="$KEY"

SETTINGS="$HOME/.pig/agent/settings.json"
BAK=$(mktemp)
cp "$SETTINGS" "$BAK"
trap 'cp "$BAK" "$SETTINGS"' EXIT

HARNESS=$(cat <<'PY'
import json, subprocess, sys, time
label, settings, pkg = sys.argv[1], sys.argv[2], sys.argv[3]
args = json.loads(sys.argv[4])
t0 = time.time()
first_line = first_token = None
p = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
for line in p.stdout:
    if not line.startswith("{"):
        continue
    if first_line is None:
        first_line = time.time() - t0
    try:
        d = json.loads(line)
    except Exception:
        continue
    ev = d.get("assistantMessageEvent") or {}
    if first_token is None and ev.get("type") in ("text_delta", "thinking_delta"):
        first_token = time.time() - t0
p.wait()
total = time.time() - t0
f = lambda v: "  n/a" if v is None else f"{v:6.2f}s"
print(f"{label:4s} first_event={f(first_line)} first_token={f(first_token)} total={total:6.2f}s")
PY
)

run_one() { # $1=label  rest=pig args
  local label="$1"; shift
  local extra
  extra=$(printf '%s\n' "$@" | python3 -c 'import json,sys;print(json.dumps([l.rstrip("\n") for l in sys.stdin if l.strip()]))')
  local base
  base=$(python3 -c "import json;print(json.dumps(['pig','--model','$MODEL','--mode','json','--no-session','-p','$PROMPT']))")
  local args
  args=$(python3 -c "import json,sys;print(json.dumps(json.loads(sys.argv[1])+json.loads(sys.argv[2])))" "$base" "$extra")
  python3 -c "$HARNESS" "$label" "" "" "$args"
}

if [ "$WHICH" = "both" ] || [ "$WHICH" = "go" ]; then
  echo "== Go extension (pure-Go provider, Bun bridge)"
  python3 - "$SETTINGS" <<'PY'
import json,sys
p=sys.argv[1]; d=json.load(open(p))
pk=[x for x in d.get('packages',[]) if 'pi-cursor-sdk' not in x]
d['packages']=pk
json.dump(d, open(p,'w'), indent=2)
PY
  for i in $(seq 1 "$RUNS"); do run_one go -e .; done
fi

if [ "$WHICH" = "both" ] || [ "$WHICH" = "npm" ]; then
  echo "== TypeScript extension (npm:pi-cursor-sdk)"
  python3 - "$SETTINGS" <<'PY'
import json,sys
p=sys.argv[1]; d=json.load(open(p))
d['packages']=[x for x in d.get('packages',[]) if 'pi-cursor-sdk' not in x] + ['npm:pi-cursor-sdk']
json.dump(d, open(p,'w'), indent=2)
PY
  for i in $(seq 1 "$RUNS"); do run_one npm; done
fi
