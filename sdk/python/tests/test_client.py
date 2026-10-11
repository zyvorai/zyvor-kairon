# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
import copy
import json
import sys
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from kairon import APIError, API_VERSION, Client, UIClient

MANIFEST = {"apiVersion": API_VERSION, "kind": "Machine", "metadata": {"name": "test"},
            "spec": {"resources": {"cpu": "1", "memory": "1Gi"}, "guestAgent": {"enabled": True}}}


class Fixture:
    def __init__(self):
        self.calls = []
        self.obj = None
        self.responses = []
        self.fail = None
        self.redirect = False
        self.get_count = 0
        fixture = self
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def handle_request(self):
                length = int(self.headers.get("Content-Length", 0))
                body = json.loads(self.rfile.read(length)) if length else None
                fixture.calls.append((self.command, self.path, dict(self.headers), body))
                if self.headers.get("Authorization") not in ("Bearer kube-secret", "Bearer ui-secret"):
                    self.send(401, {"error": "unauthorized"}); return
                if fixture.redirect:
                    self.send_response(302)
                    self.send_header("Location", "/steal")
                    self.end_headers(); return
                if fixture.fail:
                    self.send(fixture.fail, {"error": "private response secret"}); return
                if self.path.startswith("/api/v1/"):
                    if self.path.endswith("/exec"):
                        self.send(200, {"exitCode": 0, "stdout": "ok", "stderr": ""})
                    elif self.path.startswith("/api/v1/usage.csv"):
                        self.send_response(200); self.end_headers(); self.wfile.write(b"namespace,ledger\ndefault,test\n")
                    else:
                        self.send(200, {})
                    return
                if self.command == "POST":
                    fixture.obj = copy.deepcopy(body)
                    fixture.obj["metadata"].update(uid="uid-a", generation=1)
                    fixture.obj["status"] = {"phase": "Running", "observedGeneration": 1,
                                             "conditions": [{"type": "Ready", "status": "True"}]}
                    self.send(201, fixture.obj)
                elif self.command == "PATCH":
                    fixture.obj["spec"].update(body["spec"])
                    self.send(200, fixture.obj)
                elif self.command == "DELETE":
                    if fixture.obj and fixture.obj["metadata"]["uid"] != body["preconditions"]["uid"]:
                        self.send(409, {"message": "UID conflict"}); return
                    fixture.obj = None
                    self.send(200, {"kind": "Status", "status": "Success"})
                elif self.command == "GET":
                    fixture.get_count += 1
                    if fixture.responses:
                        self.send(200, fixture.responses.pop(0))
                    elif self.path.split("?", 1)[0].endswith("/machines"):
                        self.send(200, {"items": [fixture.obj] if fixture.obj else []})
                    elif fixture.obj:
                        self.send(200, fixture.obj)
                    else:
                        self.send(404, {"message": "not found"})
            def send(self, status, body):
                raw = json.dumps(body).encode()
                self.send_response(status); self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw))); self.end_headers(); self.wfile.write(raw)
            do_GET = do_POST = do_PATCH = do_DELETE = handle_request
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
    def close(self):
        self.server.shutdown(); self.server.server_close(); self.thread.join()


class ClientTests(unittest.TestCase):
    def setUp(self):
        self.fixture = Fixture()
        self.client = Client(self.fixture.url, "kube-secret")
    def tearDown(self):
        self.fixture.close()
    def test_create_uses_actual_group_and_does_not_mutate_manifest(self):
        original = copy.deepcopy(MANIFEST)
        out = self.client.machines.create(MANIFEST)
        self.assertEqual(MANIFEST, original)
        method, path, headers, body = self.fixture.calls[-1]
        self.assertEqual((method, path), ("POST", "/apis/kairon.zyvor.dev/v1/namespaces/default/machines"))
        self.assertEqual(body["metadata"]["namespace"], "default")
        self.assertEqual(out["metadata"]["uid"], "uid-a")
        self.assertEqual(headers["Authorization"], "Bearer kube-secret")
    def test_adaptive_migration_is_live_with_overrides(self):
        self.client.migrations.migrate("db", name="move-db", strategy="adaptive",
                                       adaptive={"compression": {"algorithm": "zstd", "level": 3}})
        method, path, _, body = self.fixture.calls[-1]
        self.assertEqual((method, path), ("POST", "/apis/kairon.zyvor.dev/v1/namespaces/default/machinemigrations"))
        self.assertEqual(body["kind"], "MachineMigration")
        self.assertEqual(body["spec"]["strategy"], "live")
        self.assertEqual(body["spec"]["multifdChannels"], 2)
        self.assertEqual(body["spec"]["adaptive"]["compression"]["algorithm"], "zstd")
        with self.assertRaises(ValueError):
            self.client.migrations.migrate("db", strategy="cold", adaptive={})
        with self.assertRaises(ValueError):
            self.client.migrations.migrate("db", strategy="fast")
    def test_migration_progress_reads_status(self):
        self.client.migrations.migrate("db", name="move-db", strategy="adaptive")
        self.fixture.obj["status"] = {"phase": "Running", "ramTotal": 4, "ramRemaining": 1,
                                      "progress": {"throughputMbps": 900},
                                      "adaptive": {"stage": "AutoConverge"}}
        progress = self.client.migrations.progress("move-db")
        self.assertEqual(progress["phase"], "Running")
        self.assertEqual(progress["ramRemaining"], 1)
        self.assertEqual(progress["progress"]["throughputMbps"], 900)
        self.assertEqual(progress["adaptive"]["stage"], "AutoConverge")
    def test_readiness_requires_current_generation_and_ready(self):
        obj = self.client.machines.create(MANIFEST)
        stale = copy.deepcopy(obj); stale["metadata"]["generation"] = 2
        current = copy.deepcopy(stale); current["status"]["observedGeneration"] = 2
        self.fixture.responses = [stale, current]
        self.assertEqual(self.client.machines.wait("test", interval=.001)["metadata"]["generation"], 2)
        self.assertEqual(self.fixture.get_count, 2)
    def test_wait_timeout_and_recovery_fail_closed(self):
        obj = self.client.machines.create(MANIFEST)
        self.fixture.obj["status"]["phase"] = "Pending"
        with self.assertRaises(TimeoutError):
            self.client.machines.wait("test", timeout=.025, interval=.005)
        self.fixture.obj["status"]["phase"] = "NeedsRecovery"
        with self.assertRaisesRegex(RuntimeError, "recovery"):
            self.client.machines.wait("test")
        self.fixture.obj = obj
    def test_wait_rejects_replacement_identity(self):
        self.client.machines.create(MANIFEST)
        with self.assertRaisesRegex(RuntimeError, "identity changed"):
            self.client.machines.wait("test", uid="other")
    def test_cleanup_on_callback_and_readiness_failure(self):
        with self.assertRaisesRegex(RuntimeError, "application failed"):
            with self.client.temporary_machine(MANIFEST):
                raise RuntimeError("application failed")
        self.assertIsNone(self.fixture.obj)
        self.assertEqual(self.fixture.calls[-1][3]["preconditions"], {"uid": "uid-a"})
        self.fixture.responses = [{"metadata": {"uid": "uid-a"}, "status": {"phase": "Failed"}}]
        with self.assertRaises(RuntimeError):
            with self.client.temporary_machine(MANIFEST):
                self.fail("must not run callback")
        self.assertIsNone(self.fixture.obj)
    def test_uid_preconditions_preserve_replacement(self):
        self.client.machines.create(MANIFEST)
        self.fixture.obj["metadata"]["uid"] = "replacement"
        with self.assertRaises(APIError) as ctx:
            self.client.machines.delete("test", uid="uid-a")
        self.assertEqual(ctx.exception.status, 409)
        self.assertIsNotNone(self.fixture.obj)
    def test_write_not_retried_and_error_excludes_response_secrets(self):
        self.fixture.fail = 503
        with self.assertRaises(APIError) as ctx:
            self.client.machines.create(MANIFEST)
        self.assertEqual(len(self.fixture.calls), 1)
        self.assertNotIn("secret", str(ctx.exception))
    def test_redirect_is_never_followed(self):
        self.fixture.redirect = True
        with self.assertRaises(APIError) as ctx:
            self.client.machines.list()
        self.assertEqual(ctx.exception.status, 302)
        self.assertEqual(len(self.fixture.calls), 1)
    def test_path_namespace_and_metadata_validation(self):
        for value in ("../admin", "x/y", "a..b", "a.-b", "", "a" * 64):
            with self.assertRaises(ValueError):
                Client(self.fixture.url, "token", namespace=value)
        for field in ("uid", "resourceVersion", "deletionTimestamp", "ownerReferences"):
            bad = copy.deepcopy(MANIFEST); bad["metadata"][field] = "unsafe"
            with self.assertRaises(ValueError):
                self.client.machines.create(bad)
        bad = copy.deepcopy(MANIFEST); bad["metadata"]["namespace"] = "other"
        with self.assertRaises(ValueError): self.client.machines.create(bad)
        with self.assertRaises(ValueError): self.client.resource("../../secrets")
    def test_tls_and_timeout_validation(self):
        for url in ("http://remote.example", "https://u:p@example.com", "https://example.com?token=x"):
            with self.assertRaises(ValueError): Client(url, "secret")
        for timeout in (0, -1, float("nan"), float("inf"), True):
            with self.assertRaises(ValueError): Client(self.fixture.url, "secret", timeout=timeout)
        with self.assertRaises(ValueError): Client(self.fixture.url, "secret\r\nheader")
    def test_ui_credentials_and_qga_wire_shape(self):
        ui = UIClient(self.fixture.url, "ui-secret")
        out = ui.exec("test", ["/usr/bin/python3", "--version"], timeout_seconds=120)
        _, path, headers, body = self.fixture.calls[-1]
        self.assertEqual(path, "/api/v1/machines/default/test/exec")
        self.assertEqual(body, {"path": "/usr/bin/python3", "args": ["--version"], "timeoutSeconds": 120})
        self.assertEqual(headers["Authorization"], "Bearer ui-secret")
        self.assertEqual(out["exitCode"], 0)
        self.assertIn("namespace,ledger", ui.usage_csv())
    def test_claims_and_fleet_use_separate_groups(self):
        self.client.claims.create({"apiVersion": API_VERSION, "kind": "MachineClaim", "metadata": {"name": "claim"},
                                   "spec": {"poolName": "warm", "ttlSeconds": 900, "reclaimPolicy": "Delete"}})
        self.assertIn("/machineclaims", self.fixture.calls[-1][1])
        self.client.resource("machineusageledgers", fleet=True).list()
        self.assertIn("/apis/fleet.kairon.zyvor.dev/v1alpha1/", self.fixture.calls[-1][1])
    def test_fork_copies_spec_and_guards_attached_storage(self):
        self.client.machines.create(MANIFEST)
        self.fixture.obj["spec"]["nodeName"] = "worker-1"
        self.fixture.obj["spec"]["volumes"] = [{"name": "data"}]
        with self.assertRaisesRegex(ValueError, "boot disk"):
            self.client.machines.fork("test", "child")
        self.fixture.obj["spec"].pop("volumes")
        child = self.client.machines.fork("test", "child")
        self.assertEqual(child["metadata"]["annotations"], {"kairon.zyvor.dev/fork-from": "test"})
        self.assertEqual(child["spec"]["nodeName"], "worker-1")
        self.assertNotIn("finalizers", self.fixture.calls[-1][3]["metadata"])


if __name__ == "__main__":
    unittest.main()
