#!/usr/bin/env python3
"""Offline PTY check of inline internal-card style beside a native bash card."""
import importlib.util
import json
from pathlib import Path
import tempfile
import time
import uuid

spec = importlib.util.spec_from_file_location("inline_check", Path(__file__).with_name("tty-inline-tools.py"))
mod = importlib.util.module_from_spec(spec)
spec.loader.exec_module(mod)


def main():
    now = int(time.time() * 1000)
    with tempfile.TemporaryDirectory(prefix="cursor-style-") as folder:
        path = Path(folder) / "session.jsonl"
        rows = [
            {"type": "session", "version": 3, "id": str(uuid.uuid4()), "timestamp": "2026-11-12T00:00:00Z", "cwd": str(mod.ROOT)},
            {"type": "model_change", "id": "model", "parentId": None, "provider": "cursor", "modelId": "grok-4.7"},
            {"type": "message", "id": "user", "parentId": "model", "message": {"role": "user", "content": [{"type": "text", "text": "Review these tool cards."}], "timestamp": now}},
            {"type": "message", "id": "assistant", "parentId": "user", "message": {
                "role": "assistant", "provider": "cursor", "api": "pig-cursor-sdk", "model": "grok-4.7", "timestamp": now,
                "stopReason": "toolUse", "usage": {}, "content": [
                    {"type": "text", "text": "◇ cursor: shell · unmarked example"},
                    {"type": "text", "text": "Before internal call."},
                    {"type": "text", "textSignature": mod.SIGNATURE,
                     "text": "\n\n<!--pig-cursor-sdk:internal-tool:v1-->\n\n◇ cursor: shell · echo internal\n\n"},
                    {"type": "text", "text": "After internal call."},
                    {"type": "toolCall", "id": "native-call", "name": "bash", "arguments": {"command": "echo native"}},
                ]}},
            {"type": "message", "id": "result", "parentId": "assistant", "message": {
                "role": "toolResult", "toolCallId": "native-call", "toolName": "bash", "isError": False,
                "timestamp": now, "content": [{"type": "text", "text": "native-output"}]}},
        ]
        path.write_text("".join(json.dumps(row) + "\n" for row in rows))
        terminal = mod.Terminal(["--session", str(path)])
        try:
            def drawn():
                lines = terminal.grid.display
                matches = [i for i, line in enumerate(lines) if "◇ cursor: shell · echo internal" in line]
                if not matches or "native-output" not in terminal.text() or "unmarked example" not in terminal.text():
                    return False
                row = matches[0]
                bg = terminal.grid.buffer[row][3].bg
                return row > 0 and row + 1 < mod.ROWS and bg != "default" and terminal.grid.buffer[row - 1][3].bg == bg and terminal.grid.buffer[row + 1][3].bg == bg

            assert terminal.until(drawn, 60), terminal.text()
            assert "<!--pig-cursor" not in terminal.text(), "display marker leaked"
            lines = terminal.grid.display
            row = next(i for i, line in enumerate(lines) if "◇ cursor: shell · echo internal" in line)
            ordinary = next(i for i, line in enumerate(lines) if "unmarked example" in line)
            assert terminal.grid.buffer[ordinary][3].bg == "default", "unmarked ordinary text got internal-card styling"
            bg = terminal.grid.buffer[row][3].bg
            for i in (row - 1, row, row + 1):
                print(f"row {i}: bg={terminal.grid.buffer[i][3].bg} text={lines[i].strip()!r}")
            assert bg != "default", "internal item has no card background"
            assert terminal.grid.buffer[row - 1][3].bg == bg and terminal.grid.buffer[row + 1][3].bg == bg, "padding is outside card background"
            screen = terminal.text()
            assert screen.count("◇ cursor: shell · echo internal") == 1, "marked card was duplicated"
            assert screen.index("Before internal call.") < screen.index("◇ cursor: shell · echo internal") < screen.index("After internal call."), "card changed chat order"
            assert "echo native" in terminal.text(), "native bash card not drawn"
            print("PASS: padded internal card inline; native bash card and output unchanged (no model call)")
        finally:
            terminal.stop()


if __name__ == "__main__":
    main()
