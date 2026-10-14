// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
/**
 * Agent sessions: a FluxVM agent session (its own sandbox VM, a workspace, a
 * compiled policy, checkpoints and an event ledger) as a Kairon AgentSession,
 * driven entirely through kairon-ui.
 *
 *   const s = await AgentSession.create(ui, {agent: 'codex', git: 'https://github.com/org/repo', ttlSeconds: 3600});
 *   try { console.log((await s.exec('go test ./...')).stdout); } finally { await s.close(); }
 *
 * Verbs need an admin account, or a named operator when kairon-ui runs with
 * ui.auth.sandboxOperatorExec.
 */
import {APIError, fromBase64, toBase64, type ExecResult, type Json, type UIClient} from './index.js';
import {CommandError} from './sandbox.js';

export class SessionError extends Error {}

export interface SessionSpecOptions {
  agent?: string; git?: string; workspace?: string; image?: string; cpu?: string; memory?: string;
  preset?: 'offline' | 'developer-safe' | 'open'; allowHosts?: string[];
  processAllow?: string[]; processDeny?: string[]; processAsk?: string[]; ttlSeconds?: number;
  budget?: Record<string, number>; policy?: Record<string, Json>;
}

/** An approved command's id, or how long to hold the request open for the decision (1-120 s). */
export interface ApprovalOptions { approvalId?: string; approvalWaitSeconds?: number }

const UUID = /^[0-9a-fA-F-]{36}$/;
const INLINE_WRITE = 1 << 20;
const UPLOAD_CHUNK = 4 << 20;
const MAX_UPLOAD = 2 * 1024 * 1024 * 1024;

function fluxID(value: string): string {
  if (typeof value !== 'string' || !UUID.test(value)) throw new Error('expected a FluxVM id (UUID)');
  return value;
}

function applyApproval(body: Record<string, Json>, o: ApprovalOptions): number {
  if (o.approvalId) body.approval_id = fluxID(o.approvalId);
  if (o.approvalWaitSeconds) {
    if (!Number.isInteger(o.approvalWaitSeconds) || o.approvalWaitSeconds < 1 || o.approvalWaitSeconds > 120) throw new Error('approvalWaitSeconds is 1-120');
    body.approval_wait_seconds = o.approvalWaitSeconds;
  }
  return o.approvalWaitSeconds ?? 0;
}

async function sha256Hex(data: Uint8Array): Promise<string> {
  const view = data.buffer instanceof ArrayBuffer ? data as Uint8Array<ArrayBuffer> : new Uint8Array(data);
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', view));
  return Array.from(digest, b => b.toString(16).padStart(2, '0')).join('');
}

/** An AgentSession spec from options; policy entries override the generated policy. */
export function sessionSpec(o: SessionSpecOptions = {}): Record<string, Json> {
  const spec: Record<string, Json> = {};
  const pick = (m: Record<string, string | undefined>) => Object.fromEntries(Object.entries(m).filter(([, v]) => v)) as Record<string, string>;
  if (o.agent) spec.agent = o.agent;
  if (o.git || o.workspace) spec.workspace = pick({git: o.git, path: o.workspace});
  if (o.image) spec.image = o.image;
  if (o.cpu || o.memory) spec.resources = pick({cpu: o.cpu, memory: o.memory});
  const policy: Record<string, Json> = {};
  if (o.preset) policy.preset = o.preset;
  if (o.allowHosts?.length) policy.network = {allow: o.allowHosts.map(host => ({host}))};
  if (o.processAllow?.length || o.processDeny?.length || o.processAsk?.length) {
    const p: Record<string, Json> = {};
    if (o.processAllow?.length) p.allow = o.processAllow;
    if (o.processDeny?.length) p.deny = o.processDeny;
    if (o.processAsk?.length) p.ask = o.processAsk;
    policy.process = p;
  }
  Object.assign(policy, o.policy ?? {});
  if (Object.keys(policy).length) spec.policy = policy;
  if (o.ttlSeconds) spec.ttlSeconds = o.ttlSeconds;
  if (o.budget) spec.budget = {...o.budget};
  return spec;
}

function suffix(n: number): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(n)), b => (b % 16).toString(16)).join('');
}

/** One AgentSession; call close() when done. */
export class AgentSession {
  constructor(readonly ui: UIClient, readonly name: string) {}

  /** Create an AgentSession and wait until it is Running. */
  static async create(ui: UIClient, o: SessionSpecOptions & {name?: string; labels?: Record<string, string>; timeoutMs?: number} = {}): Promise<AgentSession> {
    const name = o.name ?? `session-${suffix(8)}`;
    await ui.createSession(name, sessionSpec(o), o.labels ?? {});
    const s = new AgentSession(ui, name);
    try { await s.wait(o.timeoutMs ?? 600000); } catch (error) { await s.close(); throw error; }
    return s;
  }

  async status(): Promise<Record<string, unknown>> {
    return (await this.ui.getSession(this.name)).status ?? {};
  }

  async wait(timeoutMs = 600000, intervalMs = 1000): Promise<Record<string, unknown>> {
    const deadline = performance.now() + timeoutMs;
    while (true) {
      const st = await this.status();
      const phase = String(st.phase ?? '');
      if (phase === 'Running') return st;
      if (phase === 'Failed' || phase === 'Ended') throw new SessionError(`agentsession ${this.name} is ${phase}: ${st.message ?? ''}`);
      if (performance.now() >= deadline) throw new Error(`agentsession ${this.name} not Running: ${st.message ?? (phase || 'Pending')}`);
      await new Promise(r => setTimeout(r, intervalMs));
    }
  }

  private call<T = Json>(method: string, verb: string, body?: unknown, options: {query?: Record<string, string | number | boolean>; timeoutMs?: number; data?: Uint8Array} = {}): Promise<T> {
    return this.ui.sessionCall<T>(this.name, method, verb, body, options);
  }

  /**
   * Run a shell command line in the workspace under the session policy. A command matching a
   * policy.process.ask rule throws ApprovalRequired; once a human admin approved it, call again
   * with approvalId.
   */
  async exec(command: string, o: {timeoutSeconds?: number; check?: boolean} & ApprovalOptions = {}): Promise<ExecResult> {
    if (!command || typeof command !== 'string') throw new Error('command must be a non-empty string');
    const body: Record<string, Json> = {command};
    if (o.timeoutSeconds) body.timeout_seconds = o.timeoutSeconds;
    const wait = applyApproval(body, o);
    const out = await this.call<{exit_code?: number; stdout?: string; stderr?: string}>('POST', 'exec', body,
      {timeoutMs: ((o.timeoutSeconds ?? 600) + wait + 30) * 1000});
    const res = {exitCode: out?.exit_code ?? 0, stdout: out?.stdout ?? '', stderr: out?.stderr ?? ''};
    if (o.check && res.exitCode !== 0) throw new CommandError(res, command);
    return res;
  }

  checkpoint(tag?: string, memory = true): Promise<Json> {
    const body: Record<string, Json> = memory ? {} : {memory: false};
    if (tag) body.tag = tag;
    return this.call('POST', 'checkpoint', body, {timeoutMs: 600000});
  }
  restore(tag: string): Promise<Json> { return this.call('POST', 'restore', {tag}, {timeoutMs: 600000}); }

  /** Live copies of this session; each child is its own AgentSession, returned Running. */
  async fork(count = 1, o: {identity?: string; timeoutMs?: number} = {}): Promise<AgentSession[]> {
    const body: Record<string, Json> = {count};
    if (o.identity) body.identity = o.identity;
    const out = await this.call<{agentSessions?: string[]; agentSessionErrors?: string[]}>('POST', 'fork', body, {timeoutMs: 600000});
    if (out?.agentSessionErrors?.length) throw new SessionError('fork children without an AgentSession: ' + out.agentSessionErrors.join('; '));
    const kids = (out?.agentSessions ?? []).map(n => new AgentSession(this.ui, n));
    await Promise.all(kids.map(k => k.wait(o.timeoutMs ?? 300000)));
    return kids;
  }

  async startJob(command: string, o: {timeoutSeconds?: number; stdin?: boolean} & ApprovalOptions = {}): Promise<string> {
    const body: Record<string, Json> = {command, stdin: o.stdin ?? false};
    if (o.timeoutSeconds) body.timeout_seconds = o.timeoutSeconds;
    const wait = applyApproval(body, o);
    return (await this.call<{job: string}>('POST', 'jobs', body, {timeoutMs: (60 + wait) * 1000})).job;
  }

  /** Approval requests for commands matching policy.process.ask, newest first. */
  async approvals(pendingOnly = false): Promise<Record<string, Json>[]> {
    const items = (await this.call<{items?: Record<string, Json>[]}>('GET', 'approvals'))?.items ?? [];
    return pendingOnly ? items.filter(a => a.state === 'pending') : items;
  }
  /** Approve a pending command (admin accounts only; kairon-ui records who decided). */
  approve(approvalId: string, note = ''): Promise<Json> {
    return this.call('POST', `approvals/${fluxID(approvalId)}/approve`, note ? {note} : {});
  }
  deny(approvalId: string, note = ''): Promise<Json> {
    return this.call('POST', `approvals/${fluxID(approvalId)}/deny`, note ? {note} : {});
  }

  /** Which tools the guest image has and which session features are ready. */
  probe(): Promise<Record<string, Json>> { return this.call('GET', 'probe', undefined, {timeoutMs: 60000}); }

  /**
   * Let the session's requests to hosts carry a credential it never sees (admin only). value is
   * the full header value; without it FluxVM uses the node's FLUXVM_SECRET_<REF>.
   */
  grant(secretRef: string, hosts: string[], o: {value?: string; methods?: string[]; paths?: string[]; ttlSeconds?: number} = {}): Promise<Json> {
    if (!hosts.length) throw new Error('hosts must name at least one host');
    const body: Record<string, Json> = {secret_ref: secretRef, hosts};
    if (o.value) body.value = o.value;
    if (o.methods?.length) body.methods = o.methods;
    if (o.paths?.length) body.paths = o.paths;
    if (o.ttlSeconds) body.ttl_seconds = o.ttlSeconds;
    return this.call('POST', 'grants', body);
  }
  async grants(): Promise<Record<string, Json>[]> {
    return (await this.call<{grants?: Record<string, Json>[]}>('GET', 'grants'))?.grants ?? [];
  }
  /** Revoke one grant, or every grant of the session without an id. */
  revokeGrant(grantId?: string): Promise<Json> {
    return this.call('DELETE', grantId === undefined ? 'grants' : `grants/${grantId}`);
  }

  /** Write up to 2 GiB in chunks; the guest checks the SHA-256 before the file appears. */
  async upload(path: string, data: Uint8Array, o: {mode?: number; parents?: boolean; chunkSize?: number} = {}): Promise<Json> {
    const chunk = o.chunkSize ?? UPLOAD_CHUNK;
    if (!path.startsWith('/')) throw new Error('upload needs an absolute path');
    if (data.byteLength < 1 || data.byteLength > MAX_UPLOAD || chunk < 1 || chunk > UPLOAD_CHUNK) {
      throw new Error('uploads are 1 byte to 2 GiB in chunks of at most 4 MiB');
    }
    const body: Record<string, Json> = {path, size: data.byteLength, sha256: await sha256Hex(data), parents: o.parents ?? true};
    if (o.mode !== undefined) body.mode = o.mode;
    const up = await this.call<{id: string}>('POST', 'uploads', body);
    const verb = `uploads/${fluxID(up.id)}`;
    try {
      let offset = 0;
      let resyncs = 0;
      while (offset < data.byteLength) {
        const part = data.subarray(offset, offset + chunk);
        try {
          await this.call('PUT', verb, undefined, {query: {offset}, data: part, timeoutMs: 300000});
        } catch (error) {
          if (!(error instanceof APIError && error.status === 409) || resyncs >= 5) throw error;
          resyncs++;
          const cur = await this.call<{state?: string; received?: number}>('GET', verb);
          if (cur?.state === 'complete') return cur as Json;
          offset = cur?.received ?? 0;
          continue;
        }
        offset += part.byteLength;
      }
      return await this.call('POST', `${verb}/complete`, undefined, {timeoutMs: 900000});
    } catch (error) {
      await this.call('DELETE', verb).catch(() => undefined);
      throw error;
    }
  }
  /** One read of a job: state, exit_code, output from offset and next_offset. */
  job(job: string, o: {offset?: number; stderrOffset?: number; waitMs?: number} = {}): Promise<Record<string, Json>> {
    const query: Record<string, number> = {offset: o.offset ?? 0, stderr_offset: o.stderrOffset ?? 0};
    if (o.waitMs) query.wait_ms = o.waitMs;
    return this.call('GET', `jobs/${job}`, undefined, {query, timeoutMs: 60000});
  }
  jobs(): Promise<Json> { return this.call('GET', 'jobs'); }
  async killJob(job: string, signal = ''): Promise<void> {
    await this.call('DELETE', `jobs/${job}`, undefined, signal ? {query: {signal}} : {});
  }

  listFiles(path = '/workspace'): Promise<Json> { return this.call('GET', 'files', undefined, {query: {path}}); }
  async readFile(path: string): Promise<Uint8Array> {
    const out = await this.call<{content_base64?: string}>('GET', 'files/content', undefined, {query: {path}});
    return fromBase64(out?.content_base64 ?? '');
  }
  /** Write a guest file; above 1 MiB it goes through upload(). */
  async writeFile(path: string, data: Uint8Array, o: {mode?: number; parents?: boolean} = {}): Promise<void> {
    if (!path.startsWith('/')) throw new Error('writeFile needs an absolute path');
    if (data.byteLength > INLINE_WRITE) {
      await this.upload(path, data, o);
      return;
    }
    const body: Record<string, Json> = {path, content_base64: toBase64(data), parents: o.parents ?? true};
    if (o.mode !== undefined) body.mode = o.mode;
    await this.call('PUT', 'files/content', body);
  }
  async remove(path: string, recursive = false): Promise<void> {
    await this.call('DELETE', 'files', undefined, {query: {path, recursive}});
  }

  /** git status, diff or log (query: repo, staged, path, limit). */
  git(view: 'status' | 'diff' | 'log' = 'status', query: Record<string, string | number | boolean> = {}): Promise<Json> {
    if (!['status', 'diff', 'log'].includes(view)) throw new Error('view must be status, diff or log');
    return this.call('GET', `git/${view}`, undefined, Object.keys(query).length ? {query} : {});
  }
  commit(message: string, repo = ''): Promise<Json> { return this.call('POST', 'git/commit', repo ? {message, repo} : {message}); }
  branch(name: string, repo = ''): Promise<Json> { return this.call('POST', 'git/branch', repo ? {name, repo} : {name}); }

  /** An expiring preview URL for a guest port; url is a kairon-ui path. The token is a credential. */
  expose(port: number, ttlSeconds = 0): Promise<{token: string; url: string; port: number; expires_at: string; served: boolean}> {
    return this.call('POST', 'preview', ttlSeconds ? {port, ttl_seconds: ttlSeconds} : {port});
  }
  async previews(): Promise<Record<string, Json>[]> {
    return (await this.call<{items?: Record<string, Json>[]}>('GET', 'previews'))?.items ?? [];
  }
  async unexpose(token: string): Promise<void> { await this.call('DELETE', `preview/${token}`); }

  trajectory(): Promise<Record<string, Json>> { return this.call('GET', 'trajectory'); }
  events(since = ''): Promise<Json> { return this.call('GET', 'events', undefined, since ? {query: {since}} : {}); }

  /** Delete the AgentSession; kairon-node ends the FluxVM session. */
  async close(): Promise<void> { await this.ui.deleteSession(this.name); }
}
