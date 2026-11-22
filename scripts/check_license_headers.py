#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
"""check_license_headers.py -- Go/Python/shell/TypeScript/workflow sources carry Zyvor headers."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MARKER = "Copyright 2026 Zyvor"
SPDX = "SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0"

CHECK_SUFFIXES = {".go", ".py", ".sh", ".ts", ".tsx"}
# Also check workflow YAML (short header expected).
CHECK_WORKFLOW_PREFIX = ".github/workflows/"


def tracked_files() -> list[Path]:
    out = subprocess.run(
        ["git", "ls-files"],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=True,
    )
    files = []
    for line in out.stdout.splitlines():
        rel = line.strip()
        if not rel:
            continue
        if "/node_modules/" in rel or rel.startswith("vendor/"):
            continue
        suf = Path(rel).suffix.lower()
        if suf in CHECK_SUFFIXES or (
            rel.startswith(CHECK_WORKFLOW_PREFIX) and suf in {".yml", ".yaml"}
        ):
            files.append(ROOT / rel)
    return files


def check_file(path: Path) -> str | None:
    try:
        text = path.read_text(encoding="utf-8", errors="replace")
    except OSError as e:
        return f"{path.relative_to(ROOT)}: read error: {e}"
    head = "\n".join(text.splitlines()[:15])
    if MARKER not in head:
        return f"{path.relative_to(ROOT)}: missing '{MARKER}' in first 15 lines"
    if SPDX not in head:
        return f"{path.relative_to(ROOT)}: missing '{SPDX}' in first 15 lines"
    return None


def main() -> int:
    errors = []
    files = tracked_files()
    for f in files:
        err = check_file(f)
        if err:
            errors.append(err)
    if errors:
        print(f"FATAL: {len(errors)} license header issue(s):", file=sys.stderr)
        for e in errors[:40]:
            print(f"  - {e}", file=sys.stderr)
        if len(errors) > 40:
            print(f"  ... and {len(errors) - 40} more", file=sys.stderr)
        return 1
    print(f"license headers: OK ({len(files)} files checked)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
