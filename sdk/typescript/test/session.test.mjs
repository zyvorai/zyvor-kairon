// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
import {test} from 'node:test';
import assert from 'node:assert/strict';
import {APIError, AgentSession, ApprovalRequired, CommandError, SessionError, UIClient, fromBase64, sessionSpec, toBase64} from '../dist/index.js';

function fakeUI() {
  const files = new Map();
  const phase = new Map([['work', 'Running']]);
  return {
    files, phase, calls: [], created: [], deleted: [],
    async createSession(name, spec) { this.created.push([name, spec]); if (!phase.has(name)) phase.set(name, 'Running'); return {}; },
    async getSession(name) { return {status: {phase: phase.get(name) ?? 'Pending', message: 'no capable node'}}; },
    async deleteSession(name) { this.deleted.push(name); },
    async sessionCall(name, method, verb, body, options = {}) {
      this.calls.push([name, method, verb, body, options.query]);
      if (verb === 'exec') return body.command === 'false' ? {exit_code: 1, stdout: '', stderr: 'boom'} : {exit_code: 0, stdout: 'out', stderr: ''};
      if (verb === 'fork') { phase.set('work-a', 'Running'); return {agentSessions: ['work-a']}; }
      if (verb === 'files/content' && method === 'PUT') { files.set(body.path, fromBase64(body.content_base64)); return {}; }
      if (verb === 'files/content') return {content_base64: toBase64(files.get(options.query.path))};
      if (verb === 'jobs') return {job: 'j1'};
      return {};
    },
  };
}

test('sessionSpec builds the AgentSession spec', () => {
  assert.deepEqual(sessionSpec({agent: 'codex', git: 'https://github.com/o/r', preset: 'offline', allowHosts: ['a.dev'], ttlSeconds: 60}), {
    agent: 'codex', workspace: {git: 'https://github.com/o/r'},
    policy: {preset: 'offline', network: {allow: [{host: 'a.dev'}]}}, ttlSeconds: 60,
  });
  assert.deepEqual(sessionSpec(), {});
});

test('create, exec, fork, files and close', async () => {
  const ui = fakeUI();
  const s = await AgentSession.create(ui, {name: 'work', agent: 'claude'});
  assert.equal((await s.exec('make test', {timeoutSeconds: 30})).stdout, 'out');
  assert.deepEqual(ui.calls.at(-1)[3], {command: 'make test', timeout_seconds: 30});
  await assert.rejects(s.exec('false', {check: true}), CommandError);
  assert.deepEqual((await s.fork(1)).map(k => k.name), ['work-a']);
  await s.writeFile('/workspace/a', new TextEncoder().encode('hi'));
  assert.equal(new TextDecoder().decode(await s.readFile('/workspace/a')), 'hi');
  assert.equal(await s.startJob('npm run dev'), 'j1');
  await s.close();
  assert.deepEqual(ui.deleted, ['work']);
});

test('a failed session is deleted and reported', async () => {
  const ui = fakeUI();
  ui.phase.set('bad', 'Failed');
  await assert.rejects(AgentSession.create(ui, {name: 'bad'}), SessionError);
  assert.deepEqual(ui.deleted, ['bad']);
});

test('UIClient session paths', async () => {
  const seen = [];
  const fetch = async (url, init) => { seen.push([init.method, url, init.body]); return new Response('{}', {status: 200}); };
  const ui = new UIClient('http://127.0.0.1:1', 't', {namespace: 'dev', fetch});
  await ui.sessionCall('work', 'GET', 'jobs/j1', undefined, {query: {offset: 5}});
  await ui.createSession('new', {agent: 'a'});
  assert.equal(seen[0][1], 'http://127.0.0.1:1/api/v1/agentsessions/dev/work/jobs/j1?offset=5');
  assert.equal(seen[1][1], 'http://127.0.0.1:1/api/v1/agentsessions?namespace=dev');
  assert.deepEqual(JSON.parse(seen[1][2]), {name: 'new', spec: {agent: 'a'}, labels: {}});
  for (const bad of ['/exec', '../x', 'files/../../etc']) assert.throws(() => ui.sessionCall('work', 'GET', bad));
});

const APPROVAL = '11111111-2222-4333-8444-555555555555';
const UPLOAD = 'aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee';

test('a 428 becomes ApprovalRequired and approvalId is sent on the retry', async () => {
  const bodies = [];
  const fetch = async (url, init) => {
    const body = JSON.parse(init.body);
    bodies.push(body);
    if (!body.approval_id) {
      return new Response(JSON.stringify({approval_required: true, approval_id: APPROVAL, matched: 'git push*', summary: body.command}), {status: 428});
    }
    return new Response('{"exit_code":0,"stdout":"pushed"}', {status: 200});
  };
  const s = new AgentSession(new UIClient('http://127.0.0.1:1', 't', {fetch}), 'work');
  await assert.rejects(s.exec('git push'), e => e instanceof ApprovalRequired && e instanceof APIError && e.approvalId === APPROVAL && e.matched === 'git push*');
  assert.equal((await s.exec('git push', {approvalId: APPROVAL, approvalWaitSeconds: 30})).stdout, 'pushed');
  assert.deepEqual(bodies.at(-1), {command: 'git push', approval_id: APPROVAL, approval_wait_seconds: 30});
  await assert.rejects(s.exec('ls', {approvalId: 'nope'}));
  await assert.rejects(s.exec('ls', {approvalWaitSeconds: 500}));
  assert.deepEqual(sessionSpec({processAsk: ['deploy *']}), {policy: {process: {ask: ['deploy *']}}});
});

test('large writes upload in chunks, resume on 409 and verify the checksum', async () => {
  let stored = new Uint8Array(0);
  let meta;
  let conflict = true;
  let completed = false;
  const ui = {
    async sessionCall(name, method, verb, body, options = {}) {
      if (verb === 'uploads') { meta = body; return {id: UPLOAD}; }
      if (verb === `uploads/${UPLOAD}` && method === 'PUT') {
        if (options.query.offset > 0 && conflict) { conflict = false; throw new APIError(409, 'PUT'); }
        const next = new Uint8Array(stored.length + options.data.length);
        next.set(stored); next.set(options.data, stored.length); stored = next;
        return {received: stored.length};
      }
      if (verb === `uploads/${UPLOAD}` && method === 'GET') return {state: 'open', received: stored.length};
      if (verb === `uploads/${UPLOAD}/complete`) { completed = true; return {state: 'complete'}; }
      throw new Error('unexpected ' + method + ' ' + verb);
    },
  };
  const data = new Uint8Array(9 * 1024 * 1024 + 123).map((_, i) => i % 251);
  await new AgentSession(ui, 'work').writeFile('/workspace/big.bin', data);
  assert.ok(completed && !conflict);
  assert.deepEqual(stored, data);
  const digest = Buffer.from(await crypto.subtle.digest('SHA-256', data)).toString('hex');
  assert.equal(meta.sha256, digest);
  assert.equal(meta.size, data.length);
});
