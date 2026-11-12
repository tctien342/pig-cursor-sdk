#!/usr/bin/env python3
"""Live Escape and /cursor-abort checks; two model turns per selected method."""
import argparse
import importlib.util
import os
from pathlib import Path
import tempfile

spec = importlib.util.spec_from_file_location("inline_check", Path(__file__).with_name("tty-inline-tools.py"))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


def validate_abort_history(rows):
    users = [i for i, row in enumerate(rows) if row.get("message", {}).get("role") == "user"]
    assert len(users) == 2, "unexpected user request count"
    interrupted = [i for i, row in enumerate(rows) if users[0] < i < users[1] and row.get("message", {}).get("role") == "assistant"]
    errors = [(i, row["message"]) for i, row in enumerate(rows) if row.get("message", {}).get("stopReason") == "error"]
    if errors:
        assert len(errors) == 1 and interrupted == [errors[0][0]], "error is not the sole interrupted assistant"
        message = errors[0][1]
        assert message.get("role") == "assistant" and message.get("provider") == "cursor" and message.get("api") == "pig-cursor-sdk" and message.get("errorMessage") == "context canceled", "unexpected assistant/provider error"
    return bool(errors)


def check(method):
    with tempfile.TemporaryDirectory(prefix="cursor-cancel-") as folder:
        log_path = Path(folder) / "provider.log"
        os.environ["PI_CURSOR_GO_DEBUG"] = "1"
        os.environ["PI_CURSOR_GO_LOG"] = str(log_path)
        prompt = "Use your built-in shell to run `sleep 60 && printf cursor-slow-done`. Wait for it before answering."
        terminal = mod.Terminal(["--session-dir", folder, "--model", "cursor/grok-4.7", "--thinking", "low", "--no-tools", prompt], agent_dir=Path(folder) / "agent")
        try:
            def running():
                return any("◇ cursor:" in line and "running" in line and "sleep 60" in line for line in terminal.grid.display)
            assert terminal.until(running, 150), terminal.text()
            # Hint removal precedes the bounded RPC. A doctor snapshot taken at
            # that instant is static and may legitimately show cancellation pending.
            # The private per-fixture log offset correlates the completion barrier.
            offset = log_path.stat().st_size if log_path.exists() else 0
            def outcomes():
                if not log_path.exists():
                    return []
                with log_path.open("rb") as log:
                    log.seek(offset)
                    return [line for line in log.read().decode("utf-8", "replace").splitlines() if "cursor cancellation outcome" in line]
            os.write(terminal.fd, b"\x1b" if method == "escape" else b"/cursor-abort\r")
            assert terminal.until(lambda: bool(outcomes()), 12), terminal.text()
            assert any("confirmed=true" in line or 'terminal=CANCELLED' in line for line in outcomes()), outcomes()
            if method == "command":
                assert terminal.until(lambda: "Cursor acknowledged cancellation" in terminal.text(), 12), terminal.text()
            assert terminal.until(lambda: not any("◇ cursor:" in line and "running" in line for line in terminal.grid.display), 12), terminal.text()
            os.write(terminal.fd, b"/cursor-doctor\r")
            def acknowledged():
                return "Cursor doctor" in terminal.text() and any("run: cancellation acknowledged upstream" in line or "run: cancelled" in line or "run: canceled" in line for line in terminal.grid.display)
            assert terminal.until(acknowledged, 20), terminal.text()
            print("PASS: " + method + " clears the hint; completed RPC and fresh doctor confirm upstream cancellation", flush=True)
            os.write(terminal.fd, b"What is 6 times 7? Answer with just the number.\r")
            def answered():
                _, rows = mod.entries(folder)
                users = [row for row in rows if row.get("message", {}).get("role") == "user"]
                return len(users) == 2 and any(row.get("message", {}).get("role") == "assistant" and row["message"].get("stopReason") == "stop" and any("42" in block.get("text", "") for block in row["message"].get("content", []) if isinstance(block, dict)) for row in rows)
            assert terminal.until(answered, 150), terminal.text()
            _, rows = mod.entries(folder)
            # Permit only the sole interrupted Cursor assistant's exact SDK
            # record, never an error from another provider or a continuation.
            if validate_abort_history(rows):
                print("LIMIT: host/Go SDK stores the initial abort as error: context canceled", flush=True)
            print("PASS: " + method + " permits the next user turn; no stale-run error or duplicate user request", flush=True)
        finally:
            terminal.stop()
            if log_path.exists():
                for line in log_path.read_text().splitlines():
                    if "cursor request capture" in line or "cursor cancellation boundary" in line or "cursor cancellation outcome" in line:
                        print("TRACE: " + line, flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--method", choices=["escape", "command", "both"], default="both")
    args = parser.parse_args()
    for method in (["escape", "command"] if args.method == "both" else [args.method]):
        check(method)


if __name__ == "__main__":
    main()
