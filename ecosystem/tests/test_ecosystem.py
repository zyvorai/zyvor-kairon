# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0
import copy
import json
import sys
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path
from unittest.mock import Mock
import jsonschema
import yaml
ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "ecosystem"))
sys.path.insert(0, str(ROOT / "sdk/python"))
sys.path.insert(0, str(ROOT / "sdk/python/tests"))
import catalog
import ci
import cleanup
import compatibility
import validate_extension
from kairon import Client, UIClient
from kairon.client import KINDS, FLEET_KINDS
from test_client import Fixture, MANIFEST

DIGEST = "sha256:" + "a" * 64
IMAGE = catalog.pinned_image("https://images.example/ubuntu.qcow2", DIGEST)


class CatalogTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.crds = {o["spec"]["names"]["kind"]: o for o in yaml.safe_load_all((ROOT / "deploy/crd.yaml").read_text())}
    def test_every_recipe_and_output_matches_real_crd_schema(self):
        for path in catalog.CATALOG.glob("*.json"):
            recipe = catalog.load(path.stem)
            for kind in ("Machine", "MachinePool", "MachineTemplateVersion"):
                with self.subTest(recipe=path.stem, kind=kind):
                    obj = catalog.render(recipe, "test", "default", IMAGE, kind=kind)
                    crd = self.crds[kind]
                    version = next(v for v in crd["spec"]["versions"] if v["name"] == "v1alpha1")
                    jsonschema.Draft7Validator(version["schema"]["openAPIV3Schema"]).validate(obj)
                    if kind == "MachinePool":
                        self.assertNotIn("ttlSeconds", obj["spec"]["template"]["spec"])
    def test_all_sdk_resource_mappings_match_namespaced_crds(self):
        for group, kinds in (("kairon.zyvor.dev", KINDS), ("fleet.kairon.zyvor.dev", FLEET_KINDS)):
            actual = {o["spec"]["names"]["plural"]: o["spec"]["names"]["kind"] for o in self.crds.values()
                      if o["spec"]["group"] == group and o["spec"]["scope"] == "Namespaced"}
            self.assertEqual(kinds, actual)
    def test_image_digest_and_transport_validation(self):
        for location, digest in (("http://images.example/a", DIGEST), ("https://u:p@images.example/a", DIGEST),
                                 ("https://images.example/a", "latest"), ("oci://ghcr.io/zyvorai/disk@sha256:" + "b" * 64, DIGEST)):
            with self.assertRaises(ValueError): catalog.pinned_image(location, digest)
        image = catalog.pinned_image("oci://ghcr.io/zyvorai/disk", DIGEST, "raw")
        self.assertEqual(image["source"]["oci"], "ghcr.io/zyvorai/disk@" + DIGEST)
        self.assertTrue(catalog.pinned_image("https://images.example/a.ova", DIGEST, "ova")["source"]["repair"])
    def test_rejects_recipe_path_traversal_and_privileged_specs(self):
        with self.assertRaises(ValueError): catalog.load("../catalog/python-dev")
        for key in ("deviceClaims", "nodeName", "image", "security"):
            recipe = catalog.load("python-dev"); recipe["spec"][key] = {}
            with self.assertRaises(ValueError): catalog.validate(recipe)
        for ttl in (0, -1, True):
            with self.assertRaises(ValueError): catalog.render(catalog.load("python-dev"), "test", "default", IMAGE, ttl=ttl)
    def test_renderer_preserves_recipe_and_rejects_extra_image_options(self):
        recipe = catalog.load("python-dev"); before = copy.deepcopy(recipe)
        catalog.render(recipe, "test", "default", IMAGE)
        self.assertEqual(recipe, before)
        bad = copy.deepcopy(IMAGE); bad["source"]["insecureSkipTLSVerify"] = True
        with self.assertRaises(ValueError): catalog.render(recipe, "test", "default", bad)


class AutomationTests(unittest.TestCase):
    def test_sweeper_requires_opt_in_label_uid_and_timezone(self):
        now = datetime(2026, 10, 8, tzinfo=timezone.utc)
        obj = ci.temporary_manifest(MANIFEST, "default", 900, now=now - timedelta(hours=1))
        obj["metadata"]["uid"] = "uid-a"
        self.assertTrue(cleanup.expired(obj, now))
        for change in ("missing-label", "missing-uid", "invalid-expiry", "no-timezone", "deleting"):
            bad = copy.deepcopy(obj)
            if change == "missing-label": bad["metadata"].pop("labels")
            if change == "missing-uid": bad["metadata"].pop("uid")
            if change == "invalid-expiry": bad["metadata"]["annotations"][ci.EXPIRY_ANNOTATION] = "bad"
            if change == "no-timezone": bad["metadata"]["annotations"][ci.EXPIRY_ANNOTATION] = "2026-01-01"
            if change == "deleting": bad["metadata"]["deletionTimestamp"] = "2026-01-01"
            self.assertFalse(cleanup.expired(bad, now), change)
    def test_sweeper_grace_and_manifest_identity(self):
        now = datetime.now(timezone.utc)
        obj = ci.temporary_manifest(MANIFEST, "default", 900, now=now)
        other = ci.temporary_manifest(MANIFEST, "default", 900, now=now)
        self.assertNotEqual(obj["metadata"]["name"], other["metadata"]["name"])
        self.assertEqual(MANIFEST["metadata"]["name"], "test")
        obj["metadata"]["uid"] = "uid-a"
        self.assertFalse(cleanup.expired(obj, now + timedelta(seconds=920)))
        self.assertTrue(cleanup.expired(obj, now + timedelta(seconds=970)))
    def test_ci_guest_nonzero_is_preserved_and_machine_cleaned(self):
        fixture = Fixture()
        try:
            client = Client(fixture.url, "kube-secret")
            ui = Mock(spec=UIClient); ui.exec.return_value = {"exitCode": 7, "stdout": "", "stderr": "test failed"}
            result = ci.run(MANIFEST, ["/bin/test", "false"], client, ui)
            self.assertEqual(result["exitCode"], 7)
            self.assertIsNone(fixture.obj)
            self.assertEqual(fixture.calls[-1][3]["preconditions"]["uid"], "uid-a")
        finally: fixture.close()
    def test_ci_transport_exception_still_requests_cleanup(self):
        fixture = Fixture()
        try:
            ui = Mock(spec=UIClient); ui.exec.side_effect = TimeoutError("lost response")
            with self.assertRaises(TimeoutError):
                ci.run(MANIFEST, ["/bin/true"], Client(fixture.url, "kube-secret"), ui)
            self.assertIsNone(fixture.obj)
        finally: fixture.close()
    def test_ci_validates_before_creating(self):
        client = Mock(spec=Client); client.namespace = "default"
        ui = Mock(spec=UIClient)
        for argv in ("echo unsafe", [], [False], [""]):
            with self.assertRaises(ValueError): ci.run(MANIFEST, argv, client, ui)
        with self.assertRaises(ValueError): ci.run(MANIFEST, ["/bin/true"], client, ui, ttl=100)
        client.temporary_machine.assert_not_called()
    def test_qualification_reports_cleanup_and_guest_skip(self):
        fixture = Fixture()
        try:
            client = Client(fixture.url, "kube-secret")
            # The fixture changes phase when PATCH is issued; observed generation follows the write.
            original = client.machines.power
            def power(name, state):
                obj = original(name, state)
                fixture.obj["status"]["phase"] = state
                return obj
            client.machines.power = power
            report = compatibility.qualify(client, MANIFEST, timeout=1)
            self.assertTrue(report["passed"])
            self.assertIn({"name": "guest-exec", "result": "not-run"}, report["cases"])
            self.assertIn({"name": "delete-complete", "result": "pass"}, report["cases"])
            self.assertIsNone(fixture.obj)
        finally: fixture.close()


class ExtensionTests(unittest.TestCase):
    def test_unqualified_manifest_is_valid_without_certification(self):
        obj = validate_extension.validate(ROOT / "ecosystem/examples/ci-extension.json")
        self.assertEqual(obj["qualification"]["status"], "unqualified")
    def test_tested_status_requires_complete_local_report(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            obj = json.loads((ROOT / "ecosystem/examples/ci-extension.json").read_text())
            obj["qualification"].update(status="tested", report="result.json")
            path = root / "extension.json"; path.write_text(json.dumps(obj))
            report = {"schemaVersion": 1, "scope": "single-machine-lifecycle", "passed": True,
                      "cases": [{"name": name, "result": "pass"} for name in
                                ("create", "running-ready", "stop", "restart-ready", "delete-complete")]}
            (root / "result.json").write_text(json.dumps(report))
            validate_extension.validate(path)
            report["cases"].pop()
            (root / "result.json").write_text(json.dumps(report))
            with self.assertRaises(ValueError): validate_extension.validate(path)
            obj["qualification"]["report"] = "../escape.json"
            path.write_text(json.dumps(obj))
            with self.assertRaises(ValueError): validate_extension.validate(path)
    def test_extension_unknown_fields_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "extension.json"
            obj = json.loads((ROOT / "ecosystem/examples/ci-extension.json").read_text())
            obj["runCommand"] = "curl unsafe | sh"
            path.write_text(json.dumps(obj))
            with self.assertRaises(jsonschema.ValidationError): validate_extension.validate(path)


if __name__ == "__main__":
    unittest.main()
