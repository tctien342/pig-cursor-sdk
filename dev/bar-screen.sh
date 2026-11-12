#!/usr/bin/env bash
# Is the plan bar actually painted? Start pig with a Cursor model, sit idle, and
# record the screen. Raw bytes are appended as they arrive, so a killed run still
# leaves evidence.
set -uo pipefail
cd "$(cd "$(dirname "$0")/.." && pwd)"

RAW=/tmp/pi-bar-raw.log
CAP=/tmp/pi-bar-session.txt
rm -f "$RAW" "$CAP"
: > "$RAW"

python3 - > /tmp/pi-bar-exit.log 2>&1 <<'EOF'
import os, pty, select, sys, time, fcntl, termios, struct
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 110, 0, 0))
env = dict(os.environ, TERM="xterm-256color", PI_CURSOR_GO_DEBUG="1")
pid = os.fork()
if pid == 0:
    os.close(master); os.setsid(); os.dup2(slave, 0); os.dup2(slave, 1); os.dup2(slave, 2)
    os.execvpe("pig", ["pig", "-e", ".", "--model", "cursor/grok-4.7"], env)
    os._exit(127)
os.close(slave)
out = open("/tmp/pi-bar-raw.log", "ab")
t0 = time.time(); exited = False
while time.time() - t0 < 35:
    r, _, _ = select.select([master], [], [], 0.2)
    if master in r:
        try:
            data = os.read(master, 131072)
        except OSError:
            break
        if not data:
            break
        out.write(data); out.flush()
    if not exited and time.time() - t0 > 30:
        os.write(master, b"/exit\r"); exited = True; time.sleep(1.0)
os.write(master, b"\x03"); time.sleep(0.4)
try:
    os.kill(pid, 15); os.waitpid(pid, 0)
except Exception:
    pass
out.close()
EOF

python3 dev/clean_ansi.py < "$RAW" > "$CAP" 2>/dev/null || cp "$RAW" "$CAP"

echo "=== bar glyphs on screen ==="
grep -ac '▰\|▱' "$CAP" | sed 's/^/  lines containing a bar: /'
grep -ao ' cursor ▰[^│]\{0,60\}\| cursor ▱[^│]\{0,60\}' "$CAP" | tail -3
echo "=== extension's own account of it ==="
grep -aE "plan bar" ~/.pig/agent/pig-cursor-sdk/agent.log | tail -4 | sed 's/time=[^ ]* //'
echo "=== last screen frame (last 12 non-blank lines) ==="
grep -av '^[[:space:]]*$' "$CAP" | tail -12 | cut -c1-110
cp "$CAP" docs/research/tty-bar.txt
