#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0
import os
import sys
from pathlib import Path

root = Path(__file__).resolve().parents[3]
cmd = [sys.executable, str(root / "ecosystem/ci.py"),
       "--manifest", os.environ["KAIRON_MANIFEST"], "--command", os.environ["KAIRON_COMMAND"],
       "--namespace", os.environ["KAIRON_NAMESPACE"], "--boot-timeout", os.environ["KAIRON_BOOT_TIMEOUT"],
       "--exec-timeout", os.environ["KAIRON_EXEC_TIMEOUT"], "--ttl", os.environ["KAIRON_TTL"]]
# exec keeps SIGTERM directed at the cleanup-aware CI process rather than a wrapper.
os.execv(sys.executable, cmd)
