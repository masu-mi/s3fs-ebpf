#!/usr/bin/env python3
"""Check repo-relative paths in docs: markdown links [x](path) and
backticked `docs/...` / `scripts/...` style paths.  No network, no root.

  python3 scripts/check-doc-links.py          # report, exit 1 if any broken
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
LINK = re.compile(r'\]\(([^)\s]+?)(?:#[^)]*)?\)')
TICK = re.compile(r"`(docs/[A-Za-z0-9_./-]+\.[A-Za-z0-9]+)`")

def allowed():
    p = os.path.join(ROOT, "scripts", "check-doc-links.allow")
    if not os.path.exists(p):
        return set()
    return {l.strip() for l in open(p, encoding="utf-8") if l.strip() and not l.startswith("#")}

def md_files():
    for base in ("docs", "."):
        for d, dirs, files in os.walk(os.path.join(ROOT, base)):
            dirs[:] = [x for x in dirs if x not in (".git", "node_modules", "data", ".claude", "worktrees")]
            if base == "." and d != ROOT:
                continue
            for f in files:
                if f.endswith(".md"):
                    yield os.path.join(d, f)

def main():
    bad = 0
    allow = allowed()
    seen = set()
    for p in md_files():
        if p in seen:
            continue
        seen.add(p)
        here = os.path.dirname(p)
        for n, line in enumerate(open(p, encoding="utf-8"), 1):
            for m in LINK.finditer(line):
                t = m.group(1)
                if re.match(r'^[a-z]+:', t) or t.startswith("#"):
                    continue
                if not os.path.exists(os.path.normpath(os.path.join(here, t))):
                    print(f"{os.path.relpath(p, ROOT)}:{n}: link {t}")
                    bad += 1
            for m in TICK.finditer(line):
                t = m.group(1)
                if "*" in t or "<" in t or t in allow:
                    continue
                if not os.path.exists(os.path.join(ROOT, t)):
                    print(f"{os.path.relpath(p, ROOT)}:{n}: path {t}")
                    bad += 1
    print(f"{bad} broken")
    return 1 if bad else 0

sys.exit(main())
