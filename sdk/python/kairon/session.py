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
import hashlib
import re
import time
import uuid
from typing import Any
from urllib.parse import quote

from .client import APIError, UIClient, _segment
from .sandbox import CommandError, ExecResult

_EXEC_SLACK = 30
INLINE_WRITE = 1 << 20
UPLOAD_CHUNK = 4 << 20
MAX_UPLOAD = 2 << 30
_UUID = re.compile(r"[0-9a-fA-F-]{36}")
INPUT_ACTIONS = frozenset({"type", "key", "move", "click", "double_click", "right_click", "middle_click",
                           "down", "up", "drag", "scroll"})


class SessionError(RuntimeError):
    pass


def _uuid(value: str) -> str:
    if not isinstance(value, str) or not _UUID.fullmatch(value):
        raise ValueError("expected a FluxVM id (UUID)")
    return value


def _approval(body: dict, approval_id: str, wait_seconds: int) -> int:
    """Adds the approval fields to body; returns the seconds the request may be held."""
    if approval_id:
        body["approval_id"] = _uuid(approval_id)
    if wait_seconds:
        if not 0 < int(wait_seconds) <= 120:
            raise ValueError("approval_wait_seconds is 1-120")
        body["approval_wait_seconds"] = int(wait_seconds)
    return int(wait_seconds or 0)


def session_spec(*, agent: str = "", git: str = "", workspace: str = "", image: str = "", engine: str = "",
                 cpu: str = "", memory: str = "", preset: str = "", allow_hosts: tuple | list = (),
                 process_allow: tuple | list = (), process_deny: tuple | list = (),
                 process_ask: tuple | list = (),
                 ttl_seconds: int = 0, budget: dict | None = None, policy: dict | None = None) -> dict:
    """An AgentSession spec from keyword arguments; policy entries override the generated policy."""
    spec: dict[str, Any] = {}
    if agent:
        spec["agent"] = agent
    if git or workspace:
        spec["workspace"] = {k: v for k, v in {"git": git, "path": workspace}.items() if v}
    if image:
        spec["image"] = image
    if engine:
        spec["engine"] = engine
    if cpu or memory:
        spec["resources"] = {k: v for k, v in {"cpu": cpu, "memory": memory}.items() if v}
    pol: dict[str, Any] = {}
    if preset:
        pol["preset"] = preset
    if allow_hosts:
        pol["network"] = {"allow": [{"host": h} for h in allow_hosts]}
    if process_allow or process_deny or process_ask:
        pol["process"] = {k: list(v) for k, v in
                          {"allow": process_allow, "deny": process_deny, "ask": process_ask}.items() if v}
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

    def exec(self, command: str, *, timeout_seconds: int | None = None, check: bool = False,
             approval_id: str = "", approval_wait_seconds: int = 0) -> ExecResult:
        """Run a shell command line in the workspace under the session policy.

        A command matching a policy.process.ask rule raises ApprovalRequired; once a human
        admin approved it, call again with approval_id. approval_wait_seconds (at most 120)
        holds the request open for the decision instead.
        """
        if not command or not isinstance(command, str):
            raise ValueError("command must be a non-empty string")
        body: dict[str, Any] = {"command": command}
        if timeout_seconds:
            body["timeout_seconds"] = int(timeout_seconds)
        wait = _approval(body, approval_id, approval_wait_seconds)
        out = self._call("POST", "exec", body, timeout=(timeout_seconds or 600) + wait + _EXEC_SLACK) or {}
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

    def start_job(self, command: str, *, timeout_seconds: int = 0, stdin: bool = False,
                  approval_id: str = "", approval_wait_seconds: int = 0) -> str:
        body: dict[str, Any] = {"command": command, "stdin": stdin}
        if timeout_seconds:
            body["timeout_seconds"] = int(timeout_seconds)
        wait = _approval(body, approval_id, approval_wait_seconds)
        return self._call("POST", "jobs", body, timeout=60 + wait)["job"]

    def approvals(self, *, pending_only: bool = False) -> list[dict]:
        """Approval requests for commands matching policy.process.ask, newest first."""
        items = (self._call("GET", "approvals") or {}).get("items", [])
        return [a for a in items if a.get("state") == "pending"] if pending_only else items

    def approve(self, approval_id: str, *, note: str = "") -> dict:
        """Approve a pending command (admin accounts only; kairon-ui records who decided)."""
        return self._call("POST", f"approvals/{_uuid(approval_id)}/approve", {"note": note} if note else {})

    def deny(self, approval_id: str, *, note: str = "") -> dict:
        return self._call("POST", f"approvals/{_uuid(approval_id)}/deny", {"note": note} if note else {})

    def probe(self) -> dict:
        """Which tools the guest image has and which session features are ready."""
        return self._call("GET", "probe", timeout=60)

    def screenshot(self, *, max_width: int | None = None) -> bytes:
        """The guest display of a Mac (vz) session as PNG bytes; max_width scales it down."""
        query = {"max_width": int(max_width)} if max_width else None
        return self._call("GET", "screenshot", query=query, timeout=60, binary=True)

    def input(self, *actions: dict) -> dict:
        """Keyboard and mouse input to a Mac session's display, sent in order. Actions are FluxVM's, e.g.
        ``{"action": "click", "x": 640, "y": 400, "screen_width": 1280}``, ``{"action": "type", "text": "ls"}``,
        ``{"action": "key", "key": "enter", "modifiers": ["command"]}``. Typed text stays out of the ledger."""
        if not 0 < len(actions) <= 256:
            raise ValueError("send 1-256 actions")
        for a in actions:
            if not isinstance(a, dict) or a.get("action") not in INPUT_ACTIONS:
                raise ValueError("each action is a dict whose action is one of " + ", ".join(sorted(INPUT_ACTIONS)))
        body = dict(actions[0]) if len(actions) == 1 else {"actions": [dict(a) for a in actions]}
        return self._call("POST", "input", body, timeout=120)

    def grant(self, secret_ref: str, hosts: list[str], *, value: str | None = None, methods: tuple | list = (),
              paths: tuple | list = (), ttl_seconds: int = 0) -> dict:
        """Let the session's requests to hosts carry a credential it never sees (admin only).

        value is the full header value (e.g. "Bearer ..."); without it FluxVM uses the node's
        FLUXVM_SECRET_<REF>. The answer never includes the value.
        """
        if not hosts:
            raise ValueError("hosts must name at least one host")
        body: dict[str, Any] = {"secret_ref": secret_ref, "hosts": list(hosts)}
        if value:
            body["value"] = value
        if methods:
            body["methods"] = list(methods)
        if paths:
            body["paths"] = list(paths)
        if ttl_seconds:
            body["ttl_seconds"] = int(ttl_seconds)
        return self._call("POST", "grants", body)

    def grants(self) -> list[dict]:
        return (self._call("GET", "grants") or {}).get("grants", [])

    def revoke_grant(self, grant_id: str | None = None) -> Any:
        """Revoke one grant, or every grant of the session when grant_id is None."""
        return self._call("DELETE", "grants" if grant_id is None else "grants/" + quote(grant_id, safe=""))

    def upload(self, path: str, data: bytes, *, mode: int | None = None, parents: bool = True,
               chunk_size: int = UPLOAD_CHUNK) -> dict:
        """Write up to 2 GiB in chunks; the guest checks the SHA-256 before the file appears."""
        if not path.startswith("/"):
            raise ValueError("upload needs an absolute path")
        if not 0 < len(data) <= MAX_UPLOAD or not 0 < chunk_size <= UPLOAD_CHUNK:
            raise ValueError("uploads are 1 byte to 2 GiB in chunks of at most 4 MiB")
        body: dict[str, Any] = {"path": path, "size": len(data), "sha256": hashlib.sha256(data).hexdigest(),
                                "parents": parents}
        if mode is not None:
            body["mode"] = mode
        up = self._call("POST", "uploads", body)
        verb = "uploads/" + _uuid(up["id"])
        try:
            offset, resyncs = 0, 0
            while offset < len(data):
                chunk = data[offset:offset + chunk_size]
                try:
                    self._call("PUT", verb, query={"offset": offset}, data=chunk, timeout=300)
                except APIError as exc:
                    if exc.status != 409 or resyncs >= 5:
                        raise
                    resyncs += 1
                    cur = self._call("GET", verb)
                    if cur.get("state") == "complete":
                        return cur
                    offset = int(cur.get("received", 0))
                    continue
                offset += len(chunk)
            done = self._call("POST", verb + "/complete", timeout=900)
        except BaseException:
            try:
                self._call("DELETE", verb)
            except APIError:
                pass
            raise
        return done

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
        """Write a guest file; above 1 MiB it goes through upload()."""
        if not path.startswith("/"):
            raise ValueError("write_file needs an absolute path")
        if len(data) > INLINE_WRITE:
            self.upload(path, data, mode=mode, parents=parents)
            return
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
