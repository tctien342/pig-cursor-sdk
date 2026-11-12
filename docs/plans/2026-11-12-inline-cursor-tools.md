# Inline Cursor Tools Implementation Plan

> **For agentic workers:** Use todo tools to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Put internal tool annotations between the assistant blocks that surround the actual tool event, with one running hint above the editor.

**Architecture:** Emit bounded plain-text annotation blocks at tool-start events, never executable toolCall blocks. Mark generated blocks with a namespaced textSignature for exclusion from Cursor replay. Retain the entry renderer for old sessions but stop writing new custom entries. The live widget shows only the newest unfinished call.

**Tech Stack:** Go, pig Go SDK, Python PTY/pyte.

## Global Constraints
- User selected inline assistant text, accepting model-visible lines and no card background.
- Do not execute internal tools through pig.
- Do not put completed call lists in widgets or write duplicate end-of-turn entries.
- Run only tests related to changed code.

### Task 1: Inline event order and replay
**Files:** `internal/provider/emit.go`, `cursortools.go`, `picontext.go`, `engine.go`, corresponding provider tests.
**Interfaces:** emitter `add` consumes tagged Cursor tool events; generated text blocks use `textSignature: pig-cursor-sdk:internal-tool:v1`; replay skips only that exact signature on assistant text.
- [x] Add tests for text → call → thinking → call → text, duplicate events, failures, disabled display, terminal-only answer, and host-tool invariants.
- [x] Implement annotated blocks at start, outcome notes only for failure/unfinished; close preceding text/thinking before opening the annotation.
- [x] Ensure annotations do not count as substantive text for terminal answer fallback.
- [x] Strip only marked assistant blocks from Cursor replay; preserve lookalike prose and user messages.

### Task 2: Single running hint and verification
**Files:** `internal/provider/cursortools.go`, `extension.go`, `dev/tty-live-tools.py`, README and display contract.
**Interfaces:** `snapshot() []LiveToolView` returns zero or one newest unfinished call; `LiveTools(ctx,nil)` removes the widget.
- [x] Test multiple/concurrent calls, newest selection, completed exclusion and empty removal.
- [x] Remove runtime custom entry persistence and the accumulated widget list.
- [x] Run focused provider tests and a single PTY check for inline event position, one running hint, persistence of textSignature and no custom entries.
- [x] Update docs to distinguish plain inline annotations from legacy cards.
- [x] Review diff and validation before completion; do not claim native card styling.
