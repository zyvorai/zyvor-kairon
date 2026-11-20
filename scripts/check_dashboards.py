#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0
"""check_dashboards.py -- validates charts/kairon/dashboards/*.json.

Each file must be valid JSON with a unique uid, a title and panels, and every
`kairon_*` metric name used in a panel query must be registered in
internal/metrics (histogram _bucket/_count/_sum suffixes allowed). A dashboard
that queries a metric nobody exports renders empty panels forever, so this
fails the build instead.
"""
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
dash_dir = ROOT / "charts/kairon/dashboards"
errors = []

known = set()
for f in (ROOT / "internal/metrics").glob("*.go"):
    if f.name.endswith("_test.go"):
        continue
    known.update(re.findall(r'"(kairon_[a-z0-9_]+)"', f.read_text()))
if not known:
    sys.exit("no metric names found under internal/metrics")

files = sorted(dash_dir.glob("*.json"))
if not files:
    sys.exit(f"no dashboards in {dash_dir}")

uids = set()
used_total = set()
for f in files:
    try:
        d = json.loads(f.read_text())
    except ValueError as e:
        errors.append(f"{f.name}: invalid JSON: {e}")
        continue
    for key in ("uid", "title", "panels"):
        if not d.get(key):
            errors.append(f"{f.name}: missing {key}")
    if d.get("uid") in uids:
        errors.append(f"{f.name}: duplicate uid {d.get('uid')}")
    uids.add(d.get("uid"))
    exprs = []
    for p in d.get("panels", []):
        for t in p.get("targets", []):
            exprs.append(t.get("expr", ""))
    for v in d.get("templating", {}).get("list", []):
        q = v.get("query")
        if isinstance(q, dict):
            q = q.get("query", "")
        if isinstance(q, str):
            exprs.append(q)
    if not any(exprs):
        errors.append(f"{f.name}: no queries")
    for e in exprs:
        for name in re.findall(r"\bkairon_[a-z0-9_]+", e):
            base = re.sub(r"_(bucket|count|sum)$", "", name)
            if name not in known and base not in known:
                errors.append(f"{f.name}: unknown metric {name!r} in {e!r}")
            used_total.add(base if base in known else name)

if errors:
    print("\n".join(errors), file=sys.stderr)
    sys.exit(1)
print(f"dashboards OK: {len(files)} files, {len(used_total)} distinct metrics, all registered in internal/metrics")
