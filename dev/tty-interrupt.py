#!/usr/bin/env python3
"""Send a new message while Cursor is still running one of its own tools.

That used to kill the turn before it started:

    cursor: run  is still active for this agent

— no answer, no card, nothing the user could retry. The leftover run has to be
released so the new message can go through; what must not happen is a dead turn.

    dev/tty-interrupt.py

Exit 0 when the second message gets an answer and no error is shown.
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

ROWS, COLS = 40, 120
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LOG = os.path.expanduser("~/.pig/agent/pig-cursor-sdk/agent.log")
MODEL = "cursor/grok-4.7"

SLOW = ("Run the shell command `sleep 30 && echo slow-done` with whatever tool you "
        "have. Do not answer until it finishes.")
SECOND = "What is 6 times 7? Answer with just the number."
ANSWER = "42"


def key():
    try:
        return json.load(open(os.path.expanduser("~/.pig/agent/auth.json")))["cursor"]["key"]
    except Exception:
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
        self.grid = pyte.Screen(COLS, ROWS)
        self.stream = pyte.ByteStream(self.grid)

    def feed(self, data):
        self.stream.feed(data)

    def lines(self):
        return [l.rstrip() for l in self.grid.display]

    def text(self):
        return "\n".join(self.lines())

    def has(self, needle):
        return needle in self.text()

    def dump(self, keep=22):
        return "\n".join(l for l in self.lines() if l.strip())[-keep * 120:]


def pump(fd, screen, seconds, stop=None):
    t0 = time.time()
    while time.time() - t0 < seconds:
        r, _, _ = select.select([fd], [], [], 0.3)
        if r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                return
            if not data:
                return
            screen.feed(data)
        if stop and stop():
            return


def main():
    env = dict(os.environ, CURSOR_API_KEY=key(), TERM="xterm-256color",
               PI_CURSOR_GO_DEBUG="1")
    log0 = os.path.getsize(LOG) if os.path.exists(LOG) else 0
    before = bridge_pids()
    failures = []

    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(ROOT)
        # No host tools: Cursor can only run its own, which is the case that parks a
        # run this process is not reading.
        os.execvpe("pig", ["pig", "-e", ".", "--model", MODEL, "--no-tools",
                           "--use-theme", "dark"], env)
        os._exit(127)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))

    screen = Screen()
    try:
        pump(fd, screen, 40, stop=lambda: "Startup is still in progress" not in screen.text()
             and "cursor ·" in screen.text())
        os.write(fd, (SLOW + "\r").encode())
        # Wait for the tool to actually be running, then talk over it.
        pump(fd, screen, 90, stop=lambda: "Cursor running" in screen.text() or "Cursor " in screen.text() and "done" in screen.text())
        if "Cursor running" not in screen.text():
            print("(the running hint never appeared; sending the second message anyway)")
        os.write(fd, (SECOND + "\r").encode())
        pump(fd, screen, 150, stop=lambda: re.search(r"^\s*42\s*$", screen.text(), re.M) is not None)
        text = screen.text()
        if not re.search(r"^\s*42\s*$", text, re.M):
            failures.append("the second message never got its answer")
        if "still active for this agent" in text:
            failures.append("the turn failed with 'run still active for this agent'")
        if re.search(r"^\s*(Error|error):", text, re.M):
            failures.append("an error was shown to the user")
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

    try:
        with open(LOG, "rb") as f:
            f.seek(log0)
            log = f.read().decode("utf-8", "replace")
    except OSError:
        log = ""
    print("--- what the extension logged ---")
    for line in log.splitlines():
        if re.search(r"turn complete|still active|already has active|cancelled runs|supersed", line):
            print("  " + re.sub(r"^time=\S+ ", "", line)[:150])

    if failures:
        print("--- FAIL ---")
        for f in failures:
            print(" - " + f)
        print("--- screen ---")
        print(screen.dump())
        return 1
    print("--- PASS: a message sent while Cursor is working still gets an answer ---")
    return 0


if __name__ == "__main__":
    sys.exit(main())
