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
import {fromBase64, toBase64, type ExecResult, type Json, type UIClient} from './index.js';
import {CommandError} from './sandbox.js';

export class SessionError extends Error {}

export interface SessionSpecOptions {
  agent?: string; git?: string; workspace?: string; image?: string; cpu?: string; memory?: string;
  preset?: 'offline' | 'developer-safe' | 'open'; allowHosts?: string[];
  processAllow?: string[]; processDeny?: string[]; ttlSeconds?: number;
  budget?: Record<string, number>; policy?: Record<string, Json>;
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
  if (o.processAllow?.length || o.processDeny?.length) {
    const p: Record<string, Json> = {};
    if (o.processAllow?.length) p.allow = o.processAllow;
    if (o.processDeny?.length) p.deny = o.processDeny;
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

  private call<T = Json>(method: string, verb: string, body?: unknown, options: {query?: Record<string, string | number | boolean>; timeoutMs?: number} = {}): Promise<T> {
    return this.ui.sessionCall<T>(this.name, method, verb, body, options);
  }

  /** Run a shell command line in the workspace under the session policy. */
  async exec(command: string, o: {timeoutSeconds?: number; check?: boolean} = {}): Promise<ExecResult> {
    if (!command || typeof command !== 'string') throw new Error('command must be a non-empty string');
    const body: Record<string, Json> = {command};
    if (o.timeoutSeconds) body.timeout_seconds = o.timeoutSeconds;
    const out = await this.call<{exit_code?: number; stdout?: string; stderr?: string}>('POST', 'exec', body,
      {timeoutMs: ((o.timeoutSeconds ?? 600) + 30) * 1000});
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

  async startJob(command: string, o: {timeoutSeconds?: number; stdin?: boolean} = {}): Promise<string> {
    const body: Record<string, Json> = {command, stdin: o.stdin ?? false};
    if (o.timeoutSeconds) body.timeout_seconds = o.timeoutSeconds;
    return (await this.call<{job: string}>('POST', 'jobs', body)).job;
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
  async writeFile(path: string, data: Uint8Array, o: {mode?: number; parents?: boolean} = {}): Promise<void> {
    if (!path.startsWith('/')) throw new Error('writeFile needs an absolute path');
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
