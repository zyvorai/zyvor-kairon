#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0

"""Run a small task in a disposable agent sandbox.

Environment: KAIRON_KUBE_URL, KAIRON_KUBE_TOKEN, KAIRON_UI_URL,
KAIRON_UI_TOKEN, optional KAIRON_NAMESPACE (default "default"),
KAIRON_IMAGE (a guest image with qemu-guest-agent) or KAIRON_POOL (a
MachinePool to claim from instead).
"""

import os
import sys

from kairon import Client, Sandbox, UIClient


def main() -> int:
    ns = os.environ.get("KAIRON_NAMESPACE", "default")
    kube = Client(os.environ["KAIRON_KUBE_URL"], os.environ["KAIRON_KUBE_TOKEN"], namespace=ns,
                  ca_file=os.environ.get("KAIRON_KUBE_CA_FILE"))
    ui = UIClient(os.environ["KAIRON_UI_URL"], os.environ["KAIRON_UI_TOKEN"], namespace=ns,
                  ca_file=os.environ.get("KAIRON_UI_CA_FILE"))
    if pool := os.environ.get("KAIRON_POOL"):
        sb = Sandbox.claim_from(kube, ui, pool, ttl_seconds=3600)
    else:
        sb = Sandbox.create(kube, ui, image=os.environ["KAIRON_IMAGE"],
                            idle_timeout=600, max_lifetime=3600)
    with sb:
        print(sb.run("uname -a", check=True).stdout.strip())
        sb.write_file("/tmp/task.sh", b"for i in 1 2 3; do echo step $i; sleep 1; done\n", mode=0o755)
        proc = sb.start("sh /tmp/task.sh")
        proc.wait(timeout=60)
        print(proc.logs(), end="")
        print(sb.read_file("/etc/hostname").decode().strip())
    return 0


if __name__ == "__main__":
    sys.exit(main())
