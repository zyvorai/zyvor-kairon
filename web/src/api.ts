// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { withNamespace } from './lib/namespace';

export const token = () => sessionStorage.getItem('kairon-token') || '';
export const username = () => sessionStorage.getItem('kairon-username') || '';
export const isAdmin = () => sessionStorage.getItem('kairon-is-admin') === 'true';

// True while this session signed in with the well-known lab password.
export const usingDefaultPassword = () => sessionStorage.getItem('kairon-default-pw') === 'true';
export const SESSION_CHANGED_EVENT = 'kairon:session-changed';
function setDefaultPassword(on: boolean) {
  if (on) sessionStorage.setItem('kairon-default-pw', 'true');
  else sessionStorage.removeItem('kairon-default-pw');
  window.dispatchEvent(new Event(SESSION_CHANGED_EVENT));
}

export function setSession(tok: string, user: string, admin = false) {
  if (!tok) sessionStorage.removeItem('kairon-default-pw');
  if (tok) sessionStorage.setItem('kairon-token', tok);
  else sessionStorage.removeItem('kairon-token');
  if (user) sessionStorage.setItem('kairon-username', user);
  else sessionStorage.removeItem('kairon-username');
  if (user && admin) sessionStorage.setItem('kairon-is-admin', 'true');
  else sessionStorage.removeItem('kairon-is-admin');
}

// setToken keeps the legacy raw-token entry point (still used by the
// fallback token box when username/password login isn't configured on the
// server) -- it never carries a username.
export function setToken(v: string) {
  setSession(v, '');
}

function headers(extra: Record<string, string> = {}): Record<string, string> {
  const h: Record<string, string> = { ...extra };
  if (token()) h.Authorization = `Bearer ${token()}`;
  return h;
}

// UNAUTHORIZED_EVENT fires whenever any API call gets a 401, so App.tsx can
// clear the stale session and bounce back to the login screen in one place,
// rather than every caller having to check for it individually.
export const UNAUTHORIZED_EVENT = 'kairon:unauthorized';

export async function api<T = unknown>(path: string, init: RequestInit = {}): Promise<T> {
  const r = await fetch(withNamespace(path, undefined, { method: init.method }), { ...init, headers: headers((init.headers as Record<string, string>) || {}) });
  if (r.status === 401) {
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
  }
  if (!r.ok) throw new Error((await r.text()) || r.statusText);
  if (r.status === 204) return undefined as T;
  return r.json();
}

export function apiJSON<T = unknown>(path: string, method: string, body: unknown): Promise<T> {
  return api<T>(path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
}

export interface AuthConfig {
  loginEnabled: boolean;
  tokenEnabled: boolean;
  ssoEnabled?: boolean;
  ssoLoginURL?: string;
  loginHint?: string;
  stats?: { version: string; apiRoutes: number; crdKinds: number; hypervisors: number };
}

// getAuthConfig is unauthenticated by design (see internal/uiapi/auth.go),
// so the login screen can decide which form to render before any
// credential exists yet.
export async function getAuthConfig(): Promise<AuthConfig> {
  const r = await fetch('/api/v1/auth/config');
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

export async function login(usernameInput: string, password: string): Promise<void> {
  const r = await fetch('/api/v1/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: usernameInput, password }),
  });
  if (!r.ok) throw new Error((await r.text()) || r.statusText);
  const out = await r.json();
  setSession(out.token, out.username, out.isAdmin);
  if (out.defaultPassword) setDefaultPassword(true);
}

// loginOrToken signs in with a username and password. Servers that still use
// the legacy shared bearer token accept it as the password, so one form
// serves both: when username/password sign-in is off (loginEnabled false) or
// is refused, and the server accepts tokens, the password is tried as the
// token. The token is only kept once an authenticated request succeeds.
export async function loginOrToken(user: string, password: string, cfg: { loginEnabled: boolean; tokenEnabled: boolean }): Promise<'password' | 'token'> {
  let loginError: unknown = null;
  if (cfg.loginEnabled) {
    try {
      await login(user, password);
      return 'password';
    } catch (e) {
      loginError = e;
    }
  }
  if (cfg.tokenEnabled) {
    const r = await fetch('/api/v1/overview', { headers: { Authorization: `Bearer ${password}` } });
    if (r.ok) {
      setToken(password);
      return 'token';
    }
  }
  throw loginError ?? new Error('Wrong username or password.');
}

// parseSSOCallbackFragment is the pure part of completeSSOCallback below,
// split out so it's testable without a DOM (see api.test.ts) -- it never
// touches window/sessionStorage itself.
export function parseSSOCallbackFragment(hash: string): { token: string; username: string } | { error: string } {
  const frag = new URLSearchParams(hash.replace(/^#/, ''));
  const err = frag.get('error');
  if (err) return { error: err };
  const token = frag.get('token');
  const username = frag.get('username');
  if (!token || !username) return { error: 'the identity provider did not return a usable session' };
  return { token, username };
}

// completeSSOCallback reads the outcome the backend's OIDC callback
// redirect left in the URL *fragment* (see internal/uiapi/oidc.go's own
// doc comment for why a fragment, never the query string) and, on
// success, stores the session exactly like a password login does.
// Returns an empty string on success, or an error message to show the
// operator.
export function completeSSOCallback(): string {
  const result = parseSSOCallbackFragment(window.location.hash);
  if ('error' in result) return result.error;
  setSession(result.token, result.username, false);
  return '';
}

export async function logout(): Promise<void> {
  try {
    await api('/api/v1/auth/logout', { method: 'POST' });
  } finally {
    setSession('', '');
  }
}

// changeOwnPassword re-proves currentPassword (a "confirm password" step,
// same idea as GitHub/GitLab account settings) and, on success, replaces
// it with newPassword -- see POST /api/v1/auth/password in
// internal/uiapi/auth.go. Only meaningful for a session-token login; the
// legacy shared token has no per-user identity to attach a password to.
export async function changeOwnPassword(currentPassword: string, newPassword: string): Promise<void> {
  await apiJSON('/api/v1/auth/password', 'POST', { currentPassword, newPassword });
  setDefaultPassword(false);
}

// resetUserPassword lets an admin account set another operator's password
// without hand-crafting a bcrypt hash and redeploying -- see
// POST /api/v1/users/{username}/password. Immediately invalidates that
// operator's outstanding sessions server-side.
export function resetUserPassword(targetUsername: string, newPassword: string): Promise<void> {
  return apiJSON(`/api/v1/users/${encodeURIComponent(targetUsername)}/password`, 'POST', { newPassword });
}

export interface Config {
  consoleEnabled: boolean;
  atlasEnabled?: boolean;
  atlasConsoleURL?: string;
}

// getConfig reports small feature toggles (currently just consoleEnabled)
// so the UI can decide what to render -- e.g. hiding the "Console" button
// entirely when the deployment doesn't have it configured, rather than
// showing it and only failing after a click.
export function getConfig(): Promise<Config> {
  return api<Config>('/api/v1/config');
}

// getConsoleWebSocketURL fetches a short-lived, single-use ticket (a
// normal authenticated fetch()) and builds the VNC console WebSocket URL
// from it -- a native browser WebSocket can't carry an Authorization
// header, so the real session token never appears in this URL, only the
// disposable ticket does (see internal/uiapi/console.go).
export async function getConsoleWebSocketURL(namespace: string, name: string): Promise<string> {
  const out = await api<{ ticket: string }>(`/api/v1/machines/${namespace}/${encodeURIComponent(name)}/console/ticket`, { method: 'POST' });
  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws';
  return `${proto}://${window.location.host}/api/v1/machines/${namespace}/${encodeURIComponent(name)}/console?ticket=${encodeURIComponent(out.ticket)}`;
}

// getTextConsoleWebSocketURL mirrors getConsoleWebSocketURL above, but
// requests a "text" kind ticket instead of the default "vnc" one (see
// internal/uiapi/console.go's handleConsoleTicket) and forwards the
// terminal's current size so FluxVM's own console starts at the right
// dimensions instead of always defaulting to 80x24.
export async function getTextConsoleWebSocketURL(namespace: string, name: string, cols: number, rows: number): Promise<string> {
  const out = await api<{ ticket: string }>(`/api/v1/machines/${namespace}/${encodeURIComponent(name)}/console/ticket?kind=text`, { method: 'POST' });
  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws';
  return `${proto}://${window.location.host}/api/v1/machines/${namespace}/${encodeURIComponent(name)}/console?ticket=${encodeURIComponent(out.ticket)}&cols=${cols}&rows=${rows}`;
}

export interface ExecRequest {
  path?: string;
  args?: string[];
  powershell?: string;
  timeoutSeconds?: number;
}

export interface ExecResult {
  exitCode: number;
  stdout: string;
  stderr: string;
}

// execInMachine runs a command inside the guest via kairon-node ->
// FluxVM's real qemu-guest-agent guest-exec (internal/uiapi/exec.go) --
// requires spec.guestAgent.enabled and an admin account server-side; this
// call surfaces whatever plain-text/JSON error the server returns on
// either failure the same way every other api() call does.
export function execInMachine(namespace: string, name: string, req: ExecRequest): Promise<ExecResult> {
  return apiJSON<ExecResult>(`/api/v1/machines/${namespace}/${encodeURIComponent(name)}/exec`, 'POST', req);
}

export interface AgentFileResult {
  contentBase64?: string;
  mode?: number;
}

// putAgentFile/getAgentFile write/read a file inside the guest via
// kairon-node -> FluxVM's own bespoke vsock guest agent
// (internal/uiapi/agentfile.go) -- a different channel from guest exec,
// requiring spec.guestAgent.console (FluxVM's proprietary
// fluxvm-guest-agent) and an admin account, not spec.guestAgent.enabled.
export function putAgentFile(namespace: string, name: string, path: string, contentBase64: string, mode?: number): Promise<{ ok: boolean }> {
  return apiJSON(`/api/v1/machines/${namespace}/${encodeURIComponent(name)}/agent-file/put`, 'POST', { path, contentBase64, mode });
}

export function getAgentFile(namespace: string, name: string, path: string): Promise<AgentFileResult> {
  return apiJSON(`/api/v1/machines/${namespace}/${encodeURIComponent(name)}/agent-file/get`, 'POST', { path });
}

// logsURL/authHeaders back the Logs panel's own streaming fetch()
// (internal/uiapi/logs.go) -- a plain chunked text/plain response, not
// JSON, so it can't go through api()'s own r.json() call; the component
// reads resp.body's stream directly instead.
export function logsURL(namespace: string, name: string, lines: number, follow: boolean): string {
  return `/api/v1/machines/${namespace}/${encodeURIComponent(name)}/logs?lines=${lines}&follow=${follow}`;
}

export function authHeaders(): Record<string, string> {
  return token() ? { Authorization: `Bearer ${token()}` } : {};
}

// toBase64/fromBase64 are UTF-8-safe wrappers around the browser's own
// binary-string-only btoa/atob -- plain btoa(text) throws on any
// character outside Latin1, which real file content (UTF-8 source code,
// configs with non-ASCII comments, etc.) routinely has.
export function toBase64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let binary = '';
  bytes.forEach((b) => (binary += String.fromCharCode(b)));
  return btoa(binary);
}

export function fromBase64(b64: string): string {
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return new TextDecoder().decode(bytes);
}

// downloadFile fetches an authenticated path (the bearer token cannot ride on
// a plain <a href>) and saves the response with the given filename.
export async function downloadFile(path: string, filename: string): Promise<void> {
  const r = await fetch(path, { headers: authHeaders() });
  if (r.status === 401) window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
  if (!r.ok) throw new Error((await r.text()) || r.statusText);
  const url = URL.createObjectURL(await r.blob());
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

// apiErrorText unwraps the {"error": "..."} body every uiapi route returns.
export function apiErrorText(e: unknown): string {
  const s = String(e instanceof Error ? e.message : e);
  try {
    return JSON.parse(s).error || s;
  } catch {
    return s;
  }
}
