// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {CommandError, Sandbox, fromBase64, toBase64} from '../dist/index.js';

function fakes() {
  const files = new Map();
  const calls = [];
  const machines = {
    created: [], patches: [], deleted: [], powered: [],
    async create(m) { this.created.push(m); return {metadata: {name: m.metadata.name, uid: 'uid-1'}}; },
    async wait() { return {}; },
    async power(_n, s) { this.powered.push(s); },
    async patch(n, p) { this.patches.push([n, p]); },
    async fork(_p, name) { return {metadata: {name, uid: 'uid-fork'}}; },
    async delete(n, uid) { this.deleted.push([n, uid]); },
  };
  const ui = {
    async exec(_name, argv) {
      calls.push(argv);
      if (argv[0] === 'base64') return {exitCode: 0, stdout: toBase64(files.get(argv[1])) + '\n', stderr: ''};
      if (argv[2] === ': >"$1"') files.set(argv[4], new Uint8Array());
      else if (argv[2]?.includes('base64 -d')) {
        const prev = files.get(argv[5]), add = fromBase64(argv[4]);
        const next = new Uint8Array(prev.length + add.length); next.set(prev); next.set(add, prev.length);
        files.set(argv[5], next);
      } else if (argv[2]?.includes('nohup')) return {exitCode: 0, stdout: '4242\n', stderr: ''};
      else if (argv[0] === 'tail') return {exitCode: 0, stdout: 'hello\n', stderr: ''};
      else if (argv[2] === 'false') return {exitCode: 1, stdout: '', stderr: 'boom'};
      return {exitCode: 0, stdout: '', stderr: ''};
    },
  };
  return {kube: {machines}, ui, files, calls};
}

test('create labels the Machine and sets lifecycle', async () => {
  const {kube, ui} = fakes();
  const sb = await Sandbox.create(kube, ui, {image: '/img.qcow2', name: 'sb1', idleTimeoutSeconds: 600, idleAction: 'Hibernate'});
  const m = kube.machines.created[0];
  assert.equal(m.metadata.labels['kairon.zyvor.dev/sandbox'], 'true');
  assert.deepEqual(m.spec.lifecycle, {idleTimeoutSeconds: 600, wakeOnConnect: true, idleAction: 'Hibernate'});
  await sb.close();
  assert.deepEqual(kube.machines.deleted, [['sb1', 'uid-1']]);
});

test('run, files, processes', async () => {
  const {kube, ui, calls} = fakes();
  const sb = new Sandbox(kube, ui, 'sb1');
  await assert.rejects(sb.run('false', {check: true}), CommandError);
  const data = new Uint8Array(150000).map((_, i) => i % 251);
  await sb.writeFile('/tmp/x', data, 0o600);
  assert.deepEqual(await sb.readFile('/tmp/x'), data);
  assert.ok(calls.some(c => c.join(' ') === 'chmod 600 /tmp/x'));
  const proc = await sb.start('sleep 10');
  assert.equal(proc.pid, 4242);
  assert.equal(await proc.logs(), 'hello\n');
  await proc.logs();
  assert.deepEqual(calls.at(-1).slice(0, 3), ['tail', '-c', '+7']);
});

test('pause, resume, fork', async () => {
  const {kube, ui} = fakes();
  const sb = new Sandbox(kube, ui, 'sb1', 'uid-1');
  await sb.pause(); await sb.resume();
  assert.deepEqual(kube.machines.powered, ['Paused', 'Running']);
  const child = await sb.fork('sb1-child');
  assert.equal(child.name, 'sb1-child');
  assert.equal(kube.machines.patches.at(-1)[1].metadata.labels['kairon.zyvor.dev/sandbox'], 'true');
});
