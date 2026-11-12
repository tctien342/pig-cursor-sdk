#!/usr/bin/env python3
"""Focused live proof: inline position, one running hint, saved metadata, resume.

Run with dev/.venv/bin/python dev/tty-inline-tools.py. Uses an isolated session
folder, disables unrelated extensions/skills, and makes one Cursor model turn.
No unrelated session files are read or modified.
"""
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import signal
import struct
import tempfile
import termios
import time

import pyte

ROOT = Path(__file__).resolve().parent.parent
SIGNATURE = "pig-cursor-sdk:internal-tool:v1"
ROWS, COLS = 50, 120
PROMPT = ("First say 'Starting the check.' in normal text. Then run the shell command "
          "`sleep 6 && echo inline-done` using your own shell. After it returns, say "
          "'The check finished.' in normal text. Do not call any other tools.")


class Terminal:
    def __init__(self, args):
        env = dict(os.environ, TERM="xterm-256color")
        if not env.get("CURSOR_API_KEY"):
            auth = json.loads((Path.home() / ".pig/agent/auth.json").read_text())
            env["CURSOR_API_KEY"] = auth["cursor"]["key"]
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.chdir(ROOT)
            os.execvpe("pig", ["pig", "--no-extensions", "--no-skills",
                              "--no-prompt-templates", "-e", str(ROOT),
                              "--use-theme", "dark"] + args, env)
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))
        self.grid = pyte.Screen(COLS, ROWS)
        self.stream = pyte.ByteStream(self.grid)

    def text(self):
        return "\n".join(self.grid.display)

    def until(self, predicate, timeout):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            ready, _, _ = select.select([self.fd], [], [], 0.2)
            if ready:
                try:
                    data = os.read(self.fd, 65536)
                except OSError:
                    return False
                if not data:
                    return False
                self.stream.feed(data)
            if predicate():
                return True
        return False

    def stop(self):
        # Signal only our unreaped child, never a process group that might outlive
        # it. Ctrl+D avoids synthesizing a user/model turn during normal teardown.
        reaped = False

        def exited():
            nonlocal reaped
            try:
                reaped = os.waitpid(self.pid, os.WNOHANG)[0] != 0
            except ChildProcessError:
                reaped = True
            return reaped

        try:
            if not exited():
                os.write(self.fd, b"\x04")
                self.until(exited, 5)
            if not exited():
                os.kill(self.pid, signal.SIGTERM)
                self.until(exited, 5)
            if not exited():
                os.kill(self.pid, signal.SIGKILL)
                os.waitpid(self.pid, 0)
        finally:
            os.close(self.fd)


def entries(folder):
    paths = list(Path(folder).glob("*.jsonl"))
    if not paths:
        return None, []
    path = max(paths, key=lambda p: p.stat().st_mtime_ns)
    return path, [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def check_saved(folder):
    path, rows = entries(folder)
    if not path:
        return None
    assert not any(r.get("customType") == "cursor-tool" for r in rows), "duplicate custom entries"
    messages = [r["message"] for r in rows if r.get("type") == "message"
                and r.get("message", {}).get("role") == "assistant"]
    if not messages:
        return None
    message = messages[-1]
    assert message.get("stopReason") != "error", message.get("errorMessage")
    blocks = message.get("content", [])
    notes = [i for i, b in enumerate(blocks) if b.get("textSignature") == SIGNATURE]
    assert len(notes) == 1, f"expected one signed annotation, found {len(notes)}"
    i = notes[0]
    assert "sleep 6" in blocks[i].get("text", ""), "annotation missing command"
    before = " ".join(b.get("text", "") for b in blocks[:i] if b.get("type") == "text")
    after = " ".join(b.get("text", "") for b in blocks[i+1:] if b.get("type") == "text")
    assert "Starting the check." in before, "annotation is not after initial prose"
    assert "The check finished." in after, "annotation is not before following prose"
    assert not any(b.get("type") == "toolCall" for b in blocks), "internal tool became host execution"
    return path


def main():
    with tempfile.TemporaryDirectory(prefix="cursor-inline-session-") as folder:
        terminal = Terminal(["--session-dir", folder, "--model", "cursor/grok-4.7",
                             "--thinking", "low", "--no-tools", PROMPT])
        try:
            def running():
                lines = [line for line in terminal.grid.display if "◇ cursor:" in line]
                return any("running" in line and "sleep 6" in line for line in lines)
            assert terminal.until(running, 150), "no live running hint"
            live_lines = [line for line in terminal.grid.display
                          if "◇ cursor:" in line and "running" in line]
            assert len(live_lines) == 1, "more than one running hint"
            # One start annotation in chat, one currently-running hint near editor.
            print("PASS: one running hint during the call")

            assert terminal.until(lambda: bool(entries(folder)[1]) and any(
                r.get("type") == "message" and r.get("message", {}).get("role") == "assistant"
                for r in entries(folder)[1]), 150), "assistant message was not saved"
            path = check_saved(folder)
            assert path
            print("PASS: saved prose → signed tool annotation → prose; no custom entry or host call")

            def finished():
                lines = [line for line in terminal.grid.display if "◇ cursor:" in line]
                return len(lines) == 1 and "running" not in lines[0] and "The check finished." in terminal.text()
            assert terminal.until(finished, 10), "running hint was not cleared"
            screen = terminal.text()
            # Last occurrence excludes the user's prompt repeating the requested prose.
            assert screen.rfind("Starting the check.") < screen.index("◇ cursor:") < screen.rfind("The check finished."), "screen order differs from saved order"
            print("PASS: actual screen prose → annotation → prose; no completed-call widget list")
        finally:
            terminal.stop()

        reopened = Terminal(["--session", str(path), "--no-tools"])
        try:
            assert reopened.until(lambda: "◇ cursor:" in reopened.text() and "The check finished." in reopened.text(), 60), "resume did not redraw inline annotation"
            lines = [line for line in reopened.grid.display if "◇ cursor:" in line]
            assert len(lines) == 1 and "running" not in lines[0], "resume duplicates or stale hint"
            print("PASS: resume redraws one inline annotation, no widget")
        finally:
            reopened.stop()
    print("PASS: inline tool position and single running hint")


if __name__ == "__main__":
    main()
