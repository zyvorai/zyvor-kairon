#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
"""Rewrite hardware-matrix rows in docs/COMPATIBILITY.md.

stdin: one "title<TAB>result<TAB>notes" line per case (from
hardware-migration-matrix.sh). A table row is updated when its first cell,
with backticks removed, starts with the title; its own first cell is kept so
any extra qualifier in the doc survives. Unmatched titles are an error, so a
renamed row can't silently stop being updated.
"""

from __future__ import annotations

import sys
from pathlib import Path


def norm(s: str) -> str:
    return s.replace("`", "").strip()


def main() -> int:
    path, date = Path(sys.argv[1]), sys.argv[2]
    results = []
    for line in sys.stdin.read().splitlines():
        if line.strip():
            title, result, notes = (line.split("\t") + ["", ""])[:3]
            results.append((title, result, notes))

    lines = path.read_text().splitlines()
    unmatched = []
    for title, result, notes in results:
        for i, row in enumerate(lines):
            if not row.startswith("|"):
                continue
            cells = [c.strip() for c in row.strip("|").split("|")]
            if len(cells) == 4 and norm(cells[0]).startswith(norm(title)):
                notes_cell = notes.replace("|", "\\|")
                lines[i] = f"| {cells[0]} | {result} | {date} | {notes_cell} |"
                break
        else:
            unmatched.append(title)
    if unmatched:
        print("no COMPATIBILITY.md row for: " + ", ".join(unmatched), file=sys.stderr)
        return 1
    path.write_text("\n".join(lines) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
