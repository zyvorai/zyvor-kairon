# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
import base64
import sys
import unittest
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import hashlib
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from kairon import APIError, AgentSession, ApprovalRequired, CommandError, SessionError, UIClient, session_spec

APPROVAL = "11111111-2222-4333-8444-555555555555"
UPLOAD = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"


class FakeUI:
    def __init__(self):
        self.created, self.deleted, self.calls = [], [], []
        self.phase = {"work": "Running"}
        self.files = {}

    def create_session(self, name, spec, labels=None):
        self.created.append((name, spec))
        self.phase.setdefault(name, "Running")
        return {}

    def get_session(self, name):
        return {"status": {"phase": self.phase.get(name, "Pending"), "message": "no capable node"}}

    def delete_session(self, name):
        self.deleted.append(name)

    def session_call(self, name, method, verb, body=None, query=None, timeout=None, data=None):
        self.calls.append((name, method, verb, body, query))
        if verb == "exec" and body["command"].startswith("git push") and "approval_id" not in body:
            raise ApprovalRequired("POST", {"approval_id": APPROVAL, "kind": "exec", "summary": body["command"],
                                            "matched": "git push*"})
        if verb == "uploads":
            self.upload = {"meta": body, "data": bytearray(), "conflict": True}
            return {"id": UPLOAD}
        if verb == f"uploads/{UPLOAD}" and method == "PUT":
            if query["offset"] > 0 and self.upload["conflict"]:
                self.upload["conflict"] = False
                raise APIError(409, "PUT")
            self.upload["data"] += data
            return {"received": len(self.upload["data"])}
        if verb == f"uploads/{UPLOAD}" and method == "GET":
            return {"state": "open", "received": len(self.upload["data"])}
        if verb == f"uploads/{UPLOAD}/complete":
            self.files[self.upload["meta"]["path"]] = bytes(self.upload["data"])
            return {"state": "complete"}
        if verb == "exec":
            code = 1 if body["command"] == "false" else 0
            return {"exit_code": code, "stdout": "out", "stderr": "err" if code else ""}
        if verb == "fork":
            for n in ("work-aaaaaaaa", "work-bbbbbbbb"):
                self.phase[n] = "Running"
            return {"items": [{"id": "a"}, {"id": "b"}], "agentSessions": ["work-aaaaaaaa", "work-bbbbbbbb"]}
        if verb == "files/content" and method == "PUT":
            self.files[body["path"]] = base64.b64decode(body["content_base64"])
            return {}
        if verb == "files/content":
            return {"content_base64": base64.b64encode(self.files[query["path"]]).decode(), "mode": 420}
        if verb == "jobs":
            return {"job": "j1", "state": "running"}
        if verb == "previews":
            return {"items": [{"token": "t", "url": "/preview/default/work/t/"}]}
        return {}


class SessionTests(unittest.TestCase):
    def test_spec(self):
        spec = session_spec(agent="codex", git="https://github.com/o/r", memory="4Gi", preset="developer-safe",
                            allow_hosts=["api.github.com"], process_deny=["rm -rf *"], ttl_seconds=600,
                            budget={"maxExecs": 10})
        self.assertEqual(spec, {
            "agent": "codex", "workspace": {"git": "https://github.com/o/r"}, "resources": {"memory": "4Gi"},
            "policy": {"preset": "developer-safe", "network": {"allow": [{"host": "api.github.com"}]},
                       "process": {"deny": ["rm -rf *"]}},
            "ttlSeconds": 600, "budget": {"maxExecs": 10}})
        self.assertEqual(session_spec(), {})

    def test_create_exec_close(self):
        ui = FakeUI()
        with AgentSession.create(ui, name="work", agent="claude") as s:
            res = s.exec("make test", timeout_seconds=30)
            self.assertTrue(res.ok)
            self.assertEqual(ui.calls[-1][3], {"command": "make test", "timeout_seconds": 30})
            with self.assertRaises(CommandError):
                s.exec("false", check=True)
        self.assertEqual(ui.created[0], ("work", {"agent": "claude"}))
        self.assertEqual(ui.deleted, ["work"])

    def test_create_fails_and_cleans_up(self):
        ui = FakeUI()
        ui.phase["stuck"] = "Failed"
        with self.assertRaises(SessionError):
            AgentSession.create(ui, name="stuck")
        self.assertEqual(ui.deleted, ["stuck"])

    def test_fork_files_jobs_previews(self):
        ui = FakeUI()
        s = AgentSession(ui, "work")
        kids = s.fork(2)
        self.assertEqual([k.name for k in kids], ["work-aaaaaaaa", "work-bbbbbbbb"])
        s.write_file("/workspace/a.txt", b"hi", mode=0o644)
        self.assertEqual(s.read_file("/workspace/a.txt"), b"hi")
        self.assertEqual(s.start_job("npm run dev"), "j1")
        self.assertEqual(s.previews()[0]["url"], "/preview/default/work/t/")
        s.remove("/workspace/tmp", recursive=True)
        self.assertEqual(ui.calls[-1][4], {"path": "/workspace/tmp", "recursive": "true"})
        with self.assertRaises(ValueError):
            s.write_file("relative", b"")
        with self.assertRaises(ValueError):
            s.git("push")

    def test_approvals(self):
        ui = FakeUI()
        s = AgentSession(ui, "work")
        with self.assertRaises(ApprovalRequired) as ctx:
            s.exec("git push origin fix")
        self.assertEqual((ctx.exception.approval_id, ctx.exception.matched, ctx.exception.status), (APPROVAL, "git push*", 428))
        s.approve(APPROVAL, note="ok")
        self.assertEqual(ui.calls[-1][2:4], (f"approvals/{APPROVAL}/approve", {"note": "ok"}))
        self.assertTrue(s.exec("git push origin fix", approval_id=APPROVAL, approval_wait_seconds=30).ok)
        self.assertEqual(ui.calls[-1][3], {"command": "git push origin fix", "approval_id": APPROVAL, "approval_wait_seconds": 30})
        for bad in ({"approval_id": "x"}, {"approval_wait_seconds": 500}):
            with self.assertRaises(ValueError):
                s.exec("ls", **bad)
        self.assertEqual(session_spec(process_ask=["deploy *"]), {"policy": {"process": {"ask": ["deploy *"]}}})

    def test_large_write_uploads_in_chunks(self):
        ui = FakeUI()
        s = AgentSession(ui, "work")
        data = bytes(range(256)) * ((9 << 20) // 256 + 3)
        s.write_file("/workspace/big.bin", data)
        self.assertEqual(ui.files["/workspace/big.bin"], data)
        self.assertEqual(ui.upload["meta"]["sha256"], hashlib.sha256(data).hexdigest())
        self.assertFalse(ui.upload["conflict"])
        with self.assertRaises(ValueError):
            s.upload("/x", b"")

    def test_grant_needs_hosts(self):
        s = AgentSession(FakeUI(), "work")
        with self.assertRaises(ValueError):
            s.grant("gh", [])
        s.grant("gh", ["api.github.com"], value="Bearer x", ttl_seconds=60)
        self.assertEqual(s.ui.calls[-1][2:4], ("grants", {"secret_ref": "gh", "hosts": ["api.github.com"],
                                                          "value": "Bearer x", "ttl_seconds": 60}))


class ApprovalHTTPTests(unittest.TestCase):
    def test_428_becomes_approval_required(self):
        class H(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                body = json.dumps({"approval_required": True, "approval_id": APPROVAL, "matched": "deploy *",
                                   "summary": "deploy prod"}).encode()
                self.send_response(428)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *a):
                pass

        srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        try:
            ui = UIClient(f"http://127.0.0.1:{srv.server_address[1]}", "t")
            with self.assertRaises(ApprovalRequired) as ctx:
                ui.session_call("work", "POST", "exec", {"command": "deploy prod"})
            self.assertEqual((ctx.exception.approval_id, ctx.exception.summary), (APPROVAL, "deploy prod"))
            self.assertIsInstance(ctx.exception, APIError)
        finally:
            srv.shutdown()
            srv.server_close()


class UIClientSessionTests(unittest.TestCase):
    def test_paths(self):
        seen = []

        class HTTP:
            def request(self, method, path, body=None, **kw):
                seen.append((method, path, body))
                return {}

        ui = UIClient("http://127.0.0.1:1", "t", namespace="dev")
        ui._http = HTTP()
        ui.session_call("work", "GET", "jobs/j1", query={"offset": 5})
        ui.create_session("new", {"agent": "a"})
        self.assertEqual(seen[0][:2], ("GET", "/api/v1/agentsessions/dev/work/jobs/j1?offset=5"))
        self.assertEqual(seen[1], ("POST", "/api/v1/agentsessions?namespace=dev", {"name": "new", "spec": {"agent": "a"}, "labels": {}}))
        for bad in ("/exec", "../x", "files/../../etc"):
            with self.assertRaises(ValueError):
                ui.session_call("work", "GET", bad)


if __name__ == "__main__":
    unittest.main()
