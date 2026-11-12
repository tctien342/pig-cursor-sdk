#!/usr/bin/env python3
"""Refresh internal/provider/fallback_catalog.json from a captured Cursor catalog.

Usage: scripts/gen-fallback-catalog.py [cursor-sdk-model-list.json]

The snapshot is public model metadata only, embedded so `pig --list-models
cursor` works offline before any network call. It is not a secret and never
contains keys.
"""
import json, sys, os

src = sys.argv[1] if len(sys.argv) > 1 else os.path.expanduser("~/.pig/agent/cursor-sdk-model-list.json")
doc = json.load(open(src))
items = doc["models"] if isinstance(doc, dict) else doc
keep = {"id", "displayName", "description", "parameters", "variants"}
out = [{k: v for k, v in m.items() if k in keep} for m in items]
dst = os.path.join(os.path.dirname(__file__), "..", "internal", "provider", "fallback_catalog.json")
open(dst, "w").write(json.dumps(out, indent=1, sort_keys=True))
print(f"wrote {len(out)} model entries -> {os.path.normpath(dst)}")
