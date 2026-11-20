#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0
"""Execute a JSON argv in a disposable Kairon VM; never interpolate shell input."""
from __future__ import annotations
import argparse
import json
import os
import signal
import sys
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "sdk/python"))
from kairon import Client, UIClient  # noqa: E402

TEMP_LABEL = "ecosystem.zyvor.dev/temporary"
EXPIRY_ANNOTATION = "ecosystem.zyvor.dev/expires-at"


def temporary_manifest(manifest: dict, namespace: str, ttl: int, *, now: datetime | None = None) -> dict:
    if not isinstance(ttl, int) or isinstance(ttl, bool) or ttl < 60:
        raise ValueError("CI TTL must be at least 60 seconds")
    # Use a fresh object; never delete/overwrite a user's pre-existing VM.
    obj = json.loads(json.dumps(manifest))
    if obj.get("kind") != "Machine" or obj.get("apiVersion") != "kairon.zyvor.dev/v1":
        raise ValueError("CI requires a Kairon Machine manifest")
    meta = obj.setdefault("metadata", {})
    if any(key in meta for key in ("uid", "resourceVersion", "ownerReferences", "finalizers", "deletionTimestamp")) or "status" in obj:
        raise ValueError("CI manifest must be a clean create request")
    meta["name"] = "ci-" + uuid.uuid4().hex[:20]
    meta["namespace"] = namespace
    meta.setdefault("labels", {})[TEMP_LABEL] = "true"
    now = now or datetime.now(timezone.utc)
    meta.setdefault("annotations", {})[EXPIRY_ANNOTATION] = (now + timedelta(seconds=ttl)).isoformat()
    obj["spec"]["ttlSeconds"] = ttl
    return obj


def run(manifest: dict, argv: list[str], client: Client, ui: UIClient, *,
        boot_timeout: int = 180, exec_timeout: int = 120, ttl: int = 900) -> dict:
    if not isinstance(argv, list) or not argv or any(not isinstance(x, str) for x in argv) or not argv[0]:
        raise ValueError("command must be a JSON array of strings with a non-empty executable")
    if not 1 <= exec_timeout <= 300 or not 1 <= boot_timeout <= 3600:
        raise ValueError("exec timeout must be 1..300; boot timeout must be 1..3600")
    if ttl <= boot_timeout + exec_timeout + 30:
        raise ValueError("TTL must exceed boot + exec deadlines plus 30 seconds")
    obj = temporary_manifest(manifest, client.namespace, ttl)
    if not obj["spec"].get("guestAgent", {}).get("enabled"):
        raise ValueError("CI QGA exec requires spec.guestAgent.enabled and a prepared guest image")
    with client.temporary_machine(obj, timeout=boot_timeout) as machine:
        result = ui.exec(machine["metadata"]["name"], argv, timeout_seconds=exec_timeout)
        return {"machine": machine["metadata"]["name"], "uid": machine["metadata"]["uid"], **result}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--command", required=True, help='JSON argv, e.g. ["/usr/bin/python3","--version"]')
    parser.add_argument("--namespace", default="default")
    parser.add_argument("--boot-timeout", type=int, default=180)
    parser.add_argument("--exec-timeout", type=int, default=120)
    parser.add_argument("--ttl", type=int, default=900)
    parser.add_argument("--result", help="Optional result JSON (contains guest stdout/stderr)")
    args = parser.parse_args()
    def terminate(signum, frame):
        raise SystemExit(128 + signum)
    signal.signal(signal.SIGTERM, terminate)
    try:
        client = Client(os.environ["KAIRON_KUBE_URL"], os.environ["KAIRON_KUBE_TOKEN"],
                        namespace=args.namespace, ca_file=os.environ.get("KAIRON_KUBE_CA_FILE"))
        ui = UIClient(os.environ["KAIRON_UI_URL"], os.environ["KAIRON_UI_TOKEN"], namespace=args.namespace,
                      ca_file=os.environ.get("KAIRON_UI_CA_FILE"))
        result = run(json.loads(Path(args.manifest).read_text()), json.loads(args.command), client, ui,
                     boot_timeout=args.boot_timeout, exec_timeout=args.exec_timeout, ttl=args.ttl)
        if args.result:
            path = Path(args.result)
            # Restrict command-output files to the current user; guest output can include secrets.
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, "w") as stream:
                json.dump(result, stream, indent=2)
        print(f'Kairon guest command exit code: {result["exitCode"]}; cleanup requested')
        return 0 if result["exitCode"] == 0 else 1
    except Exception as exc:
        # Do not print environment values, manifest contents or guest commands.
        print(f"Kairon CI failed ({type(exc).__name__}); inspect cluster events", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
