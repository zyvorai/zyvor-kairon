// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
import {afterEach, beforeEach, test} from 'node:test';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {APIError, API_VERSION, Client, UIClient} from '../dist/index.js';

const manifest = {apiVersion: API_VERSION, kind: 'Machine', metadata: {name: 'test'}, spec: {resources: {cpu: '1', memory: '1Gi'}}};
let server, url, calls, object, responseQueue, fail, redirect, client;
beforeEach(async () => {
  calls = []; object = undefined; responseQueue = []; fail = undefined; redirect = false;
  server = createServer(async (req, res) => {
    let data = '';
    for await (const chunk of req) data += chunk;
    const body = data ? JSON.parse(data) : undefined;
    calls.push({method: req.method, path: req.url, headers: req.headers, body});
    const send = (status, obj) => {res.writeHead(status, {'content-type': 'application/json'}); res.end(JSON.stringify(obj));};
    if (!['Bearer kube-secret', 'Bearer ui-secret'].includes(req.headers.authorization)) return send(401, {});
    if (redirect) {res.writeHead(302, {location: '/steal'}); res.end(); return;}
    if (fail) return send(fail, {error: 'private response secret'});
    if (req.url.startsWith('/api/v1')) {
      if (req.url.includes('/usage.csv')) {res.end('namespace,ledger\ndefault,test\n'); return;}
      return send(200, {exitCode: 0, stdout: 'ok', stderr: ''});
    }
    if (req.method === 'POST') {
      object = structuredClone(body);
      object.metadata.uid = 'uid-a'; object.metadata.generation = 1;
      object.status = {phase: 'Running', observedGeneration: 1, conditions: [{type: 'Ready', status: 'True'}]};
      return send(201, object);
    }
    if (req.method === 'DELETE') {
      if (object && body.preconditions.uid !== object.metadata.uid) return send(409, {});
      object = undefined; return send(200, {kind: 'Status', status: 'Success'});
    }
    if (req.method === 'PATCH') {Object.assign(object.spec, body.spec); return send(200, object);}
    if (responseQueue.length) return send(200, responseQueue.shift());
    if (req.url.split('?')[0].endsWith('/machines')) return send(200, {items: object ? [object] : []});
    return send(object ? 200 : 404, object ?? {});
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  url = `http://127.0.0.1:${server.address().port}`;
  client = new Client(url, 'kube-secret');
});
afterEach(async () => {server.closeAllConnections(); await new Promise(resolve => server.close(resolve));});

test('creates with caller RBAC and never mutates input', async () => {
  const before = structuredClone(manifest);
  const created = await client.machines.create(manifest);
  assert.deepEqual(manifest, before);
  assert.equal(calls[0].path, '/apis/kairon.zyvor.dev/v1/namespaces/default/machines');
  assert.equal(calls[0].headers.authorization, 'Bearer kube-secret');
  assert.equal(created.metadata.uid, 'uid-a');
});
test('wait checks generation and Ready condition', async () => {
  const created = await client.machines.create(manifest);
  const stale = structuredClone(created); stale.metadata.generation = 2;
  const current = structuredClone(stale); current.status.observedGeneration = 2;
  responseQueue.push(stale, current);
  assert.equal((await client.machines.wait('test', {intervalMs: 1})).metadata.generation, 2);
  assert.equal(calls.filter(c => c.method === 'GET').length, 2);
});
test('failed callback cleans up with original UID', async () => {
  await assert.rejects(client.withMachine(manifest, async () => {throw new Error('application failed');}), /application failed/);
  assert.equal(object, undefined);
  assert.deepEqual(calls.at(-1).body.preconditions, {uid: 'uid-a'});
});
test('readiness failure also cleans up', async () => {
  responseQueue.push({metadata: {uid: 'uid-a'}, status: {phase: 'NeedsRecovery'}});
  await assert.rejects(client.withMachine(manifest, async () => assert.fail('callback must not run')), /recovery/);
  assert.equal(object, undefined);
});
test('replacement object survives UID cleanup', async () => {
  await client.machines.create(manifest); object.metadata.uid = 'replacement';
  await assert.rejects(client.machines.delete('test', 'uid-a'), e => e instanceof APIError && e.status === 409);
  assert.equal(object.metadata.uid, 'replacement');
});
test('wait rejects identity changes and honors cancellation', async () => {
  await client.machines.create(manifest);
  await assert.rejects(client.machines.wait('test', {uid: 'other'}), /identity changed/);
  const controller = new AbortController(); controller.abort(new Error('cancelled'));
  await assert.rejects(client.machines.wait('test', {signal: controller.signal}), /cancelled/);
});
test('deadline ends pending wait', async () => {
  await client.machines.create(manifest); object.status.phase = 'Pending';
  await assert.rejects(client.machines.wait('test', {timeoutMs: 50, intervalMs: 10}));
});
test('writes are not retried and errors omit response secrets', async () => {
  fail = 503;
  await assert.rejects(client.machines.create(manifest), e => e.status === 503 && !e.message.includes('secret'));
  assert.equal(calls.length, 1);
});
test('redirects are not followed', async () => {
  redirect = true;
  await assert.rejects(client.machines.list(), e => e.status === 302);
  assert.equal(calls.length, 1);
});
test('validates names, namespaces, credentials and resource allowlist', () => {
  for (const ns of ['../admin', 'a/b', 'a..b', '', 'a'.repeat(64)]) assert.throws(() => new Client(url, 'secret', {namespace: ns}));
  assert.throws(() => new Client('http://remote.example', 'secret'));
  assert.throws(() => new Client('http://127.evil.example', 'secret'));
  assert.ok(!JSON.stringify(client).includes('kube-secret'));
  assert.throws(() => new Client('https://u:p@example.com', 'secret'));
  assert.throws(() => new Client(url, 'secret\r\nheader'));
  assert.throws(() => client.resource('constructor'));
  assert.throws(() => client.machines.create({...manifest, metadata: {name: 'test', uid: 'injected'}}));
});
test('guest exec has exact QGA shape and independent credentials', async () => {
  const ui = new UIClient(url, 'ui-secret');
  const result = await ui.exec('test', ['/usr/bin/python3', '--version'], 120);
  assert.equal(result.exitCode, 0);
  assert.equal(calls.at(-1).path, '/api/v1/machines/default/test/exec');
  assert.equal(calls.at(-1).headers.authorization, 'Bearer ui-secret');
  assert.deepEqual(calls.at(-1).body, {path: '/usr/bin/python3', args: ['--version'], timeoutSeconds: 120});
  assert.match(await ui.usageCSV(), /namespace,ledger/);
});
test('fork rejects attached storage and preserves parent placement', async () => {
  await client.machines.create(manifest); object.spec.nodeName = 'worker-1'; object.spec.volumes = [{name: 'data'}];
  await assert.rejects(client.machines.fork('test', 'child'), /boot disk/);
  delete object.spec.volumes;
  const child = await client.machines.fork('test', 'child');
  assert.equal(child.metadata.annotations['kairon.zyvor.dev/fork-from'], 'test');
  assert.equal(child.spec.nodeName, 'worker-1');
});
test('cleanup runs even when user wait is aborted', async () => {
  const controller = new AbortController(); controller.abort(new Error('cancelled'));
  await assert.rejects(client.withMachine(manifest, async () => {}, {signal: controller.signal}), /cancelled/);
  assert.equal(object, undefined);
});
test('primary and cleanup failures are both retained', async () => {
  await assert.rejects(client.withMachine(manifest, async () => {
    object.metadata.uid = 'replacement'; throw new Error('original');
  }), e => e instanceof AggregateError && e.errors[0].message === 'original' && e.errors[1].status === 409);
});
test('adaptive migration is a live migration with policy overrides', async () => {
  await client.migrations.migrate('db', {name: 'move-db', strategy: 'adaptive', adaptive: {compression: {algorithm: 'zstd', level: 3}}});
  const call = calls.at(-1);
  assert.equal(call.path, `/apis/${API_VERSION}/namespaces/default/machinemigrations`);
  assert.equal(call.body.kind, 'MachineMigration');
  assert.equal(call.body.spec.strategy, 'live');
  assert.equal(call.body.spec.multifdChannels, 2);
  assert.equal(call.body.spec.adaptive.compression.algorithm, 'zstd');
  assert.throws(() => client.migrations.migrate('db', {strategy: 'cold', adaptive: {}}), /adaptive settings/);
  assert.throws(() => client.migrations.migrate('db', {bandwidthMbps: 1.5}), /positive integer/);
  object.status = {phase: 'Running', ramTotal: 4, ramRemaining: 1, progress: {throughputMbps: 900}, adaptive: {stage: 'AutoConverge'}};
  const progress = await client.migrations.progress('move-db');
  assert.equal(progress.phase, 'Running');
  assert.equal(progress.ramRemaining, 1);
  assert.equal(progress.progress.throughputMbps, 900);
  assert.equal(progress.adaptive.stage, 'AutoConverge');
});
test('incremental backup inherits its parent target; chain groups S3 backups', async () => {
  const target = {s3: {endpoint: 'http://minio:9000', bucket: 'vm', credentialsSecretRef: 's3'}};
  responseQueue.push({metadata: {name: 'f1'}, spec: {machineName: 'web', target, encryption: {keySecretRef: 'k'}}});
  await client.backups.create('web', {parent: 'f1', name: 'i1'});
  const call = calls.at(-1);
  assert.equal(call.path, `/apis/${API_VERSION}/namespaces/default/machinebackups`);
  assert.equal(call.body.spec.mode, 'incremental');
  assert.deepEqual(call.body.spec.target, target);
  assert.equal(call.body.spec.encryption.keySecretRef, 'k');
  await assert.rejects(client.backups.create('web', {mode: 'incremental'}), /S3 target/);
  const point = (name, mode, sequence, parent = '', at = '2026-01-01T00:00:00Z') =>
    ({metadata: {name}, spec: {machineName: 'web', target, mode}, status: {phase: 'Succeeded', pointInTime: at, storedBytes: 10, chain: {id: 'c1', sequence, parent}}});
  responseQueue.push({items: [point('i1', 'incremental', 2, 'f1', '2026-01-01T01:00:00Z'), point('f1', 'full', 1), {metadata: {name: 'local'}, spec: {machineName: 'web'}}]});
  const chains = await client.backups.chain('web');
  assert.equal(chains.length, 1);
  assert.deepEqual(chains[0].points.map(p => p.name), ['f1', 'i1']);
  assert.equal(chains[0].points[1].depth, 1);
  assert.equal(chains[0].storedBytes, 20);
});
