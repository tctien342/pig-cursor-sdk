#!/usr/bin/env python3
"""Show Cursor's own tool calls on screen — live, and again after a resume.

With no host tools advertised, a Cursor agent has only its built-in tools, so this
is the reliable way to make the feature visible on demand. Two phases:

1. a live turn: the line must be drawn as the call finishes;
2. `pig -c` on the same session: the line must be drawn again, this time from the
   entry stored in the session file, with no live run at all.

    dev/tty-cursor-tools.py

Exit 0 means both. The final screen of each phase is printed either way.
"""
import fcntl
import json
import os
import pty
import re
import select
import struct
import sys
import termios
import time

import pyte

ROWS, COLS = 40, 110
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LOG = os.path.expanduser("~/.pig/agent/pig-cursor-sdk/agent.log")
RAW = "/tmp/pi-cursor-tools-raw.log"
MODEL = "cursor/grok-4.7"
PROMPT = ("Run the shell command `ls -1` with whatever tool you have, then tell me "
          "how many entries it printed. Answer with the number only.")


def log_size():
    try:
        return os.path.getsize(LOG)
    except OSError:
        return 0


def log_since(offset):
    try:
        with open(LOG, "rb") as f:
            f.seek(offset)
            return f.read().decode("utf-8", "replace")
    except OSError:
        return ""


def bridge_pids():
    out = os.popen("ps -ax -o pid=,command=").read()
    pids = set()
    for line in out.splitlines():
        head = line.split(None, 1)
        if len(head) == 2 and "cursor-sdk-bridge" in head[1]:
            try:
                pids.add(int(head[0]))
            except ValueError:
                pass
    return pids


class Screen:
    def __init__(self):
        self.grid = pyte.Screen(COLS, ROWS)  # pyte takes (columns, lines)
        self.stream = pyte.ByteStream(self.grid)

    def feed(self, data):
        self.stream.feed(data)

    def lines(self):
        return [l.rstrip() for l in self.grid.display]

    def text(self):
        return "\n".join(self.lines())

    def hits(self, pattern):
        rx = re.compile(pattern)
        return [l for l in self.lines() if rx.search(l)]


def draw_after_reopen():
    """Reopen the session just written and return the screen lines, or None.

    -c continues the previous session for this directory, which is the one the
    live phase above just wrote.
    """
    key = ""
    try:
        key = json.load(open(os.path.expanduser("~/.pig/agent/auth.json")))["cursor"]["key"]
    except Exception:
        pass
    env = dict(os.environ, CURSOR_API_KEY=key, TERM="xterm-256color")
    before = bridge_pids()
    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(ROOT)
        os.execvpe("pig", ["pig", "-e", ".", "--continue"], env)
        os._exit(127)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
    screen = Screen()
    found = None
    t0 = time.time()
    try:
        while time.time() - t0 < 60:
            r, _, _ = select.select([fd], [], [], 0.3)
            if not r:
                if os.waitpid(pid, os.WNOHANG)[0]:
                    break
                continue
            try:
                data = os.read(fd, 65536)
            except OSError:
                break
            if not data:
                break
            screen.feed(data)
            if screen.hits(r"◇ cursor:"):
                found = screen.lines()
                break
    finally:
        try:
            os.kill(pid, 9)
            os.waitpid(pid, 0)
        except Exception:
            pass
        for bridge in bridge_pids() - before:
            try:
                os.kill(bridge, 9)
            except Exception:
                pass
    return found


def main():
    key = ""
    try:
        key = json.load(open(os.path.expanduser("~/.pig/agent/auth.json")))["cursor"]["key"]
    except Exception:
        pass
    env = dict(os.environ, CURSOR_API_KEY=key, TERM="xterm-256color", PI_CURSOR_GO_DEBUG="1")
    offset = log_size()
    before = bridge_pids()
    raw = open(RAW, "wb")

    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(ROOT)
        # No host tools: Cursor can only use its own, which is what this displays.
        os.execvpe("pig", ["pig", "-e", ".", "--model", MODEL, "--no-tools", PROMPT], env)
        os._exit(127)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))

    screen = Screen()
    failures = []
    t0 = time.time()
    finished = False
    try:
        while time.time() - t0 < 240:
            r, _, _ = select.select([fd], [], [], 0.3)
            if r:
                try:
                    data = os.read(fd, 65536)
                except OSError:
                    break
                if not data:
                    break
                screen.feed(data)
                raw.write(data)
                raw.flush()
            log = log_since(offset)
            if not finished and "turn complete" in log:
                finished = True
                # Give the entry renderer a moment to be asked and drawn.
                deadline = time.time() + 8
                while time.time() < deadline:
                    r, _, _ = select.select([fd], [], [], 0.3)
                    if r:
                        try:
                            d = os.read(fd, 65536)
                        except OSError:
                            break
                        if not d:
                            break
                        screen.feed(d)
                        raw.write(d)
                        raw.flush()
                break
            if os.waitpid(pid, os.WNOHANG)[0]:
                break
        if not finished:
            failures.append("the turn never completed")

        cards = screen.hits(r"◇ cursor:")
        logged = re.findall(r'msg="cursor internal tool" .*?tool=(\S+)', log_since(offset))
        print(f"extension logged Cursor-side calls: {sorted(set(logged)) or 'none'}")
        print(f"lines on screen: {len(cards)}")
        for line in cards:
            print("  " + line.strip())
        if not cards:
            failures.append("no Cursor-tool line reached the screen")
        if cards and not any("ls -1" in c for c in cards):
            failures.append("the line does not say what was run (expected the command)")

        # ctrl+o is pig's expand: the arguments each call ran with.
        if cards:
            os.write(fd, b"\x0f")
            deadline = time.time() + 8
            detail = []
            while time.time() < deadline:
                r, _, _ = select.select([fd], [], [], 0.3)
                if r:
                    try:
                        d = os.read(fd, 65536)
                    except OSError:
                        break
                    if not d:
                        break
                    screen.feed(d)
                    raw.write(d)
                    raw.flush()
                detail = screen.hits(r"command: ls -1")
                if detail:
                    break
            if detail:
                print("expanded: " + detail[0].strip())
            else:
                failures.append("ctrl+o did not expand the line's arguments")
    finally:
        try:
            os.write(fd, b"/exit\r")
        except OSError:
            pass
        for _ in range(20):
            try:
                if os.waitpid(pid, os.WNOHANG)[0]:
                    break
            except Exception:
                break
            time.sleep(0.25)
        try:
            os.kill(pid, 9)
            os.waitpid(pid, 0)
        except Exception:
            pass
        for bridge in bridge_pids() - before:
            try:
                os.kill(bridge, 9)
            except Exception:
                pass
        raw.close()

    print("--- final screen (live turn) ---")
    print("\n".join(l for l in screen.lines() if l.strip()))

    # Phase 2: the same session, reopened. Nothing runs here — the line can only
    # come from the entry in the session file and the renderer registered by the
    # factory, which is what a resumed transcript is made of.
    resumed = draw_after_reopen()
    if resumed is None:
        failures.append("reopening the session did not redraw the Cursor-tool line")
    else:
        print("--- final screen (reopened session) ---")
        print("\n".join(l for l in resumed if l.strip()))

    if failures:
        print("--- FAIL ---")
        for f in failures:
            print(" - " + f)
        return 1
    print("--- PASS: Cursor's own tool calls are visible in the transcript ---")
    return 0


if __name__ == "__main__":
    sys.exit(main())
