# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
import base64
import sys
import unittest
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from kairon import CommandError, Sandbox


class FakeMachines:
    def __init__(self):
        self.created, self.patches, self.deleted, self.power_calls = [], [], [], []
    def create(self, manifest):
        self.created.append(manifest)
        return {"metadata": {"name": manifest["metadata"]["name"], "uid": "uid-1"}}
    def wait(self, name, **kw):
        return {}
    def power(self, name, state):
        self.power_calls.append(state)
    def patch(self, name, patch):
        self.patches.append((name, patch))
    def fork(self, parent, name, ttl_seconds=900):
        return {"metadata": {"name": name, "uid": "uid-fork"}}
    def delete(self, name, uid):
        self.deleted.append((name, uid))


class FakeKube:
    def __init__(self):
        self.machines = FakeMachines()


class FakeUI:
    def __init__(self, files=None):
        self.calls, self.files = [], files if files is not None else {}
    def exec(self, name, argv, timeout_seconds=60):
        self.calls.append(argv)
        if argv[0] == "base64":
            return {"exitCode": 0, "stdout": base64.b64encode(self.files[argv[1]]).decode() + "\n"}
        if argv[:3] == ["/bin/sh", "-c", ': >"$1"']:
            self.files[argv[4]] = b""
        elif len(argv) > 2 and "base64 -d" in argv[2]:
            self.files[argv[5]] += base64.b64decode(argv[4])
        elif len(argv) > 2 and "nohup" in argv[2]:
            return {"exitCode": 0, "stdout": "4242\n"}
        elif argv[0] == "tail":
            return {"exitCode": 0, "stdout": "hello\n"}
        elif argv == ["/bin/sh", "-c", "false"]:
            return {"exitCode": 1, "stdout": "", "stderr": "boom"}
        return {"exitCode": 0, "stdout": "", "stderr": ""}


class SandboxTests(unittest.TestCase):
    def test_create_labels_and_lifecycle(self):
        kube, ui = FakeKube(), FakeUI()
        with Sandbox.create(kube, ui, image="/img.qcow2", name="sb1", idle_timeout=600, max_lifetime=3600) as sb:
            self.assertEqual(sb.uid, "uid-1")
        m = kube.machines.created[0]
        self.assertEqual(m["metadata"]["labels"]["kairon.zyvor.dev/sandbox"], "true")
        self.assertEqual(m["spec"]["lifecycle"], {"idleTimeoutSeconds": 600, "maxLifetimeSeconds": 3600, "wakeOnConnect": True})
        self.assertTrue(m["spec"]["guestAgent"]["enabled"])
        self.assertEqual(kube.machines.deleted, [("sb1", "uid-1")])

    def test_run_check_raises(self):
        sb = Sandbox(FakeKube(), FakeUI(), "sb1")
        self.assertTrue(sb.run("true").ok)
        with self.assertRaises(CommandError):
            sb.run("false", check=True)

    def test_files_round_trip_in_chunks(self):
        ui = FakeUI()
        sb = Sandbox(FakeKube(), ui, "sb1")
        data = bytes(range(256)) * 600  # > one 64 KiB base64 chunk
        sb.write_file("/tmp/x", data, mode=0o600)
        self.assertEqual(sb.read_file("/tmp/x"), data)
        self.assertIn(["chmod", "600", "/tmp/x"], ui.calls)
        with self.assertRaises(ValueError):
            sb.write_file("relative", b"")

    def test_start_process_logs(self):
        sb = Sandbox(FakeKube(), FakeUI(), "sb1")
        proc = sb.start("sleep 10")
        self.assertEqual(proc.pid, 4242)
        self.assertEqual(proc.logs(), "hello\n")
        self.assertEqual(proc._offset, 6)
        self.assertEqual(sb.ui.calls[-1][:3], ["tail", "-c", "+1"])
        proc.logs()
        self.assertEqual(sb.ui.calls[-1][:3], ["tail", "-c", "+7"])

    def test_pause_resume_fork(self):
        kube = FakeKube()
        sb = Sandbox(kube, FakeUI(), "sb1", uid="uid-1")
        sb.pause(); sb.resume()
        self.assertEqual(kube.machines.power_calls, ["Paused", "Running"])
        child = sb.fork("sb1-child")
        self.assertEqual((child.name, child.uid), ("sb1-child", "uid-fork"))
        self.assertEqual(kube.machines.patches[-1][1]["metadata"]["labels"]["kairon.zyvor.dev/sandbox"], "true")


if __name__ == "__main__":
    unittest.main()
