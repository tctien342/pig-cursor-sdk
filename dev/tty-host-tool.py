#!/usr/bin/env python3
"""One live turn proving an advertised pig bash call remains a native tool call."""
import importlib.util
from pathlib import Path
import tempfile

spec = importlib.util.spec_from_file_location("inline_check", Path(__file__).with_name("tty-inline-tools.py"))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


def main():
    with tempfile.TemporaryDirectory(prefix="cursor-host-tool-") as folder:
        prompt = "Use the advertised pig bash custom tool, not Cursor's own shell, to run `printf pig-native-proof`. Then briefly report its output."
        terminal = mod.Terminal(["--session-dir", folder, "--model", "cursor/grok-4.7", "--thinking", "low", "--tools", "bash", prompt])
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
            assert any("pig-native-proof" in str(r.get("content")) and not r.get("isError") for r in results), results
            assert not any(block.get("textSignature") == mod.SIGNATURE and "pig-native-proof" in block.get("text", "")
                           for row in rows for block in row.get("message", {}).get("content", []) if isinstance(block, dict)), "pig call was converted to internal annotation"
            print("PASS: one real pig bash toolCall + matching successful toolResult; no internal-tool conversion")
        finally:
            terminal.stop()


if __name__ == "__main__":
    main()
