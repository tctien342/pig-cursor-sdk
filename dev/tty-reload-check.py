#!/usr/bin/env python3
"""Drive pig interactively and prove the two things the extension used to break:
a chat turn completes, and /reload survives it.

The failure this guards against is a hang with no error: the extension answered
neither turn_start nor session_start, so pig sat there forever and the only trace
was a goroutine dump. So the harness never sleeps on a timer alone; it waits for
the extension's own log and for text to appear on the emulated screen, and it
fails loudly on a timeout.

    dev/tty-reload-check.py

Exit status 0 means: a real turn answered, /reload came back, and a turn after the
reload answered too.
"""
import fcntl
import json
import os
import pty
import select
import struct
import sys
import termios
import time

import pyte

ROWS, COLS = 40, 110
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LOG = os.path.expanduser("~/.pig/agent/pig-cursor-sdk/agent.log")
RAW = "/tmp/pi-reload-raw.log"
MODEL = "cursor/grok-4.7"


def key():
    try:
        return json.load(open(os.path.expanduser("~/.pig/agent/auth.json")))["cursor"]["key"]
    except Exception:
        return ""


def log_size():
    try:
        return os.path.getsize(LOG)
    except OSError:
        return 0


def bridge_pids():
    """Pids of the Cursor bridge processes this extension spawns.

    pig is killed here rather than exited, so the extension never gets to close
    its bridge; the run's own bridges are cleaned up at the end instead of
    accumulating one per run.
    """
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


def log_since(offset):
    try:
        with open(LOG, "rb") as f:
            f.seek(offset)
            return f.read().decode("utf-8", "replace")
    except OSError:
        return ""




class Screen:
    """What a user would actually see, from a real terminal emulator."""

    def __init__(self):
        self.grid = pyte.Screen(COLS, ROWS)  # pyte takes (columns, lines)
        self.stream = pyte.ByteStream(self.grid)

    def feed(self, data):
        self.stream.feed(data)

    def text(self):
        return "\n".join(line.rstrip() for line in self.grid.display)

    def has(self, needle):
        return needle in self.text()

    def has_line(self, needle):
        """True when needle is a whole line, i.e. an answer and not the echoed prompt."""
        return any(line.strip() == needle for line in self.text().splitlines())

    def show(self, tail=12):
        lines = [l for l in self.text().splitlines() if l.strip()]
        return "\n".join(lines[-tail:])


def wait_for(pid, fd, screen, what, predicate, timeout, raw):
    """Pump the PTY until predicate(screen, log) is true, or fail on timeout."""
    t0 = time.time()
    while time.time() - t0 < timeout:
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
        if predicate(screen):
            return time.time() - t0
        if os.waitpid(pid, os.WNOHANG)[0]:
            break
    return None


def main():
    env = dict(os.environ, CURSOR_API_KEY=key(), TERM="xterm-256color", PI_CURSOR_GO_DEBUG="1")
    offset = log_size()
    bridges_before = bridge_pids()
    raw = open(RAW, "wb")

    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(ROOT)
        os.execvpe("pig", ["pig", "-e", ".", "--model", MODEL], env)
        os._exit(127)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))

    screen = Screen()
    failures = []   # a hang, a dead extension, a reload that never finished
    warnings = []   # what the model said is not the extension's contract
    try:
        # 1. The extension must come up and paint the bar, which is the last thing
        #    it does on the session_start path that used to deadlock.
        t = wait_for(pid, fd, screen, "extension up",
                     lambda s: "plan bar" in log_since(offset), 120, raw)
        if t is None:
            failures.append("extension never logged a plan bar line (session_start path hung?)")
        else:
            print(f"extension up after {t:.1f}s")

        # 2. A real turn must answer. "turn complete" in the log is the extension's
        #    own report that the run finished, so this is not just a painted word.
        os.write(fd, b"Reply with exactly: first-turn-ok\r")
        t = wait_for(pid, fd, screen, "first turn",
                     lambda s: "turn complete" in log_since(offset), 180, raw)
        if t is None:
            failures.append("first turn never completed")
        else:
            print(f"first turn completed after {t:.1f}s")
            # The turn finishing is the extension's contract; the model answering
            # with the exact token is not, so a refusal is a warning, not a fail.
            if screen.has_line("first-turn-ok"):
                print("first answer on screen")
            else:
                warnings.append("the model did not answer with first-turn-ok (see the final screen)")

        # 3. /reload: pig refuses while a response is in flight, so wait for the
        #    turn to settle (the working indicator is gone) before asking. A Go
        #    extension's factory is re-invoked in the process that already holds
        #    its module, so the pid does not change; the host's own "Reloaded"
        #    line is what proves the reload ran.
        idle = wait_for(pid, fd, screen, "idle", lambda s: not s.has("Working"), 60, raw)
        if idle is None:
            failures.append("pig never went idle after the first turn")
        os.write(fd, b"/reload\r")
        t = wait_for(pid, fd, screen, "reload", lambda s: s.has("Reloaded"), 180, raw)
        if t is None:
            failures.append("/reload never reported itself finished")
        else:
            print(f"reload finished after {t:.1f}s")

        # 4. And the session must still be usable afterwards.
        wait_for(pid, fd, screen, "idle after reload", lambda s: not s.has("Working"), 60, raw)
        mark2 = log_size()
        os.write(fd, b"Reply with exactly: after-reload-ok\r")
        t = wait_for(pid, fd, screen, "post-reload turn",
                     lambda s: "turn complete" in log_since(mark2), 180, raw)
        if t is None:
            failures.append("the turn after /reload never completed")
        else:
            print(f"post-reload turn completed after {t:.1f}s")
            if screen.has_line("after-reload-ok"):
                print("post-reload answer on screen")
            else:
                warnings.append("the model did not answer with after-reload-ok (see the final screen)")
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
        for bridge in bridge_pids() - bridges_before:
            try:
                os.kill(bridge, 9)
            except Exception:
                pass
        raw.close()

    print(f"capture: {os.path.getsize(RAW)} bytes in {RAW}")
    print("--- final screen ---")
    print(screen.show())
    for w in warnings:
        print("warning: " + w)
    if failures:
        print("--- FAIL ---")
        for f in failures:
            print(" - " + f)
        return 1
    print("--- PASS: chat, /reload, and a turn after reload all completed ---")
    return 0


if __name__ == "__main__":
    sys.exit(main())
