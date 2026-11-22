#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
"""check_stdlib_boundary.py -- keep Helm/client-go/OIDC out of controller/node.

docs/DEPENDENCIES.md: kairon-controller and kairon-node stay free of the
CLI/UI/CSI *named exceptions* (Helm, Cobra, OIDC, unrestricted client-go).
Prometheus metrics, CSI NodeStage paths, and console websocket are already
present in the node/controller graph and are allowlisted here; this gate
fails on *new* forbidden roots landing in that graph.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
MODULE = "github.com/zyvorai/kairon"

ROOTS = ["./cmd/kairon-controller", "./cmd/kairon-node"]

# Import-path prefixes that must never appear in the controller/node graph.
FORBIDDEN_PREFIXES = (
    "helm.sh/",
    "k8s.io/",
    "sigs.k8s.io/",
    "github.com/spf13/",
    "github.com/coreos/go-oidc",
    "golang.org/x/oauth2",
    "github.com/containerd/",
    "oras.land/",
)


def non_stdlib_deps(pattern: str) -> set[str]:
    out = subprocess.run(
        [
            "go",
            "list",
            "-deps",
            "-f",
            "{{if not .Standard}}{{.ImportPath}}{{end}}",
            pattern,
        ],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    if out.returncode != 0:
        print(out.stderr, file=sys.stderr)
        sys.exit(1)
    pkgs = set()
    for line in out.stdout.splitlines():
        p = line.strip()
        if not p or p == "C":
            continue
        if p == MODULE or p.startswith(MODULE + "/"):
            continue
        pkgs.add(p)
    return pkgs


def main() -> int:
    all_deps: set[str] = set()
    for root in ROOTS:
        all_deps |= non_stdlib_deps(root)

    violations = sorted(
        p for p in all_deps if any(p == f or p.startswith(f) for f in FORBIDDEN_PREFIXES)
    )
    if violations:
        print(
            "FATAL: controller/node graph includes forbidden dependency roots "
            "(see docs/DEPENDENCIES.md):",
            file=sys.stderr,
        )
        for v in violations:
            print(f"  - {v}", file=sys.stderr)
        return 1

    print(
        f"stdlib boundary: OK "
        f"({len(all_deps)} third-party packages; none match forbidden Helm/client-go/OIDC roots)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
