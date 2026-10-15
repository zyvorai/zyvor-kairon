#!/usr/bin/env python3
# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
"""End-to-end MCP check: drive a built `kaironctl mcp serve` with the
official MCP Python SDK client against a fake Kubernetes/UI API.

Usage: scripts/test-mcp-sdk.py path/to/kaironctl
Requires: pip install mcp
"""

import asyncio
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

MACHINES = "/apis/kairon.zyvor.dev/v1/machines"
WEB = "/apis/kairon.zyvor.dev/v1/namespaces/default/machines/web"
PATCHES = []

# Tools the calls below depend on; read-only mode may expose more, all marked readOnlyHint.
READ_CORE = {"list_machines", "get_machine", "list_network_policies", "machine_network", "machine_edge_identity"}
# Exactly what --allow-write adds. Adding a write tool must update this list.
WRITE_TOOLS = {
    "apply_claim_step", "apply_network_policy", "audit_record", "claim_machine", "create_sealed_claim",
    "create_snapshot", "delete_machine", "fork_machine", "machine_backup", "machine_disk", "machine_nic",
    "network_capture", "release_claim", "set_power_state", "snapshot_volume",
    "session_checkpoint", "session_create", "session_delete", "session_exec", "session_fork",
    "session_git_commit", "session_input", "session_job_start", "session_preview", "session_restore",
    "session_write_file",
}


class Fake(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == MACHINES:
            self.reply(200, {"items": [{
                "metadata": {"name": "web", "namespace": "default"},
                "spec": {"powerState": "Running", "resources": {"cpu": "1", "memory": "1Gi"}},
                "status": {"phase": "Running", "nodeName": "n1", "guestIP": "10.0.0.5"},
            }]})
        elif path == WEB:
            self.reply(200, {"metadata": {"name": "web", "namespace": "default"}, "status": {"phase": "Running"}})
        elif path == "/api/v1/machines/default/web/network-drops":
            if self.headers.get("Authorization") != "Bearer ui-secret":
                self.reply(401, {"error": "unauthorized"})
                return
            self.reply(200, {"items": [{"reason": "dns_deny", "packets": 3}]})
        else:
            self.reply(404, {"error": "not found"})

    def do_PATCH(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path.split("?")[0] == WEB:
            PATCHES.append(body.decode())
            self.reply(200, {})
        else:
            self.reply(404, {"error": "not found"})


def attr(obj, *names):
    for n in names:
        if hasattr(obj, n):
            return getattr(obj, n)
    raise AttributeError(names[0])


def check(cond, msg):
    if not cond:
        raise SystemExit("FAIL: " + msg)
    print("ok:", msg)


async def call(session, name, args):
    res = await session.call_tool(name, args)
    return bool(attr(res, "is_error", "isError")), "".join(getattr(c, "text", "") for c in res.content)


def read_only_hint(tool):
    ann = getattr(tool, "annotations", None)
    return bool(ann and attr(ann, "read_only_hint", "readOnlyHint"))


async def run(binary, base, allow_write, read_tools=None):
    env = dict(os.environ, KAIRON_KUBE_URL=base, KAIRON_UI_URL=base, KAIRON_UI_TOKEN="ui-secret")
    args = ["mcp", "serve"] + (["--allow-write"] if allow_write else [])
    params = StdioServerParameters(command=binary, args=args, env=env)
    async with stdio_client(params) as (r, w):
        async with ClientSession(r, w) as session:
            init = await session.initialize()
            check(attr(attr(init, "server_info", "serverInfo"), "name") == "kairon", "serverInfo.name is kairon")
            tools = {t.name: t for t in (await session.list_tools()).tools}
            if not allow_write:
                check(READ_CORE <= set(tools), f"read-only tools include {sorted(READ_CORE)}")
                check(not set(tools) & WRITE_TOOLS, f"no write tools without --allow-write: {sorted(set(tools) & WRITE_TOOLS)}")
                unmarked = sorted(n for n, t in tools.items() if not read_only_hint(t))
                check(not unmarked, f"every read-only tool carries readOnlyHint (unmarked: {unmarked})")
                err, text = await call(session, "list_machines", {})
                check(not err and "10.0.0.5" in text, "list_machines returns guest IP")
                err, text = await call(session, "get_machine", {"name": "web"})
                check(not err and "Running" in text, "get_machine returns phase")
                err, text = await call(session, "machine_network", {"name": "web", "kind": "network-drops", "limit": 5})
                check(not err and "dns_deny" in text, "machine_network reaches UI API with token")
                err, text = await call(session, "machine_network", {"name": "web", "kind": "bogus"})
                check(err, "invalid kind is a tool error")
                err, text = await call(session, "get_machine", {"name": "web", "extra": 1})
                check(err and "invalid arguments" in text, "unknown arguments rejected")
                err, text = await call(session, "set_power_state", {"name": "web", "state": "Stopped"})
                check(err and "--allow-write" in text and not PATCHES, "write tool refused without --allow-write")
            else:
                added = set(tools) - read_tools
                check(added == WRITE_TOOLS, f"--allow-write adds exactly the write tools (added {sorted(added)})")
                err, text = await call(session, "set_power_state", {"name": "web", "state": "Stopped"})
                check(not err and len(PATCHES) == 1 and '"Stopped"' in PATCHES[0], "set_power_state patches Machine")
            return set(tools)


def main():
    if len(sys.argv) != 2:
        raise SystemExit(__doc__)
    binary = os.path.abspath(sys.argv[1])
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Fake)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    base = f"http://127.0.0.1:{srv.server_address[1]}"
    read_tools = asyncio.run(run(binary, base, False))
    asyncio.run(run(binary, base, True, read_tools))
    print("MCP SDK end-to-end: PASS")


if __name__ == "__main__":
    main()
