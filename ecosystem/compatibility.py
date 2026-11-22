#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
"""Opt-in real-cluster lifecycle qualification; never implies migration certification."""
from __future__ import annotations
import argparse
import json
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sdk/python"))
from kairon import APIError, Client, UIClient  # noqa: E402
from ci import temporary_manifest  # noqa: E402


def qualify(client: Client, manifest: dict, *, ui: UIClient | None = None, timeout: int = 180) -> dict:
    if not isinstance(timeout, int) or timeout <= 0:
        raise ValueError("timeout must be a positive integer")
    report = {"schemaVersion": 1, "timestamp": datetime.now(timezone.utc).isoformat(),
              "scope": "single-machine-lifecycle", "cases": [], "passed": False}
    obj = None
    def record(case, result):
        report["cases"].append({"name": case, "result": result})
    try:
        obj = client.machines.create(temporary_manifest(manifest, client.namespace, max(900, timeout * 4 + 120)))
        record("create", "pass")
        name, uid = obj["metadata"]["name"], obj["metadata"]["uid"]
        client.machines.wait(name, uid=uid, timeout=timeout)
        record("running-ready", "pass")
        if ui:
            result = ui.exec(name, ["/bin/echo", "kairon-compatible"])
            if result["exitCode"] != 0 or result["stdout"].strip() != "kairon-compatible":
                raise RuntimeError("guest exec returned unexpected output")
            record("guest-exec", "pass")
        else:
            record("guest-exec", "not-run")
        client.machines.power(name, "Stopped")
        client.machines.wait(name, uid=uid, phase="Stopped", ready=False, timeout=timeout)
        record("stop", "pass")
        client.machines.power(name, "Running")
        client.machines.wait(name, uid=uid, timeout=timeout)
        record("restart-ready", "pass")
    except Exception as exc:
        record("operation", "fail:" + type(exc).__name__)
    finally:
        if obj:
            try:
                name, uid = obj["metadata"]["name"], obj["metadata"]["uid"]
                client.machines.delete(name, uid=uid)
                deadline = time.monotonic() + timeout
                while True:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        raise TimeoutError("deletion did not complete")
                    try:
                        current = client.machines.get(name, timeout=min(remaining, 30))
                        if current["metadata"]["uid"] != uid:
                            break  # original object has gone; never touch a replacement
                    except APIError as exc:
                        if exc.status == 404:
                            break
                        raise
                    time.sleep(min(1, max(0, deadline - time.monotonic())))
                record("delete-complete", "pass")
            except Exception as exc:
                record("delete-complete", "fail:" + type(exc).__name__)
    report["passed"] = bool(report["cases"]) and not any(c["result"].startswith("fail:") for c in report["cases"])
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", action="store_true", help="Explicitly create, stop, restart and delete a test VM")
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--namespace", default="default")
    parser.add_argument("--timeout", type=int, default=180)
    parser.add_argument("--guest-exec", action="store_true")
    parser.add_argument("--report", default="compatibility.result.json")
    args = parser.parse_args()
    if not args.run:
        parser.error("supply --run to execute this mutating qualification")
    client = Client(os.environ["KAIRON_KUBE_URL"], os.environ["KAIRON_KUBE_TOKEN"], namespace=args.namespace,
                    ca_file=os.environ.get("KAIRON_KUBE_CA_FILE"))
    ui = UIClient(os.environ["KAIRON_UI_URL"], os.environ["KAIRON_UI_TOKEN"], namespace=args.namespace,
                  ca_file=os.environ.get("KAIRON_UI_CA_FILE")) if args.guest_exec else None
    report = qualify(client, json.loads(Path(args.manifest).read_text()), ui=ui, timeout=args.timeout)
    Path(args.report).write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
