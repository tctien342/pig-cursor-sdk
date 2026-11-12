#!/usr/bin/env python3
"""Cursor's own tool calls must be visible *while* they run, not only afterwards.

The transcript cannot do that: pig commits an assistant message at the end of the
turn, so an entry appended mid-turn lands above the message it belongs to. So the
calls are drawn as cards in a widget above the editor while the turn streams, and
only written into the transcript when the turn ends.

    dev/tty-live-tools.py

Exit 0 when a running card is on screen during the call, and the same call appears
once in the transcript afterwards with the widget gone.
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
PROMPT = ("Run the shell command `sleep 8 && echo live-done` with whatever tool you "
          "have, then tell me the single word it printed.")


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

    def cards(self):
        return [l.strip() for l in self.lines() if "◇ cursor:" in l]

    def dump(self, keep=24):
        return "\n".join(l for l in self.lines() if l.strip())[-keep * 120:]


def pump(fd, screen, seconds, stop=None):
    t0 = time.time()
    while time.time() - t0 < seconds:
        r, _, _ = select.select([fd], [], [], 0.2)
        if r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                return False
            if not data:
                return False
            screen.feed(data)
        if stop and stop():
            return True
    return False


def log_since(offset):
    try:
        with open(LOG, "rb") as f:
            f.seek(offset)
            return f.read().decode("utf-8", "replace")
    except OSError:
        return ""


def main():
    env = dict(os.environ, CURSOR_API_KEY=key(), TERM="xterm-256color",
               PI_CURSOR_GO_DEBUG="1")
    log0 = os.path.getsize(LOG) if os.path.exists(LOG) else 0
    before = bridge_pids()
    failures = []

    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(ROOT)
        # No host tools: Cursor can only use its own, which is what this shows.
        os.execvpe("pig", ["pig", "-e", ".", "--model", MODEL, "--no-tools",
                           "--use-theme", "dark", PROMPT], env)
        os._exit(127)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))

    screen = Screen()
    try:
        # The call runs for eight seconds; the card has to be on screen inside that.
        running = pump(fd, screen, 120, stop=lambda: any("running" in c for c in screen.cards()))
        live = [c for c in screen.cards() if "running" in c]
        if live:
            print("live card while running: " + live[0])
        else:
            failures.append("no running card appeared while the tool ran")

        # Then the turn ends: the widget goes, and the card lands in the transcript.
        pump(fd, screen, 120, stop=lambda: "turn complete" in log_since(log0))
        pump(fd, screen, 4)
        after = screen.cards()
        for c in after:
            print("after the turn: " + c)
        if not after:
            failures.append("the finished call is not in the transcript")
        if any("running" in c for c in after):
            failures.append("a card still says running after the turn ended")
        if len(after) > 1:
            failures.append(f"the call is drawn {len(after)} times (widget and transcript both)")
        if "live-done" not in " ".join(after):
            failures.append("the transcript card does not say what ran")
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

    log = log_since(log0)
    print("--- what the extension logged ---")
    for line in log.splitlines():
        if re.search(r"live tool widget|turn complete|cursor internal tool", line):
            print("  " + re.sub(r"^time=\S+ ", "", line)[:140])

    if failures:
        print("--- FAIL ---")
        for f in failures:
            print(" - " + f)
        print("--- screen ---")
        print(screen.dump())
        return 1
    print("--- PASS: the running call is on screen while it runs, and once after ---")
    return 0


if __name__ == "__main__":
    sys.exit(main())
