#!/usr/bin/env python3
"""Offline PTY check: doctor/fast commands, no model turn or bridge startup."""
import importlib.util
import os
from pathlib import Path
import tempfile

spec = importlib.util.spec_from_file_location("inline_check", Path(__file__).with_name("tty-inline-tools.py"))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


def main():
    os.environ["CURSOR_API_KEY"] = "unused-offline-doctor-test"
    os.environ["PI_CURSOR_PLAN_USAGE"] = "0"
    os.environ["PI_CURSOR_PLAN_BAR"] = "0"
    with tempfile.TemporaryDirectory(prefix="cursor-doctor-") as folder:
        terminal = mod.Terminal(["--session-dir", folder, "--model", "cursor/grok-4.7@500k:fast", "--no-tools"], agent_dir=Path(folder) / "agent")
        try:
            assert terminal.until(lambda: "cursor ·" in terminal.text(), 90), terminal.text()
            os.write(terminal.fd, b"/cursor-doctor\r")
            assert terminal.until(lambda: "Cursor doctor" in terminal.text() and "bridge: not started" in terminal.text(), 30), terminal.text()
            assert "pig tools: 0 active" in terminal.text(), terminal.text()
            print("PASS: actual doctor command reports no bridge and zero active tools")
            os.write(terminal.fd, b"/cursor-fast off\r")
            assert terminal.until(lambda: "Cursor selection:" in terminal.text() and ":slow" in terminal.text(), 20), terminal.text()
            os.write(terminal.fd, b"/cursor-doctor --probe\r")
            assert terminal.until(lambda: "selected: grok-4.7@500k:slow" in terminal.text() and "skipped: no bridge" in terminal.text(), 20), terminal.text()
            assert "fast=false" in terminal.text(), terminal.text()
            _, rows = mod.entries(folder)
            assert not any(row.get("message", {}).get("role") in ("user", "assistant") for row in rows), "command accidentally became a model turn"
            print("PASS: fast command changes registered model/parameters; probe skips startup; no user/model messages")
        finally:
            terminal.stop()


if __name__ == "__main__":
    main()
