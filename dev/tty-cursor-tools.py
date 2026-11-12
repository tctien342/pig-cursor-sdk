#!/usr/bin/env python3
"""Compatibility entry point for the inline-tool display check."""
from pathlib import Path
import runpy

if __name__ == "__main__":
    runpy.run_path(str(Path(__file__).with_name("tty-inline-tools.py")), run_name="__main__")
