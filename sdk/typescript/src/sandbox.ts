// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
/**
 * Agent sandboxes: one Machine driven through the Kubernetes API (lifecycle)
 * and kairon-ui (commands and files).
 *
 *   const sb = await Sandbox.create(kube, ui, {image: '/var/lib/kairon/images/agent.qcow2', idleTimeoutSeconds: 600});
 *   try { console.log((await sb.run('uname -a')).stdout); } finally { await sb.close(); }
 *
 * Commands use qemu-guest-agent exec (spec.guestAgent.enabled); files travel
 * base64-encoded over the same channel.
 */
import {API_VERSION, fromBase64, toBase64, type Client, type ExecResult, type Json, type UIClient} from './index.js';

export const SANDBOX_LABEL = 'kairon.zyvor.dev/sandbox';
// One exec argument carries at most this much base64 (Linux MAX_ARG_STRLEN is 128 KiB).
const WRITE_CHUNK = 64 * 1024;

export class CommandError extends Error {
  readonly result: ExecResult;
  constructor(result: ExecResult, cmd: string) {
    super(`command exited ${result.exitCode}: ${cmd}: ${result.stderr.trim().slice(0, 500)}`);
    this.result = result;
  }
}

export interface SandboxOptions {
  image: string; name?: string; cpu?: string; memory?: string; backend?: string;
  idleTimeoutSeconds?: number; idleAction?: 'Pause' | 'Hibernate' | 'Halt' | 'Stop' | 'Delete';
  maxLifetimeSeconds?: number; wakeOnConnect?: boolean;
  labels?: Record<string, string>; spec?: Record<string, Json>; timeoutMs?: number;
}

function suffix(n: number): string {
  return Array.from(crypto.getRandomValues(new Uint8Array(n)), b => (b % 36).toString(36)).join('');
}
function sh(cmd: string | string[]): string[] {
  if (typeof cmd === 'string') return ['/bin/sh', '-c', cmd];
  if (!cmd.length || cmd.some(a => typeof a !== 'string')) throw new Error('cmd must be a shell string or a non-empty argv list');
  return [...cmd];
}

/** A background command started by Sandbox.start; output goes to a log file. */
export class Process {
  private offset = 0;
  constructor(readonly sandbox: Sandbox, readonly pid: number, readonly log: string) {}
  /** Output written since the previous call. */
  async logs(): Promise<string> {
    const out = (await this.sandbox.run(['tail', '-c', `+${this.offset + 1}`, this.log])).stdout;
    this.offset += new TextEncoder().encode(out).length;
    return out;
  }
  async alive(): Promise<boolean> {
    return (await this.sandbox.run(['kill', '-0', String(this.pid)])).exitCode === 0;
  }
  async kill(signal = 'TERM'): Promise<void> {
    await this.sandbox.run(['kill', `-${signal}`, String(this.pid)]);
  }
}

export class Sandbox {
  constructor(readonly kube: Client, readonly ui: UIClient, readonly name: string,
              private uid = '', private claim = '', private claimUid = '') {}

  /** Create a sandbox Machine and wait until it is Running. */
  static async create(kube: Client, ui: UIClient, o: SandboxOptions): Promise<Sandbox> {
    const name = o.name ?? `sandbox-${suffix(8)}`;
    const lifecycle: Record<string, Json> = {};
    if (o.idleTimeoutSeconds) {
      lifecycle.idleTimeoutSeconds = o.idleTimeoutSeconds;
      lifecycle.wakeOnConnect = o.wakeOnConnect ?? true;
    }
    if (o.idleAction) lifecycle.idleAction = o.idleAction;
    if (o.maxLifetimeSeconds) lifecycle.maxLifetimeSeconds = o.maxLifetimeSeconds;
    const spec: Record<string, Json> = {
      image: {path: o.image}, resources: {cpu: o.cpu ?? '2', memory: o.memory ?? '2Gi'},
      runtime: {backend: o.backend ?? 'qemu'}, network: {mode: 'user'},
      guestAgent: {enabled: true}, powerState: 'Running',
      ...(Object.keys(lifecycle).length ? {lifecycle} : {}), ...(o.spec ?? {}),
    };
    const obj = await kube.machines.create({apiVersion: API_VERSION, kind: 'Machine',
      metadata: {name, labels: {[SANDBOX_LABEL]: 'true', ...(o.labels ?? {})}}, spec});
    const sb = new Sandbox(kube, ui, name, obj.metadata.uid ?? '');
    try {
      await kube.machines.wait(name, {uid: sb.uid, timeoutMs: o.timeoutMs ?? 180000});
    } catch (error) { await sb.close(); throw error; }
    return sb;
  }

  /** Take a warm Machine from a MachinePool; close() releases the claim. */
  static async claimFrom(kube: Client, ui: UIClient, pool: string, o: {name?: string; ttlSeconds?: number; timeoutMs?: number} = {}): Promise<Sandbox> {
    const name = o.name ?? `${pool}-claim-${suffix(6)}`;
    const claim = await kube.claims.create({apiVersion: API_VERSION, kind: 'MachineClaim', metadata: {name},
      spec: {poolName: pool, labels: {[SANDBOX_LABEL]: 'true'}, ...(o.ttlSeconds ? {ttlSeconds: o.ttlSeconds} : {})}});
    const deadline = performance.now() + (o.timeoutMs ?? 60000);
    while (true) {
      const st = (await kube.claims.get(name)).status ?? {};
      if (st.phase === 'Bound' && typeof st.machineName === 'string') {
        const sb = new Sandbox(kube, ui, st.machineName, '', name, claim.metadata.uid ?? '');
        await kube.machines.wait(st.machineName, {timeoutMs: Math.max(1000, deadline - performance.now()) + 30000});
        return sb;
      }
      if (performance.now() >= deadline) {
        await kube.claims.delete(name, claim.metadata.uid ?? '');
        throw new Error(`claim ${name} not bound: ${String(st.message ?? '')}`);
      }
      await new Promise(r => setTimeout(r, 500));
    }
  }

  /** Run cmd (a shell string, or an argv list) and wait for it. */
  async run(cmd: string | string[], o: {timeoutSeconds?: number; check?: boolean} = {}): Promise<ExecResult> {
    const out = await this.ui.exec(this.name, sh(cmd), o.timeoutSeconds ?? 60);
    const res = {exitCode: Number(out.exitCode ?? 0), stdout: out.stdout ?? '', stderr: out.stderr ?? ''};
    if (o.check && res.exitCode !== 0) throw new CommandError(res, typeof cmd === 'string' ? cmd : cmd.join(' '));
    return res;
  }

  /** Start cmd in the background, detached from the exec call. */
  async start(cmd: string, log = `/tmp/kairon-proc-${suffix(8)}.log`): Promise<Process> {
    const res = await this.run(['/bin/sh', '-c', 'nohup /bin/sh -c "$1" >"$2" 2>&1 </dev/null & echo $!', 'sh', cmd, log], {check: true});
    return new Process(this, Number.parseInt(res.stdout.trim(), 10), log);
  }

  async readFile(path: string): Promise<Uint8Array> {
    return fromBase64((await this.run(['base64', path], {check: true})).stdout);
  }

  async writeFile(path: string, data: Uint8Array | string, mode = 0o644): Promise<void> {
    if (!path.startsWith('/') || mode < 0 || mode > 0o777) throw new Error('writeFile needs an absolute path and a mode <= 0777');
    const enc = toBase64(typeof data === 'string' ? new TextEncoder().encode(data) : data);
    await this.run(['/bin/sh', '-c', ': >"$1"', 'sh', path], {check: true});
    for (let i = 0; i < enc.length; i += WRITE_CHUNK) {
      await this.run(['/bin/sh', '-c', 'printf %s "$1" | base64 -d >>"$2"', 'sh', enc.slice(i, i + WRITE_CHUNK), path], {check: true});
    }
    await this.run(['chmod', mode.toString(8), path], {check: true});
  }

  async pause(timeoutMs = 60000): Promise<void> {
    await this.kube.machines.power(this.name, 'Paused');
    await this.kube.machines.wait(this.name, {phase: 'Paused', ready: false, timeoutMs});
  }

  async resume(timeoutMs = 120000): Promise<void> {
    await this.kube.machines.power(this.name, 'Running');
    await this.kube.machines.wait(this.name, {timeoutMs});
  }

  /** A live copy of this sandbox (memory, CPU state and disk), Running in milliseconds. */
  async fork(name = `${this.name}-fork-${suffix(4)}`, ttlSeconds = 900, timeoutMs = 120000): Promise<Sandbox> {
    const obj = await this.kube.machines.fork(this.name, name, ttlSeconds);
    const child = new Sandbox(this.kube, this.ui, name, obj.metadata.uid ?? '');
    await this.kube.machines.patch(name, {metadata: {labels: {[SANDBOX_LABEL]: 'true'}}});
    await this.kube.machines.wait(name, {uid: child.uid, timeoutMs});
    return child;
  }

  /** Delete the Machine, or release the claim (which deletes it per its reclaimPolicy). */
  async close(): Promise<void> {
    if (this.claim) await this.kube.claims.delete(this.claim, this.claimUid);
    else if (this.uid) await this.kube.machines.delete(this.name, this.uid);
    this.uid = this.claim = '';
  }
}
