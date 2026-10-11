# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
"""Small synchronous clients for Kairon's Kubernetes and optional UI APIs.

Writes are never retried: a lost response does not prove a write failed.
Kubernetes credentials and UI credentials are intentionally separate.
"""
from __future__ import annotations

import base64
import copy
import ipaddress
import json
import math
import re
import ssl
import time
from contextlib import contextmanager
from typing import Any, Iterator
from urllib.error import HTTPError
from urllib.parse import urlencode, urlsplit
from urllib.request import HTTPRedirectHandler, HTTPSHandler, Request, build_opener

API_VERSION = "kairon.zyvor.dev/v1"
FLEET_VERSION = "fleet.kairon.zyvor.dev/v1alpha1"
KINDS = {
    "machines": "Machine", "machinepools": "MachinePool",
    "machineclaims": "MachineClaim", "machinesnapshots": "MachineSnapshot",
    "machinesnapshotrestores": "MachineSnapshotRestore",
    "machinemigrations": "MachineMigration", "machinequotas": "MachineQuota",
    "machinesets": "MachineSet", "machinenetworkpolicies": "MachineNetworkPolicy",
    "machinebackups": "MachineBackup", "machinebackuprestores": "MachineBackupRestore",
    "machinedisruptionbudgets": "MachineDisruptionBudget", "machineinstancetypes": "MachineInstanceType",
    "machinesnapshotschedules": "MachineSnapshotSchedule", "migrationpolicies": "MigrationPolicy",
    "networksecuritygroups": "NetworkSecurityGroup",
}
FLEET_KINDS = {
    "machinehaprofiles": "MachineHAProfile", "nodefencerequests": "NodeFenceRequest",
    "machineactionapprovals": "MachineActionApproval",
    "machinebalancepolicies": "MachineBalancePolicy", "machineautoscalers": "MachineAutoscaler",
    "machinerecoveryplans": "MachineRecoveryPlan", "machinebackupgroups": "MachineBackupGroup",
    "machineimportplans": "MachineImportPlan", "machinetemplateversions": "MachineTemplateVersion",
    "machinetemplateclaims": "MachineTemplateClaim", "machinevirtualnetworks": "MachineVirtualNetwork",
    "machinenetworkclaims": "MachineNetworkClaim", "machineusageledgers": "MachineUsageLedger",
}
MAX_RESPONSE_BYTES = 8 * 1024 * 1024


class APIError(RuntimeError):
    """An HTTP failure, with a safe message that excludes request/response secrets."""
    def __init__(self, status: int, method: str):
        self.status = status
        super().__init__(f"Kairon {method} failed (HTTP {status})")


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _segment(value: str) -> str:
    if not isinstance(value, str) or len(value) > 253 or not re.fullmatch(
        r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?", value
    ) or any(not part or len(part) > 63 or part.startswith("-") or part.endswith("-")
             for part in value.split(".")):
        raise ValueError("expected a Kubernetes DNS name")
    return value


def _positive(value: float, label: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value <= 0:
        raise ValueError(f"{label} must be finite and positive")
    return value


def _url(value: str, allow_insecure: bool) -> str:
    u = urlsplit(value)
    if u.scheme not in ("http", "https") or not u.hostname or u.username or u.password or u.query or u.fragment:
        raise ValueError("URL must be an HTTP(S) origin or base path without credentials/query/fragment")
    local = u.hostname == "localhost"
    try:
        local = local or ipaddress.ip_address(u.hostname).is_loopback
    except ValueError:
        pass
    if u.scheme == "http" and not local and not allow_insecure:
        raise ValueError("remote HTTP requires allow_insecure=True; use HTTPS in production")
    return value.rstrip("/")


class _HTTP:
    def __init__(self, url: str, token: str, *, ca_file: str | None = None,
                 timeout: float = 30, allow_insecure: bool = False):
        self.url = _url(url, allow_insecure)
        if not token or "\r" in token or "\n" in token:
            raise ValueError("a non-empty bearer token is required")
        self._token = token
        self.timeout = _positive(timeout, "timeout")
        self._opener = build_opener(_NoRedirect(), HTTPSHandler(context=ssl.create_default_context(cafile=ca_file)))

    def request(self, method: str, path: str, body: Any = None, *,
                content_type: str = "application/json", timeout: float | None = None,
                raw: bool = False) -> Any:
        req = Request(self.url + path, method=method,
                      data=None if body is None else json.dumps(body).encode(),
                      headers={"Authorization": "Bearer " + self._token,
                               "Accept": "application/json", "Content-Type": content_type})
        try:
            with self._opener.open(req, timeout=self.timeout if timeout is None else _positive(timeout, "timeout")) as response:
                data = response.read(MAX_RESPONSE_BYTES + 1)
                if len(data) > MAX_RESPONSE_BYTES:
                    raise ValueError("Kairon response exceeds 8 MiB")
                if raw:
                    return data.decode("utf-8")
                return json.loads(data) if data else None
        except HTTPError as exc:
            exc.close()
            raise APIError(exc.code, method) from None


class Resource:
    """A namespace-scoped CRD collection. Every call uses the caller's RBAC."""
    def __init__(self, http: _HTTP, namespace: str, resource: str, kind: str, version: str):
        self._http, self.namespace, self.kind, self.version = http, _segment(namespace), kind, version
        self.path = f"/apis/{version}/namespaces/{self.namespace}/{resource}"

    def list(self, *, label_selector: str = "") -> list[dict]:
        query = "?" + urlencode({"labelSelector": label_selector}) if label_selector else ""
        return self._http.request("GET", self.path + query).get("items", [])

    def get(self, name: str, *, timeout: float | None = None) -> dict:
        return self._http.request("GET", self.path + "/" + _segment(name), timeout=timeout)

    def create(self, manifest: dict) -> dict:
        obj = copy.deepcopy(manifest)
        if obj.get("apiVersion") != self.version or obj.get("kind") != self.kind:
            raise ValueError("manifest kind/apiVersion does not match this resource")
        meta = obj.get("metadata", {})
        _segment(meta.get("name", ""))
        if meta.get("namespace", self.namespace) != self.namespace:
            raise ValueError("manifest namespace does not match the client namespace")
        if "status" in obj or any(k in meta for k in ("uid", "resourceVersion", "deletionTimestamp", "ownerReferences")):
            raise ValueError("create manifest must omit server-managed status/metadata")
        meta["namespace"] = self.namespace
        return self._http.request("POST", self.path, obj)

    def patch(self, name: str, patch: dict) -> dict:
        return self._http.request("PATCH", self.path + "/" + _segment(name), patch,
                                  content_type="application/merge-patch+json")

    def delete(self, name: str, *, uid: str) -> None:
        if not isinstance(uid, str) or not uid:
            raise ValueError("deletion requires the object's UID")
        try:
            self._http.request("DELETE", self.path + "/" + _segment(name), {
                "apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {"uid": uid}})
        except APIError as exc:
            if exc.status != 404:
                raise


class Machines(Resource):
    def power(self, name: str, state: str) -> dict:
        if state not in ("Running", "Stopped", "Paused", "Halted"):
            raise ValueError("unsupported power state")
        return self.patch(name, {"spec": {"powerState": state}})

    def wait(self, name: str, *, phase: str = "Running", timeout: float = 120,
             interval: float = 1, ready: bool = True, uid: str | None = None) -> dict:
        _positive(timeout, "timeout")
        _positive(interval, "interval")
        deadline = time.monotonic() + timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError(f"Machine did not reach {phase} before the deadline")
            obj = self.get(name, timeout=min(remaining, self._http.timeout))
            if uid is not None and obj.get("metadata", {}).get("uid") != uid:
                raise RuntimeError("Machine identity changed while waiting")
            status = obj.get("status", {})
            if status.get("phase") in ("Failed", "NeedsRecovery"):
                raise RuntimeError("Machine requires operator recovery")
            generation = obj.get("metadata", {}).get("generation", 0)
            observed = status.get("observedGeneration", 0) >= generation
            is_ready = any(c.get("type") == "Ready" and c.get("status") == "True"
                           for c in status.get("conditions", []))
            if observed and status.get("phase") == phase and (not ready or is_ready):
                return obj
            time.sleep(min(interval, max(0, deadline - time.monotonic())))

    def fork(self, parent: str, name: str, *, ttl_seconds: int = 900) -> dict:
        _segment(name)
        if not isinstance(ttl_seconds, int) or isinstance(ttl_seconds, bool) or ttl_seconds <= 0:
            raise ValueError("ttl_seconds must be a positive integer")
        source = self.get(parent)
        if len(f"kairon-{self.namespace}-{name}-1") > 63:
            raise ValueError("fork runtime name exceeds 63 characters")
        if source.get("status", {}).get("phase") != "Running" or not source.get("spec", {}).get("nodeName"):
            raise ValueError("fork parent must be Running on a node")
        if any(source["spec"].get(key) for key in ("volumes", "disks", "deviceClaims")):
            raise ValueError("fork copies only the boot disk; volumes/disks/devices are unsupported")
        # Do not copy parent owner references, finalizers, runtime IDs or migration annotations.
        spec = copy.deepcopy(source["spec"])
        spec.update(nodeName=source["spec"]["nodeName"], powerState="Running", ttlSeconds=ttl_seconds)
        return self.create({"apiVersion": API_VERSION, "kind": "Machine", "metadata": {
            "name": name, "namespace": self.namespace,
            "annotations": {"kairon.zyvor.dev/fork-from": parent},
            "labels": {"kairon.zyvor.dev/forked-from": parent,
                       "kairon.zyvor.dev/assigned-node": source["spec"]["nodeName"]}}, "spec": spec})


class Migrations(Resource):
    def migrate(self, machine: str, *, name: str | None = None, strategy: str = "auto",
                target_node: str = "", mode: str = "", bandwidth_mbps: int = 0,
                max_downtime_ms: int = 0, multifd_channels: int = 0,
                adaptive: dict | None = None) -> dict:
        """Create a MachineMigration. strategy="adaptive" is a live migration steered
        by kairon-node's adaptive engine (experimental); ``adaptive`` overrides the
        matching MigrationPolicy's settings field by field."""
        _segment(machine)
        if strategy not in ("auto", "live", "cold", "adaptive"):
            raise ValueError("strategy must be auto, live, cold or adaptive")
        if strategy == "adaptive":
            strategy = "live"
            adaptive = {} if adaptive is None else adaptive
        if adaptive is not None and strategy != "live":
            raise ValueError("adaptive settings need strategy live or adaptive")
        compression = (adaptive or {}).get("compression") or {}
        if compression.get("algorithm", "none") != "none" and not multifd_channels:
            multifd_channels = 2
        spec: dict = {"machineName": machine, "strategy": strategy}
        for key, value in (("targetNode", target_node), ("mode", mode)):
            if value:
                spec[key] = value
        for key, value in (("bandwidthMbps", bandwidth_mbps), ("maxDowntimeMs", max_downtime_ms),
                           ("multifdChannels", multifd_channels)):
            if value:
                _positive(value, key)
                spec[key] = value
        if adaptive is not None:
            spec["adaptive"] = copy.deepcopy(adaptive)
        name = name or f"{machine}-migrate-{int(time.time() * 1000):x}"
        return self.create({"apiVersion": API_VERSION, "kind": "MachineMigration",
                            "metadata": {"name": name, "namespace": self.namespace}, "spec": spec})

    def progress(self, name: str) -> dict:
        """Transfer progress, telemetry and adaptive engine state from status."""
        status = self.get(name).get("status", {})
        out = {"phase": status.get("phase", "Pending")}
        for key in ("ramTotal", "ramTransferred", "ramRemaining", "downtimeMs"):
            out[key] = status.get(key, 0)
        for key in ("progress", "adaptive"):
            if key in status:
                out[key] = status[key]
        return out


class Client:
    def __init__(self, kube_url: str, token: str, *, namespace: str = "default",
                 ca_file: str | None = None, timeout: float = 30, allow_insecure: bool = False):
        self.namespace = _segment(namespace)
        if len(namespace) > 63 or "." in namespace:
            raise ValueError("namespace must be a DNS label")
        self._http = _HTTP(kube_url, token, ca_file=ca_file, timeout=timeout, allow_insecure=allow_insecure)
        self.machines = Machines(self._http, namespace, "machines", "Machine", API_VERSION)
        self.migrations = Migrations(self._http, namespace, "machinemigrations", "MachineMigration", API_VERSION)
        self.pools = self.resource("machinepools")
        self.claims = self.resource("machineclaims")

    def resource(self, name: str, *, fleet: bool = False) -> Resource:
        kinds = FLEET_KINDS if fleet else KINDS
        if name not in kinds:
            raise ValueError("unsupported Kairon resource")
        return Resource(self._http, self.namespace, name, kinds[name], FLEET_VERSION if fleet else API_VERSION)

    @contextmanager
    def temporary_machine(self, manifest: dict, *, timeout: float = 120) -> Iterator[dict]:
        obj = self.machines.create(manifest)
        name, uid = obj["metadata"]["name"], obj["metadata"]["uid"]
        primary = None
        try:
            yield self.machines.wait(name, uid=uid, timeout=timeout)
        except BaseException as exc:
            primary = exc
            raise
        finally:
            try:
                self.machines.delete(name, uid=uid)
            except Exception as cleanup:
                if primary is None:
                    raise
                primary.add_note(f"Kairon cleanup failed: {type(cleanup).__name__}; inspect the Machine")


class UIClient:
    """Optional guest operations; the UI enforces admin and per-Machine access."""
    def __init__(self, url: str, token: str, *, namespace: str = "default", **options):
        self.namespace = _segment(namespace)
        if len(namespace) > 63 or "." in namespace:
            raise ValueError("namespace must be a DNS label")
        self._http = _HTTP(url, token, **options)

    def _machine(self, name: str) -> str:
        return f"/api/v1/machines/{self.namespace}/{_segment(name)}"

    def exec(self, name: str, argv: list[str], *, timeout_seconds: int = 60) -> dict:
        if not argv or any(not isinstance(arg, str) for arg in argv) or not argv[0]:
            raise ValueError("argv must contain an executable and string arguments")
        if not isinstance(timeout_seconds, int) or isinstance(timeout_seconds, bool) or not 1 <= timeout_seconds <= 300:
            raise ValueError("timeout_seconds must be between 1 and 300")
        return self._http.request("POST", self._machine(name) + "/exec", {
            "path": argv[0], "args": argv[1:], "timeoutSeconds": timeout_seconds}, timeout=timeout_seconds + 15)

    def agent_exec(self, name: str, command: str, *, timeout_seconds: int = 60) -> dict:
        if not command or not isinstance(command, str):
            raise ValueError("command must be non-empty")
        if not isinstance(timeout_seconds, int) or isinstance(timeout_seconds, bool) or not 1 <= timeout_seconds <= 300:
            raise ValueError("timeout_seconds must be between 1 and 300")
        return self._http.request("POST", self._machine(name) + "/agent-exec", {
            "command": command, "timeoutSeconds": timeout_seconds}, timeout=timeout_seconds + 15)

    def put_file(self, name: str, path: str, data: bytes, *, mode: int = 0o600) -> None:
        if len(data) > 3 * 1024 * 1024 or not path.startswith("/") or not 0 <= mode <= 0o777:
            raise ValueError("file needs an absolute path, <=3 MiB content and a mode <=0777")
        self._http.request("POST", self._machine(name) + "/agent-file/put", {
            "path": path, "contentBase64": base64.b64encode(data).decode(), "mode": mode})

    def capabilities(self, node: str) -> dict:
        return self._http.request("GET", "/api/v1/nodes/" + _segment(node) + "/capabilities")

    def usage_csv(self) -> str:
        return self._http.request("GET", "/api/v1/usage.csv?" + urlencode({"namespace": self.namespace}), raw=True)
