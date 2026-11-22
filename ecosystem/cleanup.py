#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
"""Dry-run by default: sweep expired ecosystem temporary VMs by UID."""
from __future__ import annotations
import argparse
import os
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sdk/python"))
from kairon import Client  # noqa: E402
from ci import EXPIRY_ANNOTATION, TEMP_LABEL  # noqa: E402


def expired(obj: dict, now: datetime, grace_seconds: int = 60) -> bool:
    if grace_seconds < 0:
        raise ValueError("grace must be non-negative")
    meta = obj.get("metadata", {})
    if meta.get("labels", {}).get(TEMP_LABEL) != "true" or not meta.get("uid") or meta.get("deletionTimestamp"):
        return False
    raw = meta.get("annotations", {}).get(EXPIRY_ANNOTATION)
    if not isinstance(raw, str):
        return False
    try:
        until = datetime.fromisoformat(raw.replace("Z", "+00:00"))
    except ValueError:
        return False
    return until.tzinfo is not None and until + timedelta(seconds=grace_seconds) < now


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", default="default")
    parser.add_argument("--delete", action="store_true", help="Request deletion; otherwise only show eligible names")
    parser.add_argument("--grace-seconds", type=int, default=60)
    args = parser.parse_args()
    if args.grace_seconds < 0:
        parser.error("grace must be non-negative")
    client = Client(os.environ["KAIRON_KUBE_URL"], os.environ["KAIRON_KUBE_TOKEN"], namespace=args.namespace,
                    ca_file=os.environ.get("KAIRON_KUBE_CA_FILE"))
    for obj in client.machines.list(label_selector=TEMP_LABEL + "=true"):
        if expired(obj, datetime.now(timezone.utc), args.grace_seconds):
            meta = obj["metadata"]
            print(("delete requested " if args.delete else "would delete ") + meta["name"])
            if args.delete:
                client.machines.delete(meta["name"], uid=meta["uid"])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
