#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
"""Keep feature stability labels honest (see docs/STABILITY.md).

Reads docs/stability.json (the registry) and the result tables in
docs/COMPATIBILITY.md. A feature may be labelled "ga" only when it has rows
and every one of them passes; the README row must say "(experimental)" exactly
when the registry does. With --max-age-days N (the nightly job), a "ga"
feature's rows must also be dated within N days. Prints a summary table
(also written to $GITHUB_STEP_SUMMARY). Exit 1 on any violation.
"""
import argparse
import datetime
import json
import os
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def sections(text):
    """{section title: [(case, result, date)]} from `| Case | Last result | Date | Notes |` tables."""
    out, cur = {}, None
    for line in text.splitlines():
        m = re.match(r"^##\s+(.+?)\s*$", line)
        if m:
            cur = m.group(1)
            out.setdefault(cur, [])
            continue
        if cur is None or not line.startswith("|"):
            continue
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) < 3 or cells[0] in ("Case", "Component") or set(cells[0]) <= set("-: "):
            continue
        if cells[1].lower() in ("last result", "result"):
            continue
        out[cur].append((cells[0], cells[1].lower(), cells[2]))
    return out


def is_pass(result):
    return result.startswith("pass")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--max-age-days", type=int, default=None)
    args = ap.parse_args()

    reg = json.loads((ROOT / "docs" / "stability.json").read_text())
    compat = sections((ROOT / "docs" / "COMPATIBILITY.md").read_text())
    readme = (ROOT / "README.md").read_text()
    today = datetime.date.today()
    errors, rows_out = [], []

    for f in reg["features"]:
        rows = []
        for s in f["sections"]:
            if s not in compat:
                errors.append(f"{f['id']}: COMPATIBILITY.md has no section '{s}'")
                continue
            rows += compat[s]
        if f.get("rowFilter"):
            rows = [r for r in rows if re.search(f["rowFilter"], r[0], re.I)]
        passed = sum(is_pass(r[1]) for r in rows)
        label = f["label"]

        if label not in ("ga", "experimental"):
            errors.append(f"{f['id']}: label must be ga or experimental")
        if label == "ga":
            if not rows:
                errors.append(f"{f['id']}: labelled ga but has no COMPATIBILITY.md rows")
            for case, result, date in rows:
                if not is_pass(result):
                    errors.append(f"{f['id']}: labelled ga but row '{case}' is '{result}'")
                elif args.max_age_days is not None:
                    try:
                        age = (today - datetime.date.fromisoformat(date[:10])).days
                    except ValueError:
                        errors.append(f"{f['id']}: row '{case}' has no ISO date ('{date}')")
                        continue
                    if age > args.max_age_days:
                        errors.append(f"{f['id']}: row '{case}' is {age} days old (limit {args.max_age_days})")
        if not (ROOT / f["guide"]).exists():
            errors.append(f"{f['id']}: guide {f['guide']} is missing")
        if f.get("readme"):
            row = next((l for l in readme.splitlines() if l.startswith("| **") and f["readme"] in l.split("|")[1]), None)
            if row is None:
                errors.append(f"{f['id']}: no README features row for '{f['readme']}'")
            else:
                says = "(experimental" in row.split("|")[1]
                if says != (label == "experimental"):
                    errors.append(f"{f['id']}: README row says {'experimental' if says else 'ga'}, registry says {label}")
        rows_out.append((f["name"], label, passed, len(rows)))

    lines = ["| Feature | Label | Rows passing |", "|---|---|---|"]
    lines += [f"| {n} | {l} | {p}/{t} |" for n, l, p, t in rows_out]
    report = "\n".join(lines)
    print(report)
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a") as fh:
            fh.write("### Feature stability\n\n" + report + "\n")
    for e in errors:
        print(f"::error::{e}" if os.environ.get("GITHUB_ACTIONS") else f"ERROR: {e}", file=sys.stderr)
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
