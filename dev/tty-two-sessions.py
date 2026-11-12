#!/usr/bin/env python3
"""Does one Cursor session ending break another session that is still open?

Two pig runs share a working directory, so they share everything Cursor keys on
it: the extension's agent registry and ~/.cursor/projects/<cwd>/sdk-agent-store.
With --no-session both runs compute the same registry key, which is the worst
case and the one that makes the mechanism visible.

Three scenarios, one per round:

1. the other session ends cleanly — its shutdown calls CloseAgent;
2. the other session is killed mid-turn — no shutdown, so a bridge is left
   holding a run upstream;
3. both sessions open the SAME session file, which is what makes two processes
   want the same Cursor agent.

In both, the session that stayed open must still answer afterwards.

    dev/tty-two-sessions.py

Exit 0 only when the open session answered on every turn of every round.
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
MODEL = "cursor/grok-4.7"


# Arithmetic, not "repeat this phrase": a model that suspects a jailbreak refuses
# the prompt, and the harness then reports a broken session that is not broken.
ASK_B1, ANSWER_B1 = "What is 17 plus 4? Answer with just the number.", "21"
ASK_B2, ANSWER_B2 = "What is 3 times 9? Answer with just the number.", "27"
ASK_A, ANSWER_A = "What is 5 plus 5? Answer with just the number.", "10"
TOOL_PROMPT_A = [
    "Run the bash command `sleep 12 && echo done-a`, then tell me the single word it printed."
]
ANSWER_A_TOOL = "done-a"


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

    def has_line(self, needle):
        return any(l.strip() == needle for l in self.lines())

    def dump(self, keep=16):
        rows = [l for l in self.lines() if l.strip()]
        return "\n".join(rows[-keep:])


def spawn(args, extra_env=None):
    env = dict(os.environ, CURSOR_API_KEY=key(), TERM="xterm-256color", PI_CURSOR_GO_DEBUG="1")
    if extra_env:
        env.update(extra_env)
    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(ROOT)
        os.execvpe("pig", ["pig", "-e", ".", "--model", MODEL] + args, env)
        os._exit(127)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
    return pid, fd


def pump(fd, screen, seconds, stop=None):
    t0 = time.time()
    while time.time() - t0 < seconds:
        r, _, _ = select.select([fd], [], [], 0.3)
        if r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                return True
            if not data:
                return True
            screen.feed(data)
        if stop and stop():
            return True
    return False


def kill(pid):
    try:
        os.kill(pid, 9)
        os.waitpid(pid, 0)
    except Exception:
        pass


def one_shot(prompt, timeout=150):
    """A print-mode run in this directory; returns the text it printed."""
    pid, fd = spawn(["--no-session", "-p", prompt])
    screen = Screen()
    done = pump(fd, screen, timeout, stop=lambda: not screen.lines()[-1:] or _settled(screen))
    if not done:
        pump(fd, screen, 5)
    kill(pid)
    rows = [l.strip() for l in screen.lines() if l.strip()]
    return " | ".join(rows[-6:])


def _settled(screen):
    rows = [l.strip() for l in screen.lines() if l.strip()]
    if not rows:
        return False
    last = rows[-1]
    return "Working" not in last and bool(rows)


def open_session(first_prompt, args=("--no-session",)):
    pid, fd = spawn(list(args) + [first_prompt])
    return pid, fd, Screen()


def log_slice(offset):
    try:
        with open(LOG, "rb") as f:
            f.seek(offset)
            return f.read().decode("utf-8", "replace")
    except OSError:
        return ""


def story(text):
    keep = re.compile(
        r"agent decision|already has active|agent closed|resume failed|run failed|request failed"
        r"|turn complete|diverged|reap|owns this conversation|cancelled the run|replaying transcript"
    )
    out = []
    for line in text.splitlines():
        if keep.search(line):
            m = re.search(r"pid=(\d+).*?level=(\w+) msg=\"([^\"]+)\"(.*)", line)
            if m:
                out.append(f"  pid={m.group(1)[-5:]:>5} {m.group(2)[:4]} {m.group(3)}{re.sub(' ext=pig-cursor-sdk', '', m.group(4))[:110]}")
            else:
                out.append("  " + line[:150])
    return out


def session_files():
    root = os.path.expanduser("~/.pig/agent/sessions")
    found = set()
    for base, _, names in os.walk(root):
        for name in names:
            if name.endswith(".jsonl"):
                found.add(os.path.join(base, name))
    return found


def main():
    if os.environ.get("CURSOR_API_KEY") is None:
        os.environ["CURSOR_API_KEY"] = key()
    failures = []
    before = bridge_pids()
    log0 = os.path.getsize(LOG) if os.path.exists(LOG) else 0

    for scenario in ("clean-exit", "killed-mid-turn", "same-session-file"):
        print(f"\n===== scenario: the other session {scenario} =====")
        extra = [] if scenario == "same-session-file" else ["--no-session"]
        files_before = session_files()
        bpid, bfd, bs = open_session(ASK_B1, args=extra)
        try:
            if not pump(bfd, bs, 120, stop=lambda: bs.has_line(ANSWER_B1)):
                failures.append(f"[{scenario}] B's first turn never answered")
                print(bs.dump())
                continue
            print("B turn 1 answered")

            if scenario == "same-session-file":
                # The second process opens the file the first one is writing, so the
                # two share a conversation identity — the case the agent registry has
                # to keep apart.
                new_files = sorted(session_files() - files_before, key=os.path.getmtime)
                if not new_files:
                    failures.append("[same-session-file] no session file was written")
                    continue
                path = new_files[-1]
                print("B's session file: " + os.path.basename(path))
                apid, afd = spawn(["--session", path, "-p", ASK_A])
                ascreen = Screen()
                pump(afd, ascreen, 150, stop=lambda: ascreen.has_line(ANSWER_A))
                kill(apid)
                print("A answered on the same session file and exited")
                os.write(bfd, (ASK_B2 + "\r").encode())
                if not pump(bfd, bs, 150, stop=lambda: bs.has_line(ANSWER_B2)):
                    failures.append("[same-session-file] B's turn after A ran never answered")
                    print(bs.dump(24))
                else:
                    print("B turn 2 answered")
                continue

            apid, afd = spawn(["--no-session", "-p"] + TOOL_PROMPT_A)
            ascreen = Screen()
            # Let A get its tool call running, then end it one way or the other.
            pump(afd, ascreen, 14)
            if scenario != "killed-mid-turn":
                pump(afd, ascreen, 150, stop=lambda: ANSWER_A_TOOL in " ".join(ascreen.lines()))
                kill(apid)
                print("A finished and exited")
            else:
                kill(apid)
                print("A was killed mid-turn")

            time.sleep(2)
            os.write(bfd, (ASK_B2 + "\r").encode())
            if not pump(bfd, bs, 150, stop=lambda: bs.has_line(ANSWER_B2)):
                failures.append(f"[{scenario}] B's turn after A ended never answered")
                print("--- B screen ---")
                print(bs.dump(24))
            else:
                print("B turn 2 answered")
        finally:
            kill(bpid)

    for bridge in bridge_pids() - before:
        try:
            os.kill(bridge, 9)
        except Exception:
            pass

    print("\n--- what the extension logged ---")
    for line in story(log_slice(log0))[-26:]:
        print(line)

    if failures:
        print("--- FAIL ---")
        for f in failures:
            print(" - " + f)
        return 1
    print("--- PASS: an open Cursor session survives another one ending ---")
    return 0


if __name__ == "__main__":
    sys.exit(main())
