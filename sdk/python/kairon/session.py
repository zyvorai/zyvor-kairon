# Copyright 2026 Zyvor · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
"""Agent sessions: a FluxVM agent session (its own sandbox VM, a workspace,
a compiled policy, checkpoints and an event ledger) as a Kairon AgentSession,
driven entirely through kairon-ui.

    with AgentSession.create(ui, agent="codex", git="https://github.com/org/repo",
                             preset="developer-safe", ttl_seconds=3600) as s:
        print(s.exec("go test ./...").stdout)
        s.checkpoint("green")
        for child in s.fork(3):
            child.exec("./try-fix.sh")

Verbs need an admin account, or a named operator when kairon-ui runs with
ui.auth.sandboxOperatorExec.
"""
from __future__ import annotations

import base64
import time
import uuid
from typing import Any

from .client import UIClient, _segment
from .sandbox import CommandError, ExecResult

_EXEC_SLACK = 30


class SessionError(RuntimeError):
    pass


def session_spec(*, agent: str = "", git: str = "", workspace: str = "", image: str = "",
                 cpu: str = "", memory: str = "", preset: str = "", allow_hosts: tuple | list = (),
                 process_allow: tuple | list = (), process_deny: tuple | list = (),
                 ttl_seconds: int = 0, budget: dict | None = None, policy: dict | None = None) -> dict:
    """An AgentSession spec from keyword arguments; policy entries override the generated policy."""
    spec: dict[str, Any] = {}
    if agent:
        spec["agent"] = agent
    if git or workspace:
        spec["workspace"] = {k: v for k, v in {"git": git, "path": workspace}.items() if v}
    if image:
        spec["image"] = image
    if cpu or memory:
        spec["resources"] = {k: v for k, v in {"cpu": cpu, "memory": memory}.items() if v}
    pol: dict[str, Any] = {}
    if preset:
        pol["preset"] = preset
    if allow_hosts:
        pol["network"] = {"allow": [{"host": h} for h in allow_hosts]}
    if process_allow or process_deny:
        pol["process"] = {k: list(v) for k, v in {"allow": process_allow, "deny": process_deny}.items() if v}
    pol.update(policy or {})
    if pol:
        spec["policy"] = pol
    if ttl_seconds:
        spec["ttlSeconds"] = int(ttl_seconds)
    if budget:
        spec["budget"] = dict(budget)
    return spec


class AgentSession:
    """One AgentSession. Use as a context manager, or call close()."""
    def __init__(self, ui: UIClient, name: str):
        self.ui, self.name = ui, _segment(name)

    @classmethod
    def create(cls, ui: UIClient, *, name: str | None = None, timeout: float = 600,
               labels: dict | None = None, **spec_args) -> "AgentSession":
        """Create an AgentSession and wait until it is Running. spec_args go to session_spec."""
        name = name or "session-" + uuid.uuid4().hex[:8]
        ui.create_session(name, session_spec(**spec_args), labels=labels)
        s = cls(ui, name)
        try:
            s.wait(timeout=timeout)
        except BaseException:
            s.close()
            raise
        return s

    def __enter__(self) -> "AgentSession":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def status(self) -> dict:
        return self.ui.get_session(self.name).get("status", {})

    def wait(self, *, timeout: float = 600, interval: float = 1) -> dict:
        deadline = time.monotonic() + timeout
        while True:
            st = self.status()
            phase = st.get("phase", "")
            if phase == "Running":
                return st
            if phase in ("Failed", "Ended"):
                raise SessionError(f"agentsession {self.name} is {phase}: {st.get('message', '')}")
            if time.monotonic() >= deadline:
                raise TimeoutError(f"agentsession {self.name} not Running: {st.get('message', phase or 'Pending')}")
            time.sleep(interval)

    def _call(self, method: str, verb: str, body: Any = None, **kw) -> Any:
        return self.ui.session_call(self.name, method, verb, body, **kw)

    def exec(self, command: str, *, timeout_seconds: int | None = None, check: bool = False) -> ExecResult:
        """Run a shell command line in the workspace under the session policy."""
        if not command or not isinstance(command, str):
            raise ValueError("command must be a non-empty string")
        body: dict[str, Any] = {"command": command}
        if timeout_seconds:
            body["timeout_seconds"] = int(timeout_seconds)
        out = self._call("POST", "exec", body, timeout=(timeout_seconds or 600) + _EXEC_SLACK) or {}
        res = ExecResult(int(out.get("exit_code") or 0), out.get("stdout", ""), out.get("stderr", ""))
        if check and not res.ok:
            raise CommandError(res, command)
        return res

    def checkpoint(self, tag: str | None = None, *, memory: bool = True) -> dict:
        body: dict[str, Any] = {} if memory else {"memory": False}
        if tag:
            body["tag"] = tag
        return self._call("POST", "checkpoint", body, timeout=600)

    def restore(self, tag: str) -> dict:
        return self._call("POST", "restore", {"tag": tag}, timeout=600)

    def fork(self, count: int = 1, *, identity: str = "", timeout: float = 300) -> list["AgentSession"]:
        """Live copies of this session; each child is its own AgentSession, returned Running."""
        body: dict[str, Any] = {"count": int(count)}
        if identity:
            body["identity"] = identity
        out = self._call("POST", "fork", body, timeout=600) or {}
        if out.get("agentSessionErrors"):
            raise SessionError("fork children without an AgentSession: " + "; ".join(out["agentSessionErrors"]))
        children = [AgentSession(self.ui, n) for n in out.get("agentSessions", [])]
        for c in children:
            c.wait(timeout=timeout)
        return children

    def start_job(self, command: str, *, timeout_seconds: int = 0, stdin: bool = False) -> str:
        body: dict[str, Any] = {"command": command, "stdin": stdin}
        if timeout_seconds:
            body["timeout_seconds"] = int(timeout_seconds)
        return self._call("POST", "jobs", body)["job"]

    def job(self, job: str, *, offset: int = 0, stderr_offset: int = 0, wait_ms: int = 0) -> dict:
        """One read of a job: state, exit_code, output from offset and next_offset."""
        q = {"offset": offset, "stderr_offset": stderr_offset}
        if wait_ms:
            q["wait_ms"] = wait_ms
        return self._call("GET", "jobs/" + _segment(job), query=q, timeout=60)

    def jobs(self) -> Any:
        return self._call("GET", "jobs")

    def kill_job(self, job: str, *, signal: str = "") -> None:
        self._call("DELETE", "jobs/" + _segment(job), query={"signal": signal} if signal else None)

    def list_files(self, path: str = "/workspace") -> Any:
        return self._call("GET", "files", query={"path": path})

    def read_file(self, path: str) -> bytes:
        out = self._call("GET", "files/content", query={"path": path}) or {}
        return base64.b64decode(out.get("content_base64", ""))

    def write_file(self, path: str, data: bytes, *, mode: int | None = None, parents: bool = True) -> None:
        if not path.startswith("/"):
            raise ValueError("write_file needs an absolute path")
        body: dict[str, Any] = {"path": path, "content_base64": base64.b64encode(data).decode(), "parents": parents}
        if mode is not None:
            body["mode"] = mode
        self._call("PUT", "files/content", body)

    def remove(self, path: str, *, recursive: bool = False) -> None:
        self._call("DELETE", "files", query={"path": path, "recursive": str(recursive).lower()})

    def git(self, view: str = "status", **query) -> Any:
        """git status, diff or log (query: repo, staged, path, limit)."""
        if view not in ("status", "diff", "log"):
            raise ValueError("view must be status, diff or log")
        return self._call("GET", "git/" + view, query={k: v for k, v in query.items() if v} or None)

    def commit(self, message: str, *, repo: str = "") -> Any:
        return self._call("POST", "git/commit", {"message": message, **({"repo": repo} if repo else {})})

    def branch(self, name: str, *, repo: str = "") -> Any:
        return self._call("POST", "git/branch", {"name": name, **({"repo": repo} if repo else {})})

    def expose(self, port: int, *, ttl_seconds: int = 0) -> dict:
        """An expiring preview URL for a guest port; url is a kairon-ui path. The token is a credential."""
        body: dict[str, Any] = {"port": int(port)}
        if ttl_seconds:
            body["ttl_seconds"] = int(ttl_seconds)
        return self._call("POST", "preview", body)

    def previews(self) -> list[dict]:
        return (self._call("GET", "previews") or {}).get("items", [])

    def unexpose(self, token: str) -> None:
        self._call("DELETE", "preview/" + _segment(token))

    def trajectory(self) -> dict:
        return self._call("GET", "trajectory")

    def events(self, *, since: str = "") -> Any:
        return self._call("GET", "events", query={"since": since} if since else None)

    def close(self) -> None:
        """Delete the AgentSession; kairon-node ends the FluxVM session."""
        self.ui.delete_session(self.name)
