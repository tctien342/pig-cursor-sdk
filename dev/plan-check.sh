#!/usr/bin/env bash
# Live check of the plan readout, both ways it can go: Cursor Desktop present,
# and Cursor Desktop absent. Captures the terminal so the footer and the
# /cursor-usage text are read the way a user sees them.
set -uo pipefail
cd "$(dirname "$0")/.."
KEY=$(python3 -c "import json,os;print(json.load(open(os.path.expanduser('~/.pig/agent/auth.json')))['cursor']['key'])")
export CURSOR_API_KEY="$KEY"

echo "== 1. Cursor Desktop installed and logged in =="
python3 dev/tty-check.py cursor/grok-4.7 "Reply with exactly: plan-check" >/dev/null 2>&1
cp docs/research/tty-session.txt /tmp/plan-present.txt
grep -nE "plan:|cycle:|plan [0-9]|account:|not read" /tmp/plan-present.txt | sed 's/^/   /' | head -8
cp /tmp/plan-present.txt docs/evidence/tty-plan-usage.txt

echo
echo "== 2. same build, Cursor Desktop not findable =="
PI_CURSOR_STORE_PATH=/nonexistent/state.vscdb python3 dev/tty-check.py cursor/grok-4.7 "Reply with exactly: plan-check" >/dev/null 2>&1
grep -nE "plan:|not read" docs/research/tty-session.txt | sed 's/^/   /' | head -4
cp docs/research/tty-session.txt /tmp/plan-absent.txt
