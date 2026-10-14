# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
"""Agent sandboxes: one Machine driven through the Kubernetes API (lifecycle)
and kairon-ui (commands and files).

    with Sandbox.create(kube, ui, image="/var/lib/kairon/images/agent.qcow2",
                        idle_timeout=600) as sb:
        print(sb.run("uname -a").stdout)
        sb.write_file("/tmp/task.py", b"print(42)")
        proc = sb.start("python3 /tmp/task.py")
        print(proc.logs())

Commands use qemu-guest-agent exec (spec.guestAgent.enabled), so the image
must run qemu-guest-agent. Files travel base64-encoded over the same channel.
"""
from __future__ import annotations

import base64
import shlex
import time
import uuid
from dataclasses import dataclass

from .client import API_VERSION, Client, UIClient, _segment

SANDBOX_LABEL = "kairon.zyvor.dev/sandbox"
# One exec argument carries at most this much base64 (Linux MAX_ARG_STRLEN
# is 128 KiB).
_WRITE_CHUNK = 64 * 1024


@dataclass
class ExecResult:
    exit_code: int
    stdout: str
    stderr: str

    @property
    def ok(self) -> bool:
        return self.exit_code == 0


class CommandError(RuntimeError):
    def __init__(self, result: ExecResult, cmd: str):
        self.result = result
        super().__init__(f"command exited {result.exit_code}: {cmd}: {result.stderr.strip()[:500]}")


def _sh(cmd: str | list[str]) -> list[str]:
    if isinstance(cmd, str):
        return ["/bin/sh", "-c", cmd]
    if not cmd or any(not isinstance(a, str) for a in cmd):
        raise ValueError("cmd must be a shell string or a non-empty argv list")
    return list(cmd)


class Process:
    """A background command started by Sandbox.start; output goes to a log file."""
    def __init__(self, sandbox: "Sandbox", pid: int, log: str):
        self.sandbox, self.pid, self.log = sandbox, pid, log
        self._offset = 0

    def logs(self) -> str:
        """Output written since the previous call."""
        out = self.sandbox.run(["tail", "-c", f"+{self._offset + 1}", self.log]).stdout
        self._offset += len(out.encode())
        return out

    def alive(self) -> bool:
        return self.sandbox.run(["kill", "-0", str(self.pid)]).ok

    def wait(self, *, timeout: float = 300, interval: float = 1) -> None:
        deadline = time.monotonic() + timeout
        while self.alive():
            if time.monotonic() >= deadline:
                raise TimeoutError(f"process {self.pid} still running")
            time.sleep(interval)

    def kill(self, signal: str = "TERM") -> None:
        self.sandbox.run(["kill", f"-{signal}", str(self.pid)])


class Sandbox:
    """A Machine used as an agent sandbox. Use as a context manager, or call close()."""
    def __init__(self, kube: Client, ui: UIClient, name: str, *, uid: str = "", claim: str = ""):
        self.kube, self.ui, self.name = kube, ui, _segment(name)
        self.uid, self.claim = uid, claim

    @classmethod
    def create(cls, kube: Client, ui: UIClient, *, image: str, name: str | None = None,
               cpu: str = "2", memory: str = "2Gi", backend: str = "qemu",
               idle_timeout: int = 0, idle_action: str = "", max_lifetime: int = 0,
               wake_on_connect: bool = True, labels: dict | None = None,
               spec: dict | None = None, timeout: float = 180) -> "Sandbox":
        """Create a sandbox Machine and wait until it is Running.

        idle_timeout/max_lifetime are seconds (0 = off); idle_action is Pause
        (default), Hibernate, Halt, Stop or Delete. spec entries override the
        generated Machine spec."""
        name = name or "sandbox-" + uuid.uuid4().hex[:8]
        body = {
            "image": {"path": image},
            "resources": {"cpu": cpu, "memory": memory},
            "runtime": {"backend": backend},
            "network": {"mode": "user"},
            "guestAgent": {"enabled": True},
            "powerState": "Running",
        }
        lifecycle = {k: v for k, v in {
            "idleTimeoutSeconds": idle_timeout, "idleAction": idle_action,
            "maxLifetimeSeconds": max_lifetime,
            "wakeOnConnect": wake_on_connect if idle_timeout else False,
        }.items() if v}
        if lifecycle:
            body["lifecycle"] = lifecycle
        body.update(spec or {})
        obj = kube.machines.create({
            "apiVersion": API_VERSION, "kind": "Machine",
            "metadata": {"name": name, "labels": {SANDBOX_LABEL: "true", **(labels or {})}},
            "spec": body,
        })
        sb = cls(kube, ui, name, uid=obj["metadata"]["uid"])
        try:
            kube.machines.wait(name, uid=sb.uid, timeout=timeout)
        except BaseException:
            sb.close()
            raise
        return sb

    @classmethod
    def claim_from(cls, kube: Client, ui: UIClient, pool: str, *, name: str | None = None,
                   ttl_seconds: int = 0, timeout: float = 60) -> "Sandbox":
        """Take a warm Machine from a MachinePool; close() releases the claim."""
        name = name or f"{pool}-claim-{uuid.uuid4().hex[:6]}"
        spec = {"poolName": _segment(pool), "labels": {SANDBOX_LABEL: "true"}}
        if ttl_seconds:
            spec["ttlSeconds"] = ttl_seconds
        claim = kube.claims.create({"apiVersion": API_VERSION, "kind": "MachineClaim",
                                    "metadata": {"name": name}, "spec": spec})
        deadline = time.monotonic() + timeout
        while True:
            st = kube.claims.get(name).get("status", {})
            if st.get("phase") == "Bound" and st.get("machineName"):
                machine = st["machineName"]
                break
            if time.monotonic() >= deadline:
                kube.claims.delete(name, uid=claim["metadata"]["uid"])
                raise TimeoutError(f"claim {name} not bound: {st.get('message', '')}")
            time.sleep(0.5)
        sb = cls(kube, ui, machine, claim=name)
        sb._claim_uid = claim["metadata"]["uid"]
        kube.machines.wait(machine, timeout=max(1, deadline - time.monotonic()) + 30)
        return sb

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def run(self, cmd: str | list[str], *, timeout_seconds: int = 60, check: bool = False) -> ExecResult:
        """Run cmd (a shell string, or an argv list) and wait for it."""
        out = self.ui.exec(self.name, _sh(cmd), timeout_seconds=timeout_seconds)
        res = ExecResult(int(out.get("exitCode", 0)), out.get("stdout", ""), out.get("stderr", ""))
        if check and not res.ok:
            raise CommandError(res, cmd if isinstance(cmd, str) else shlex.join(cmd))
        return res

    def start(self, cmd: str, *, log: str | None = None) -> Process:
        """Start cmd in the background, detached from the exec call."""
        log = log or f"/tmp/kairon-proc-{uuid.uuid4().hex[:8]}.log"
        res = self.run(["/bin/sh", "-c", 'nohup /bin/sh -c "$1" >"$2" 2>&1 </dev/null & echo $!', "sh", cmd, log],
                       check=True)
        return Process(self, int(res.stdout.strip()), log)

    def read_file(self, path: str) -> bytes:
        res = self.run(["base64", path], check=True)
        return base64.b64decode(res.stdout)

    def write_file(self, path: str, data: bytes, *, mode: int = 0o644) -> None:
        if not path.startswith("/") or not 0 <= mode <= 0o777:
            raise ValueError("write_file needs an absolute path and a mode <= 0777")
        enc = base64.b64encode(data).decode()
        self.run(["/bin/sh", "-c", ': >"$1"', "sh", path], check=True)
        for i in range(0, len(enc), _WRITE_CHUNK):
            self.run(["/bin/sh", "-c", 'printf %s "$1" | base64 -d >>"$2"', "sh", enc[i:i + _WRITE_CHUNK], path],
                     check=True)
        self.run(["chmod", format(mode, "o"), path], check=True)

    def pause(self, *, timeout: float = 60) -> None:
        self.kube.machines.power(self.name, "Paused")
        self.kube.machines.wait(self.name, phase="Paused", ready=False, timeout=timeout)

    def resume(self, *, timeout: float = 120) -> None:
        self.kube.machines.power(self.name, "Running")
        self.kube.machines.wait(self.name, timeout=timeout)

    def fork(self, name: str | None = None, *, ttl_seconds: int = 900, timeout: float = 120) -> "Sandbox":
        """A live copy of this sandbox (memory, CPU state and disk), Running in milliseconds."""
        name = name or f"{self.name}-fork-{uuid.uuid4().hex[:4]}"
        obj = self.kube.machines.fork(self.name, name, ttl_seconds=ttl_seconds)
        child = Sandbox(self.kube, self.ui, name, uid=obj["metadata"]["uid"])
        self.kube.machines.patch(name, {"metadata": {"labels": {SANDBOX_LABEL: "true"}}})
        self.kube.machines.wait(name, uid=child.uid, timeout=timeout)
        return child

    def close(self) -> None:
        """Delete the Machine, or release the claim (which deletes it per its reclaimPolicy)."""
        if self.claim:
            self.kube.claims.delete(self.claim, uid=getattr(self, "_claim_uid", ""))
        elif self.uid:
            self.kube.machines.delete(self.name, uid=self.uid)
        self.uid = self.claim = ""
