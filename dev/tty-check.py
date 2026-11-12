#!/usr/bin/env python3
"""Drive pig interactively in a real PTY and save what the terminal showed.

The point is to read the screen the way a user does: tool cards, thinking, and
the answer, with no protocol noise. Usage:

    dev/tty-check.py [model] ["prompt"]

Writes docs/research/tty-session.txt (ANSI stripped) plus the raw capture.
"""
import os, pty, re, select, subprocess, sys, time

root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
model = sys.argv[1] if len(sys.argv) > 1 else "cursor/grok-4.7"
prompt = sys.argv[2] if len(sys.argv) > 2 else "Use the bash tool to run exactly: echo tty-check. Then tell me what printed."
# After the turn settles we ask for the usage readout, so one capture shows both
# the footer line and what /cursor-usage prints.
followups = ["/cursor-usage", "/cursor-status"]

key = None
try:
    import json
    key = json.load(open(os.path.expanduser("~/.pig/agent/auth.json")))["cursor"]["key"]
except Exception:
    pass
env = dict(os.environ, CURSOR_API_KEY=key or "", TERM="xterm-256color", PI_CURSOR_GO_DEBUG="1")

cmd = ["pig", "-e", ".", "--model", model, prompt]
pid, fd = pty.fork()
if pid == 0:
    os.chdir(root)
    os.execvpe(cmd[0], cmd, env)

buf = b""
deadline = time.time() + 300
sent = False
last_input = time.time()
queue = list(followups)
while time.time() < deadline:
    r, _, _ = select.select([fd], [], [], 0.5)
    if r:
        try:
            data = os.read(fd, 65536)
        except OSError:
            break
        if not data:
            break
        buf += data
        last_input = time.time()
    else:
        if not sent and time.time() - last_input > 3:
            # the first prompt was passed on the command line; give the run time
            sent = True
        if queue and sent and time.time() - last_input > 6:
            cmd = queue.pop(0).encode() + b"\r"
            os.write(fd, cmd)
            last_input = time.time()
            time.sleep(4)  # let the readout render before the next one
            continue
        if sent and not queue and time.time() - last_input > 20:
            break
try:
    os.write(fd, b"\x03")  # Ctrl+C: interrupt whatever is running
    time.sleep(0.6)
    os.write(fd, b"/exit\r")
    time.sleep(1.0)
except OSError:
    pass
try:
    os.close(fd)
except OSError:
    pass
os.waitpid(pid, os.WNOHANG)

raw = os.path.join(root, "docs", "research", "tty-raw.txt")
plain = os.path.join(root, "docs", "research", "tty-session.txt")
os.makedirs(os.path.dirname(raw), exist_ok=True)
open(raw, "wb").write(buf)

text = buf.decode("utf-8", "replace")
text = re.sub(r"\x1b\[[0-9;?]*[A-Za-z]", "", text)
text = re.sub(r"\x1b\][^\x07\x1b]*(\x07|\x1b\\)", "", text)
text = re.sub(r"\x1b[()][A-Z0-9]", "", text)
text = text.replace("\r\n", "\n").replace("\r", "\n")
lines = [l.rstrip() for l in text.split("\n")]
out, blank = [], 0
for l in lines:
    if not l.strip():
        blank += 1
        if blank > 1:
            continue
    else:
        blank = 0
    out.append(l)
open(plain, "w").write("\n".join(out) + "\n")
print(f"captured {len(buf)} bytes -> {plain}")
