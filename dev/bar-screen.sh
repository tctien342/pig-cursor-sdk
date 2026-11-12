#!/usr/bin/env bash
# Is the plan bar actually painted? Start pig with a Cursor model, sit idle, and
# record the screen with the harness that is known to work (pty.fork gives the
# child a controlling terminal; openpty+setsid does not, and pig's TUI exits after
# its first frame). Raw bytes are appended as they arrive, so a killed run still
# leaves evidence.
set -uo pipefail
cd "$(cd "$(dirname "$0")/.." && pwd)"

RAW=/tmp/pi-bar-raw.log
CAP=/tmp/pi-bar-session.txt
rm -f "$RAW" "$CAP"; : > "$RAW"

python3 - <<'EOF'
import json, os, pty, select, time
key = None
try:
    key = json.load(open(os.path.expanduser("~/.pig/agent/auth.json")))["cursor"]["key"]
except Exception:
    pass
env = dict(os.environ, CURSOR_API_KEY=key or "", TERM="xterm-256color", PI_CURSOR_GO_DEBUG="1")
pid, fd = pty.fork()   # pty.fork returns (pid, fd), not (fd, pid)
if pid == 0:
    os.execvpe("pig", ["pig", "-e", ".", "--model", "cursor/grok-4.7"], env)
    os._exit(127)
import fcntl, struct, termios
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 110, 0, 0))  # rows, cols
out = open("/tmp/pi-bar-raw.log", "ab")
deadline = time.time() + 120
t0 = time.time()
exited = False
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
    if not exited and time.time() - t0 > 100:
        os.write(fd, b"/cursor-usage\r"); time.sleep(3)
        os.write(fd, b"/exit\r"); exited = True
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

python3 dev/clean_ansi.py < "$RAW" > "$CAP" 2>/dev/null || cp "$RAW" "$CAP"

echo "capture bytes: $(wc -c < "$RAW" | tr -d ' ')"
echo "=== bar glyphs on screen ==="
grep -ac '▰\|▱' "$CAP" | sed 's/^/  lines containing a bar: /'
grep -ao ' cursor ▰.\{0,70\}\| cursor ▱.\{0,70\}' "$CAP" | tail -3
echo "=== extension's own account of it ==="
grep -aE "plan bar" ~/.pig/agent/pig-cursor-sdk/agent.log | tail -4 | sed 's/time=[^ ]* //' | cut -c1-150
echo "=== last screen frame ==="
grep -av '^[[:space:]]*$' "$CAP" | tail -10 | cut -c1-112
cp "$CAP" docs/research/tty-bar.txt
