#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0
"""Render bench.py JSON results as the Markdown tables used in
docs/benchmarks/kairon-vs-kubevirt.md.

  report.py docs/benchmarks/kairon-2026-10-04.json docs/benchmarks/kubevirt-2026-10-04.json
"""

import json
import sys


def ms(v):
    return "-" if v is None else f"{v / 1000:.1f} s"


def mib(kib):
    return "-" if kib is None else f"{kib / 1024:.0f} MiB"


def main(paths):
    runs = [json.load(open(p)) for p in paths]
    print("| | " + " | ".join(f"{r['platform']} {r['version']}".strip() for r in runs) + " |")
    print("|---|" + "---|" * len(runs))
    print("| Control plane RSS (idle) | " + " | ".join(mib(r["control_plane_idle"]["rss_kib"]) for r in runs) + " |")
    print("| Control plane CPU (idle) | " + " | ".join(f"{r['control_plane_idle']['cpu_millicores']} m" for r in runs) + " |")
    sizes = sorted({d["n"] for r in runs for d in r["density"]})
    for n in sizes:
        rows = {"create to Running, p50": [], "create to SSH ready, p50": [], "create to SSH ready, max": [],
                "host RSS per VM": [], "VMs ready": []}
        for r in runs:
            d = next((x for x in r["density"] if x["n"] == n), None)
            if d is None:
                for k in rows:
                    rows[k].append("-")
                continue
            rows["create to Running, p50"].append(ms(d["running_ms_p50"]))
            rows["create to SSH ready, p50"].append(ms(d["ready_ms_p50"]))
            rows["create to SSH ready, max"].append(ms(d["ready_ms_max"]))
            # Only meaningful once every VM is up and the baseline was clean.
            per_vm = d["per_vm_kib"] if d["ready"] == d["n"] and (d["per_vm_kib"] or 0) > 0 else None
            rows["host RSS per VM"].append(mib(per_vm))
            rows["VMs ready"].append(f"{d['ready']}/{d['n']}")
        for k, vals in rows.items():
            print(f"| N={n}: {k} | " + " | ".join(vals) + " |")
    for r in runs:
        for d in r["density"]:
            if d.get("migration"):
                print(f"\n{r['platform']} live migration at N=1: {d['migration']}")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    main(sys.argv[1:])
