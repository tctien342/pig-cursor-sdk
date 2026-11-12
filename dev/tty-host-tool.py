#!/usr/bin/env python3
"""One live turn proving an advertised pig bash call remains a native tool call."""
import argparse
import importlib.util
from pathlib import Path
import tempfile

spec = importlib.util.spec_from_file_location("inline_check", Path(__file__).with_name("tty-inline-tools.py"))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--error", action="store_true", help="Verify an ordinary native tool failure")
    args = parser.parse_args()
    marker = "pig-native-error-proof" if args.error else "pig-native-proof"
    with tempfile.TemporaryDirectory(prefix="cursor-host-tool-") as folder:
        prompt = ("Use the advertised pig bash custom tool exactly once, not Cursor's own shell, "
                  "to run `printf pig-native-error-proof; exit 7`. Briefly acknowledge the failure; do not retry.") if args.error else (
                  "Use the advertised pig bash custom tool, not Cursor's own shell, to run `printf pig-native-proof`. Then briefly report its output.")
        terminal = mod.Terminal(["--session-dir", folder, "--model", "cursor/grok-4.7", "--thinking", "low", "--tools", "bash", prompt], agent_dir=Path(folder) / "agent")
        try:
            def done():
                _, rows = mod.entries(folder)
                return any(row.get("message", {}).get("role") == "toolResult" for row in rows) and any(
                    row.get("message", {}).get("role") == "assistant" and row["message"].get("stopReason") == "stop" for row in rows)
            assert terminal.until(done, 180), terminal.text()
            _, rows = mod.entries(folder)
            calls = [block for row in rows for block in row.get("message", {}).get("content", [])
                     if isinstance(block, dict) and block.get("type") == "toolCall"]
            results = [row["message"] for row in rows if row.get("message", {}).get("role") == "toolResult"]
            assert len(calls) == 1 and calls[0]["name"] == "bash", calls
            assert calls[0]["id"] == results[0]["toolCallId"], "native result did not match call"
            assert any(marker in str(r.get("content")) and bool(r.get("isError")) == args.error for r in results), results
            assert not any(block.get("textSignature") == mod.SIGNATURE and marker in block.get("text", "")
                           for row in rows for block in row.get("message", {}).get("content", []) if isinstance(block, dict)), "pig call was converted to internal annotation"
            print("PASS: one real pig bash toolCall + matching " + ("error" if args.error else "successful") + " toolResult; normal assistant completion; no internal-tool conversion")
        finally:
            terminal.stop()


if __name__ == "__main__":
    main()
