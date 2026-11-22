// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { loginOrToken, token } from './api';

function stubStorage() {
  const m = new Map<string, string>();
  vi.stubGlobal('sessionStorage', {
    getItem: (k: string) => m.get(k) ?? null,
    setItem: (k: string, v: string) => void m.set(k, v),
    removeItem: (k: string) => void m.delete(k),
  });
}

function respond(status: number, body: unknown = {}) {
  return Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
}

describe('loginOrToken', () => {
  beforeEach(stubStorage);
  afterEach(() => vi.unstubAllGlobals());

  it('signs in with username and password when login is enabled', async () => {
    const fetchMock = vi.fn((url: string) => (url === '/api/v1/auth/login' ? respond(200, { token: 'sess', username: 'admin', isAdmin: true }) : respond(500)));
    vi.stubGlobal('fetch', fetchMock);
    expect(await loginOrToken('admin', 'Admin@321', { loginEnabled: true, tokenEnabled: true })).toBe('password');
    expect(token()).toBe('sess');
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('uses the password as the legacy token when username/password login is off', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      const auth = (init?.headers as Record<string, string> | undefined)?.Authorization;
      return url === '/api/v1/overview' && auth === 'Bearer s3cret' ? respond(200) : respond(401);
    });
    vi.stubGlobal('fetch', fetchMock);
    expect(await loginOrToken('admin', 's3cret', { loginEnabled: false, tokenEnabled: true })).toBe('token');
    expect(token()).toBe('s3cret');
  });

  it('falls back to the token when the password is refused but tokens are accepted', async () => {
    const fetchMock = vi.fn((url: string) => (url === '/api/v1/auth/login' ? respond(401, { error: 'bad' }) : respond(200)));
    vi.stubGlobal('fetch', fetchMock);
    expect(await loginOrToken('admin', 'tok', { loginEnabled: true, tokenEnabled: true })).toBe('token');
  });

  it('keeps nothing and rejects a wrong password and a wrong token', async () => {
    vi.stubGlobal('fetch', vi.fn(() => respond(401, { error: 'bad' })));
    await expect(loginOrToken('admin', 'nope', { loginEnabled: true, tokenEnabled: true })).rejects.toBeTruthy();
    await expect(loginOrToken('admin', 'nope', { loginEnabled: false, tokenEnabled: true })).rejects.toBeTruthy();
    expect(token()).toBe('');
  });

  it('does not probe tokens when the server does not accept them', async () => {
    const fetchMock = vi.fn(() => respond(401, { error: 'bad' }));
    vi.stubGlobal('fetch', fetchMock);
    await expect(loginOrToken('admin', 'x', { loginEnabled: true, tokenEnabled: false })).rejects.toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
