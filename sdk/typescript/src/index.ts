// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
/** Kairon clients. No runtime dependencies, write retries, or credential sharing. */
export type Json = null | boolean | number | string | Json[] | { [key: string]: Json };
export interface Metadata {
  name: string; namespace?: string; uid?: string; generation?: number;
  labels?: Record<string, string>; annotations?: Record<string, string>;
  [key: string]: unknown;
}
export interface KaironObject {
  apiVersion: string; kind: string; metadata: Metadata; spec: Record<string, Json>;
  status?: { phase?: string; nodeName?: string; observedGeneration?: number;
    conditions?: {type: string; status: string}[]; [key: string]: unknown };
}
export interface Options {
  namespace?: string; timeoutMs?: number; allowInsecure?: boolean;
  /** Supply a fetch implementation with your cluster CA for private TLS. Never disable verification. */
  fetch?: typeof fetch;
}
export interface WaitOptions {
  timeoutMs?: number; intervalMs?: number; phase?: string; ready?: boolean;
  uid?: string; signal?: AbortSignal;
}
export class APIError extends Error {
  readonly status: number;
  constructor(status: number, method: string) {
    super(`Kairon ${method} failed (HTTP ${status})`);
    this.status = status;
  }
}
/**
 * HTTP 428 from an AgentSession verb: the command matched a policy.process.ask rule. A human
 * admin approves approvalId (`kaironctl session approve`); then repeat the call with approvalId.
 */
export class ApprovalRequired extends APIError {
  readonly approvalId: string;
  readonly kind: string;
  readonly summary: string;
  readonly matched: string;
  readonly expiresAt: string;
  constructor(method: string, doc: Record<string, unknown>) {
    super(428, method);
    this.approvalId = String(doc.approval_id ?? '');
    this.kind = String(doc.kind ?? '');
    this.summary = String(doc.summary ?? '');
    this.matched = String(doc.matched ?? '');
    this.expiresAt = String(doc.expires_at ?? '');
    this.message = `command needs an admin's approval (rule ${JSON.stringify(this.matched)}): approval ${this.approvalId}`;
  }
}
export const API_VERSION = 'kairon.zyvor.dev/v1';
export const FLEET_VERSION = 'fleet.kairon.zyvor.dev/v1alpha1';
const KINDS: Record<string, string> = {
  machines: 'Machine', machinepools: 'MachinePool', machineclaims: 'MachineClaim',
  machinesnapshots: 'MachineSnapshot', machinesnapshotrestores: 'MachineSnapshotRestore',
  machinemigrations: 'MachineMigration', machinequotas: 'MachineQuota',
  machinesets: 'MachineSet', machinenetworkpolicies: 'MachineNetworkPolicy',
  machinebackups: 'MachineBackup', machinebackuprestores: 'MachineBackupRestore',
  machinedisruptionbudgets: 'MachineDisruptionBudget', machineinstancetypes: 'MachineInstanceType',
  machinesnapshotschedules: 'MachineSnapshotSchedule', migrationpolicies: 'MigrationPolicy',
  networksecuritygroups: 'NetworkSecurityGroup', machinebackuppolicies: 'MachineBackupPolicy',
  machineexports: 'MachineExport', machinepreferences: 'MachinePreference',
  machineprotectionplans: 'MachineProtectionPlan', macnetworks: 'MacNetwork',
  virtualnetworks: 'VirtualNetwork',
};
const FLEET_KINDS: Record<string, string> = {
  machinehaprofiles: 'MachineHAProfile', nodefencerequests: 'NodeFenceRequest',
  machineactionapprovals: 'MachineActionApproval', machinebalancepolicies: 'MachineBalancePolicy',
  machineautoscalers: 'MachineAutoscaler', machinerecoveryplans: 'MachineRecoveryPlan',
  machinebackupgroups: 'MachineBackupGroup', machineimportplans: 'MachineImportPlan',
  machinetemplateversions: 'MachineTemplateVersion', machinetemplateclaims: 'MachineTemplateClaim',
  machinevirtualnetworks: 'MachineVirtualNetwork', machinenetworkclaims: 'MachineNetworkClaim',
  machineusageledgers: 'MachineUsageLedger',
};
function segment(value: string): string {
  if (typeof value !== 'string' || value.length > 253 || !/^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/.test(value) ||
      value.split('.').some(p => !p || p.length > 63 || p.startsWith('-') || p.endsWith('-'))) {
    throw new Error('expected a Kubernetes DNS name');
  }
  return value;
}
function positive(value: number): number {
  if (!Number.isFinite(value) || value <= 0) throw new Error('timeout/interval must be finite and positive');
  return value;
}
function positiveInt(value: number, label: string): number {
  if (!Number.isInteger(value) || value <= 0) throw new Error(`${label} must be a positive integer`);
  return value;
}
function namespace(value: string): string {
  segment(value);
  if (value.length > 63 || value.includes('.')) throw new Error('namespace must be a DNS label');
  return value;
}
class HTTP {
  readonly base: string;
  readonly timeout: number;
  readonly #token: string;
  private readonly fetcher: typeof fetch;
  constructor(url: string, token: string, options: Options) {
    const parsed = new URL(url);
    if (!['http:', 'https:'].includes(parsed.protocol) || parsed.username || parsed.password || parsed.search || parsed.hash) {
      throw new Error('URL must be HTTP(S) without credentials/query/fragment');
    }
    const local = parsed.hostname === 'localhost' || parsed.hostname === '[::1]' || /^127(?:\.\d{1,3}){3}$/.test(parsed.hostname);
    if (parsed.protocol === 'http:' && !local && !options.allowInsecure) throw new Error('remote HTTP requires allowInsecure; use HTTPS');
    if (!token || /[\r\n]/.test(token)) throw new Error('a non-empty bearer token is required');
    this.base = parsed.toString().replace(/\/$/, '');
    this.timeout = positive(options.timeoutMs ?? 30000);
    this.#token = token;
    this.fetcher = options.fetch ?? globalThis.fetch;
  }
  async request<T>(method: string, path: string, body?: unknown,
                   options: {timeoutMs?: number; signal?: AbortSignal; contentType?: string; raw?: boolean; binary?: boolean; data?: Uint8Array} = {}): Promise<T> {
    const timeout = AbortSignal.timeout(Math.max(1, Math.ceil(positive(options.timeoutMs ?? this.timeout))));
    const signal = options.signal ? AbortSignal.any([timeout, options.signal]) : timeout;
    const payload = options.data ? {body: new Uint8Array(options.data)} : body === undefined ? {} : {body: JSON.stringify(body)};
    const response = await this.fetcher(this.base + path, {
      method, signal, redirect: 'manual', headers: {
        Authorization: `Bearer ${this.#token}`, Accept: 'application/json',
        'Content-Type': options.contentType ?? (options.data ? 'application/octet-stream' : 'application/json'),
      }, ...payload,
    });
    if (response.status === 428) {
      let doc: unknown = {};
      try { doc = JSON.parse((await response.text()).slice(0, 64 * 1024)); } catch { doc = {}; }
      if (doc && typeof doc === 'object' && (doc as Record<string, unknown>).approval_id) throw new ApprovalRequired(method, doc as Record<string, unknown>);
      throw new APIError(response.status, method);
    }
    if (!response.ok) {
      await response.body?.cancel();
      throw new APIError(response.status, method);
    }
    // Stream with a hard bound rather than buffering an unbounded response.
    const reader = response.body?.getReader();
    let size = 0;
    const chunks: Uint8Array[] = [];
    if (reader) {
      try {
        while (true) {
          const next = await reader.read();
          if (next.done) break;
          size += next.value.byteLength;
          if (size > 8 * 1024 * 1024) {
            await reader.cancel();
            throw new Error('Kairon response exceeds 8 MiB');
          }
          chunks.push(next.value);
        }
      } finally { reader.releaseLock(); }
    }
    const bytes = new Uint8Array(size);
    let offset = 0;
    for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
    if (options.binary) return bytes as T;
    const text = new TextDecoder().decode(bytes);
    return (options.raw ? text : text ? JSON.parse(text) : undefined) as T;
  }
}
export class Resource {
  protected readonly http: HTTP;
  readonly namespace: string;
  readonly path: string;
  readonly kind: string;
  readonly version: string;
  constructor(http: HTTP, ns: string, resource: string, kind: string, version: string) {
    this.http = http; this.namespace = namespace(ns); this.kind = kind; this.version = version;
    this.path = `/apis/${version}/namespaces/${ns}/${resource}`;
  }
  async list(labelSelector = ''): Promise<KaironObject[]> {
    const query = labelSelector ? `?${new URLSearchParams({labelSelector})}` : '';
    const out = await this.http.request<{items?: KaironObject[]}>('GET', this.path + query);
    return out.items ?? [];
  }
  get(name: string, options: {timeoutMs?: number; signal?: AbortSignal} = {}): Promise<KaironObject> {
    return this.http.request('GET', `${this.path}/${segment(name)}`, undefined, options);
  }
  create(manifest: KaironObject): Promise<KaironObject> {
    const obj = structuredClone(manifest);
    if (obj.apiVersion !== this.version || obj.kind !== this.kind) throw new Error('manifest kind/apiVersion mismatch');
    segment(obj.metadata.name);
    if ((obj.metadata.namespace ?? this.namespace) !== this.namespace) throw new Error('manifest namespace mismatch');
    if ('status' in obj || ['uid', 'resourceVersion', 'deletionTimestamp', 'ownerReferences'].some(k => k in obj.metadata)) {
      throw new Error('create manifest must omit server-managed status/metadata');
    }
    obj.metadata.namespace = this.namespace;
    return this.http.request('POST', this.path, obj);
  }
  patch(name: string, patch: Record<string, unknown>): Promise<KaironObject> {
    return this.http.request('PATCH', `${this.path}/${segment(name)}`, patch, {contentType: 'application/merge-patch+json'});
  }
  async delete(name: string, uid: string): Promise<void> {
    if (!uid) throw new Error("deletion requires the object's UID");
    try {
      await this.http.request('DELETE', `${this.path}/${segment(name)}`, {
        apiVersion: 'v1', kind: 'DeleteOptions', preconditions: {uid},
      });
    } catch (error) { if (!(error instanceof APIError && error.status === 404)) throw error; }
  }
}
function delay(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) { reject(signal.reason); return; }
    const finish = () => { signal?.removeEventListener('abort', abort); resolve(); };
    const timer = setTimeout(finish, ms);
    const abort = () => { clearTimeout(timer); signal?.removeEventListener('abort', abort); reject(signal?.reason); };
    signal?.addEventListener('abort', abort, {once: true});
  });
}
export class Machines extends Resource {
  power(name: string, state: 'Running' | 'Stopped' | 'Paused' | 'Halted'): Promise<KaironObject> {
    if (!['Running', 'Stopped', 'Paused', 'Halted'].includes(state)) throw new Error('unsupported power state');
    return this.patch(name, {spec: {powerState: state}});
  }
  async wait(name: string, options: WaitOptions = {}): Promise<KaironObject> {
    const timeout = positive(options.timeoutMs ?? 120000), interval = positive(options.intervalMs ?? 1000);
    const deadline = performance.now() + timeout, phase = options.phase ?? 'Running';
    while (true) {
      options.signal?.throwIfAborted();
      const remaining = deadline - performance.now();
      if (remaining <= 0) throw new Error(`Machine did not reach ${phase} before the deadline`);
      const obj = await this.get(name, {timeoutMs: Math.min(remaining, this.http.timeout), ...(options.signal ? {signal: options.signal} : {})});
      if (options.uid !== undefined && obj.metadata.uid !== options.uid) throw new Error('Machine identity changed while waiting');
      const status = obj.status ?? {};
      if (['Failed', 'NeedsRecovery'].includes(status.phase ?? '')) throw new Error('Machine requires operator recovery');
      const observed = (status.observedGeneration ?? 0) >= (obj.metadata.generation ?? 0);
      const ready = status.conditions?.some(c => c.type === 'Ready' && c.status === 'True');
      if (observed && status.phase === phase && (options.ready === false || ready)) return obj;
      await delay(Math.min(interval, Math.max(0, deadline - performance.now())), options.signal);
    }
  }
  async fork(parent: string, name: string, ttlSeconds = 900): Promise<KaironObject> {
    segment(name);
    if (!Number.isInteger(ttlSeconds) || ttlSeconds <= 0) throw new Error('ttlSeconds must be a positive integer');
    if (`kairon-${this.namespace}-${name}-1`.length > 63) throw new Error('fork runtime name exceeds 63 characters');
    const source = await this.get(parent);
    if (source.status?.phase !== 'Running' || !source.spec.nodeName) throw new Error('fork parent must be Running on a node');
    for (const key of ['volumes', 'disks', 'deviceClaims']) {
      if (Array.isArray(source.spec[key]) && source.spec[key].length) throw new Error('fork copies only the boot disk; volumes/disks/devices are unsupported');
    }
    return this.create({apiVersion: API_VERSION, kind: 'Machine', metadata: {
      name, namespace: this.namespace, annotations: {'kairon.zyvor.dev/fork-from': parent},
      labels: {'kairon.zyvor.dev/forked-from': parent, 'kairon.zyvor.dev/assigned-node': String(source.spec.nodeName)},
    }, spec: {...structuredClone(source.spec), powerState: 'Running', ttlSeconds}});
  }
}
export interface AdaptiveMigrationSettings {
  completionTimeoutPerGiB?: string;
  progressTimeout?: string;
  autoConverge?: {enabled?: boolean; dirtyRateThresholdMbps?: number};
  postCopy?: {enabled?: boolean; triggerAfter?: string};
  compression?: {algorithm: 'none' | 'zlib' | 'zstd'; level?: number};
  bandwidth?: {maxMbps?: number; adaptive?: boolean};
}
export interface MigrateOptions {
  name?: string;
  // 'adaptive' is a live migration steered by kairon-node's adaptive engine (experimental).
  strategy?: 'auto' | 'live' | 'cold' | 'adaptive';
  targetNode?: string;
  mode?: 'pre-copy' | 'post-copy';
  bandwidthMbps?: number;
  maxDowntimeMs?: number;
  multifdChannels?: number;
  // Overrides the matching MigrationPolicy's adaptive settings, field by field.
  adaptive?: AdaptiveMigrationSettings;
}
export interface MigrationProgress {
  phase: string;
  ramTotal: number;
  ramTransferred: number;
  ramRemaining: number;
  downtimeMs: number;
  progress?: Record<string, Json>;
  adaptive?: Record<string, Json>;
}
export class Migrations extends Resource {
  migrate(machine: string, options: MigrateOptions = {}): Promise<KaironObject> {
    segment(machine);
    let strategy = options.strategy ?? 'auto';
    let adaptive = options.adaptive;
    if (strategy === 'adaptive') { strategy = 'live'; adaptive ??= {}; }
    if (adaptive && strategy !== 'live') throw new Error("adaptive settings need strategy 'live' or 'adaptive'");
    let multifd = options.multifdChannels;
    if (adaptive?.compression && adaptive.compression.algorithm !== 'none' && !multifd) multifd = 2;
    const spec: Record<string, Json> = {machineName: machine, strategy};
    if (options.targetNode) spec.targetNode = options.targetNode;
    if (options.mode) spec.mode = options.mode;
    if (options.bandwidthMbps) spec.bandwidthMbps = positiveInt(options.bandwidthMbps, 'bandwidthMbps');
    if (options.maxDowntimeMs) spec.maxDowntimeMs = positiveInt(options.maxDowntimeMs, 'maxDowntimeMs');
    if (multifd) spec.multifdChannels = positiveInt(multifd, 'multifdChannels');
    if (adaptive) spec.adaptive = structuredClone(adaptive) as Json;
    const name = options.name ?? `${machine}-migrate-${Date.now().toString(36)}`;
    return this.create({apiVersion: API_VERSION, kind: 'MachineMigration', metadata: {name, namespace: this.namespace}, spec});
  }
  async progress(name: string): Promise<MigrationProgress> {
    const status = (await this.get(name)).status ?? {};
    return {
      phase: String(status.phase ?? 'Pending'),
      ramTotal: Number(status.ramTotal ?? 0), ramTransferred: Number(status.ramTransferred ?? 0),
      ramRemaining: Number(status.ramRemaining ?? 0), downtimeMs: Number(status.downtimeMs ?? 0),
      ...(status.progress ? {progress: status.progress as Record<string, Json>} : {}),
      ...(status.adaptive ? {adaptive: status.adaptive as Record<string, Json>} : {}),
    };
  }
}
export type BackupMode = 'full' | 'incremental' | 'synthetic-full';
export interface BackupOptions {
  name?: string;
  /** S3 backups: full (default), incremental or synthetic-full. */
  mode?: BackupMode;
  /** The backup an incremental builds on, or the point a synthetic full consolidates. */
  parent?: string;
  /** Take the S3 target from this MachineBackupPolicy. */
  policy?: string;
  /** spec.target, e.g. {s3: {endpoint, bucket, credentialsSecretRef}}. */
  target?: Record<string, Json>;
  quiesce?: 'auto' | 'required' | 'never';
  encryptionKeySecret?: string;
  immutableFor?: string;
}
export interface BackupChainPoint {
  name: string; kind: string; phase: string; sequence: number; depth: number;
  pointInTime?: string; storedBytes: number; changedBytes: number;
}
export interface BackupChain {id: string; storedBytes: number; points: BackupChainPoint[]}
export class Backups extends Resource {
  /**
   * Create from a full manifest, or build one for a Machine. An S3 backup takes
   * its target from options.target, else from options.policy, else (incremental
   * or synthetic-full) from options.parent.
   */
  async create(manifestOrMachine: KaironObject | string, options: BackupOptions = {}): Promise<KaironObject> {
    if (typeof manifestOrMachine !== 'string') return super.create(manifestOrMachine);
    const machine = segment(manifestOrMachine);
    if (options.mode && !['full', 'incremental', 'synthetic-full'].includes(options.mode)) throw new Error('mode must be full, incremental or synthetic-full');
    const spec: Record<string, Json> = {machineName: machine};
    if (options.mode) spec.mode = options.mode;
    if (options.parent) spec.parent = segment(options.parent);
    if (options.quiesce) spec.quiesce = options.quiesce;
    if (options.immutableFor) spec.immutableFor = options.immutableFor;
    if (options.encryptionKeySecret) spec.encryption = {algorithm: 'aes256-gcm', keySecretRef: options.encryptionKeySecret};
    if (options.target) {
      spec.target = structuredClone(options.target);
    } else if (options.policy) {
      const p = (await this.http.request<KaironObject>('GET', `${this.path.replace(/\/machinebackups$/, '')}/machinebackuppolicies/${segment(options.policy)}`)).spec ?? {};
      spec.target = p.target as Json;
      for (const k of ['compression', 'encryption', 'immutableFor', 'quiesce']) if (k in p && !(k in spec)) spec[k] = p[k] as Json;
    } else if (options.parent) {
      const p = (await this.get(options.parent)).spec ?? {};
      if (!p.target) throw new Error('parent is not an S3 backup');
      for (const k of ['target', 'compression', 'encryption', 'immutableFor']) if (k in p) spec[k] = p[k] as Json;
      spec.mode ??= 'incremental';
    } else if (options.mode) {
      throw new Error('mode needs an S3 target: target, policy or parent');
    }
    if (options.policy) spec.policyName = segment(options.policy);
    const name = options.name ?? `${machine}-${new Date().toISOString().replace(/[-:]/g, '').replace('T', '-').slice(0, 15)}`;
    return super.create({apiVersion: API_VERSION, kind: 'MachineBackup', metadata: {name, namespace: this.namespace}, spec});
  }
  /** The Machine's S3 backups grouped by chain, newest chain first. */
  async chain(machine: string): Promise<BackupChain[]> {
    segment(machine);
    const byChain = new Map<string, KaironObject[]>();
    for (const b of await this.list()) {
      const chain = b.status?.chain as Record<string, Json> | undefined;
      if (b.spec?.machineName !== machine || !b.spec?.target || !chain) continue;
      const id = String(chain.id);
      byChain.set(id, [...(byChain.get(id) ?? []), b]);
    }
    const seq = (b: KaironObject) => Number((b.status?.chain as Record<string, Json>).sequence ?? 0);
    const out: BackupChain[] = [];
    for (const [id, items] of byChain) {
      items.sort((a, b) => seq(a) - seq(b) || String(a.metadata.creationTimestamp ?? '').localeCompare(String(b.metadata.creationTimestamp ?? '')));
      const depth = new Map<string, number>();
      const points = items.map((b): BackupChainPoint => {
        const st = b.status ?? {};
        const chain = st.chain as Record<string, Json>;
        const kind = String(b.spec?.mode ?? 'full');
        const d = kind === 'incremental' ? (depth.get(String(chain.parent ?? '')) ?? 0) + 1 : 0;
        depth.set(b.metadata.name, d);
        return {
          name: b.metadata.name, kind, phase: String(st.phase ?? ''), sequence: seq(b), depth: d,
          ...(st.pointInTime ? {pointInTime: String(st.pointInTime)} : {}),
          storedBytes: Number(st.storedBytes ?? 0), changedBytes: Number(st.changedBytes ?? 0),
        };
      });
      out.push({id, storedBytes: points.reduce((n, p) => n + p.storedBytes, 0), points});
    }
    const newest = (c: BackupChain) => c.points.reduce((t, p) => (p.pointInTime && p.pointInTime > t ? p.pointInTime : t), '');
    return out.sort((a, b) => newest(b).localeCompare(newest(a)));
  }
}
export class Client {
  readonly namespace: string;
  readonly machines: Machines;
  readonly migrations: Migrations;
  readonly pools: Resource;
  readonly claims: Resource;
  readonly backups: Backups;
  readonly backupPolicies: Resource;
  private readonly http: HTTP;
  constructor(kubeURL: string, token: string, options: Options = {}) {
    this.namespace = namespace(options.namespace ?? 'default');
    this.http = new HTTP(kubeURL, token, options);
    this.machines = new Machines(this.http, this.namespace, 'machines', 'Machine', API_VERSION);
    this.migrations = new Migrations(this.http, this.namespace, 'machinemigrations', 'MachineMigration', API_VERSION);
    this.pools = this.resource('machinepools'); this.claims = this.resource('machineclaims');
    this.backups = new Backups(this.http, this.namespace, 'machinebackups', 'MachineBackup', API_VERSION);
    this.backupPolicies = this.resource('machinebackuppolicies');
  }
  resource(name: string, fleet = false): Resource {
    const kinds = fleet ? FLEET_KINDS : KINDS;
    if (!Object.hasOwn(kinds, name)) throw new Error('unsupported Kairon resource');
    return new Resource(this.http, this.namespace, name, kinds[name]!, fleet ? FLEET_VERSION : API_VERSION);
  }
  async withMachine<T>(manifest: KaironObject, run: (machine: KaironObject) => Promise<T>, options: WaitOptions = {}): Promise<T> {
    const obj = await this.machines.create(manifest);
    const uid = obj.metadata.uid;
    if (!uid) throw new Error('create response has no UID; inspect the Machine before cleanup');
    let failed = false, primary: unknown;
    try {
      return await run(await this.machines.wait(obj.metadata.name, {...options, uid}));
    } catch (error) { failed = true; primary = error; throw error; }
    finally {
      // Cleanup is independent of an aborted user operation.
      try { await this.machines.delete(obj.metadata.name, uid); }
      catch (cleanup) {
        if (failed) throw new AggregateError([primary, cleanup], 'operation and Kairon cleanup both failed');
        throw cleanup;
      }
    }
  }
}
export interface ExecResult {exitCode: number; stdout: string; stderr: string}
export function toBase64(data: Uint8Array): string {
  let s = '';
  for (let i = 0; i < data.length; i += 0x8000) s += String.fromCharCode(...data.subarray(i, i + 0x8000));
  return btoa(s);
}
export function fromBase64(text: string): Uint8Array {
  const s = atob(text.replace(/\s+/g, ''));
  const out = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
  return out;
}
export class UIClient {
  private readonly http: HTTP;
  readonly namespace: string;
  constructor(url: string, token: string, options: Options = {}) {
    this.namespace = namespace(options.namespace ?? 'default'); this.http = new HTTP(url, token, options);
  }
  private machine(name: string): string { return `/api/v1/machines/${this.namespace}/${segment(name)}`; }
  exec(name: string, argv: string[], timeoutSeconds = 60): Promise<ExecResult> {
    if (!argv.length || !argv[0] || argv.some(a => typeof a !== 'string')) throw new Error('argv needs an executable and string arguments');
    if (!Number.isInteger(timeoutSeconds) || timeoutSeconds < 1 || timeoutSeconds > 300) throw new Error('timeoutSeconds must be 1..300');
    return this.http.request('POST', this.machine(name) + '/exec', {path: argv[0], args: argv.slice(1), timeoutSeconds}, {timeoutMs: (timeoutSeconds + 15) * 1000});
  }
  agentExec(name: string, command: string, timeoutSeconds = 60): Promise<ExecResult> {
    if (!command || typeof command !== 'string') throw new Error('command must be non-empty');
    if (!Number.isInteger(timeoutSeconds) || timeoutSeconds < 1 || timeoutSeconds > 300) throw new Error('timeoutSeconds must be 1..300');
    return this.http.request('POST', this.machine(name) + '/agent-exec', {command, timeoutSeconds}, {timeoutMs: (timeoutSeconds + 15) * 1000});
  }
  /** Write a guest file over the vsock agent (spec.guestAgent.console). */
  async putFile(name: string, path: string, data: Uint8Array, mode = 0o600): Promise<void> {
    if (data.length > 3 * 1024 * 1024 || !path.startsWith('/') || mode < 0 || mode > 0o777) throw new Error('file needs an absolute path, <=3 MiB content and a mode <=0777');
    await this.http.request('POST', this.machine(name) + '/agent-file/put', {path, contentBase64: toBase64(data), mode});
  }
  /** Read a guest file over the vsock agent (spec.guestAgent.console). */
  async getFile(name: string, path: string): Promise<Uint8Array> {
    if (!path.startsWith('/')) throw new Error('file needs an absolute path');
    const out = await this.http.request('POST', this.machine(name) + '/agent-file/get', {path}) as {contentBase64?: string} | null;
    return fromBase64(out?.contentBase64 ?? '');
  }
  capabilities(node: string): Promise<Record<string, Json>> {
    return this.http.request('GET', `/api/v1/nodes/${segment(node)}/capabilities`);
  }
  /** The node's NodeTopology status and the Machines' topology allocations on it. */
  topology(node: string): Promise<Record<string, Json>> {
    return this.http.request('GET', `/api/v1/nodes/${segment(node)}/topology`);
  }
  usageCSV(): Promise<string> {
    return this.http.request('GET', '/api/v1/usage.csv?' + new URLSearchParams({namespace: this.namespace}), undefined, {raw: true});
  }
  private session(name: string): string { return `/api/v1/agentsessions/${this.namespace}/${segment(name)}`; }
  /** Create an AgentSession (a FluxVM agent session) in the namespace. */
  createSession(name: string, spec: Record<string, Json>, labels: Record<string, string> = {}): Promise<KaironObject> {
    return this.http.request('POST', '/api/v1/agentsessions?' + new URLSearchParams({namespace: this.namespace}), {name: segment(name), spec, labels});
  }
  getSession(name: string): Promise<KaironObject> { return this.http.request('GET', this.session(name)); }
  listSessions(): Promise<KaironObject[]> {
    return this.http.request('GET', '/api/v1/agentsessions?' + new URLSearchParams({namespace: this.namespace}));
  }
  async deleteSession(name: string): Promise<void> {
    try { await this.http.request('DELETE', this.session(name)); } catch (error) { if (!(error instanceof APIError && error.status === 404)) throw error; }
  }
  /** One FluxVM session verb (exec, jobs/..., files/..., git/..., preview, ...) through kairon-ui. */
  sessionCall<T = Json>(name: string, method: string, verb: string, body?: unknown,
                        options: {query?: Record<string, string | number | boolean>; timeoutMs?: number; data?: Uint8Array; binary?: boolean} = {}): Promise<T> {
    if (!verb || verb.startsWith('/') || verb.split('/').includes('..')) throw new Error('verb must be a relative session route');
    let path = this.session(name) + '/' + verb.split('/').map(encodeURIComponent).join('/');
    if (options.query) path += '?' + new URLSearchParams(Object.entries(options.query).map(([k, v]) => [k, String(v)]));
    return this.http.request(method, path, body, {
      ...(options.timeoutMs === undefined ? {} : {timeoutMs: options.timeoutMs}),
      ...(options.data === undefined ? {} : {data: options.data}),
      ...(options.binary ? {binary: true} : {}),
    });
  }
}
export {CommandError, Process, Sandbox, SANDBOX_LABEL, type SandboxOptions} from './sandbox.js';
export {AgentSession, SessionError, sessionSpec, type ApprovalOptions, type InputAction, type SessionSpecOptions} from './session.js';
