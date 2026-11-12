#!/usr/bin/env bash
# How long until the extension is actually running, and is the bar on screen then?
# A blind timer kept measuring pig's compile step, not the UI, so this watches the
# extension's own log and only starts interacting once the extension has spoken.
set -uo pipefail
cd "$(cd "$(dirname "$0")/.." && pwd)"

RAW=/tmp/pi-bar-raw.log
LOG="$HOME/.pig/agent/pig-cursor-sdk/agent.log"
rm -f "$RAW"; : > "$RAW"
: > /tmp/pi-bar-marker
BEFORE=$(wc -l < "$LOG" 2>/dev/null | tr -d ' ' || echo 0)

python3 - "$BEFORE" "$LOG" <<'EOF'
import fcntl, json, os, pty, re, select, struct, sys, termios, time

before, logpath = int(sys.argv[1]), sys.argv[2]
key = None
try:
    key = json.load(open(os.path.expanduser("~/.pig/agent/auth.json")))["cursor"]["key"]
except Exception:
    pass
env = dict(os.environ, CURSOR_API_KEY=key or "", TERM="xterm-256color", PI_CURSOR_GO_DEBUG="1")

t0 = time.time()
pid, fd = pty.fork()          # (pid, fd); gives the child a controlling terminal
if pid == 0:
    os.execvpe("pig", ["pig", "-e", ".", "--model", "cursor/grok-4.7"], env)
    os._exit(127)
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 110, 0, 0))
out = open("/tmp/pi-bar-raw.log", "ab")

def logtail():
    try:
        lines = open(logpath, encoding="utf-8", errors="replace").read().splitlines()
    except OSError:
        return []
    return lines[before:]

loaded = None
bar = None
typed = 0
deadline = t0 + 300
while time.time() < deadline:
    r, _, _ = select.select([fd], [], [], 0.3)
    if r:
        try:
            data = os.read(fd, 65536)
        except OSError:
            break
        if not data:
            break
        out.write(data); out.flush()
    tail = logtail()
    if loaded is None and tail:
        loaded = time.time() - t0
        print(f"extension logged after {loaded:.1f}s: {tail[-1][:110]}", flush=True)
    if bar is None:
        hit = [l for l in tail if "plan bar" in l]
        if hit:
            bar = time.time() - t0
            print(f"plan bar logged after {bar:.1f}s: {hit[-1].split('msg=')[-1][:120]}", flush=True)
    # Once the extension is up, ask for the readout so the widget has a reason to
    # repaint, then stop shortly after.
    if loaded and bar and typed < 2 and time.time() - t0 > bar + 6 + typed * 4:
        os.write(fd, b"/cursor-usage\r" if typed == 0 else b"/exit\r")
        typed += 1
    if typed >= 2 and time.time() - t0 > bar + 20:
        break
print(f"capture: {os.path.getsize('/tmp/pi-bar-raw.log')} bytes over {time.time()-t0:.0f}s", flush=True)
out.close()
try:
    os.write(fd, b"\x03")
except OSError:
    pass
for _ in range(12):
    try:
        if os.waitpid(pid, os.WNOHANG)[0]:
            break
    except Exception:
        break
    time.sleep(0.25)
try:
    os.kill(pid, 9); os.waitpid(pid, 0)
except Exception:
    pass
EOF

echo "=== extension's account of the bar ==="
grep -aE "plan bar" "$LOG" | tail -4 | sed 's/time=[^ ]* //' | cut -c1-150
echo "=== is the bar on the rendered screen? ==="
dev/.venv/bin/python dev/screen-state.py '▰|▱|PI-WIDGET' /tmp/pi-bar-raw.log
echo "=== what is on screen ==="
dev/.venv/bin/python dev/screen-state.py '.' /tmp/pi-bar-raw.log 2>/dev/null | tail -8 | cut -c1-112
