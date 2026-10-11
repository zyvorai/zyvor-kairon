// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef, useState } from 'react';
import { authHeaders, UNAUTHORIZED_EVENT } from '../api';

export interface LiveChange {
  type: 'ADDED' | 'MODIFIED' | 'DELETED';
  kind: 'Machine' | 'MachineMigration';
  namespace: string;
  name: string;
  phase?: string;
  nodeName?: string;
  message?: string;
}

// parseSSE splits a streamed buffer into complete "change" events and the
// unconsumed remainder. Comments (": ping") and unknown events are dropped;
// a malformed data line is skipped rather than ending the stream.
export function parseSSE(buffer: string): { changes: LiveChange[]; rest: string } {
  const changes: LiveChange[] = [];
  const parts = buffer.replace(/\r\n/g, '\n').split('\n\n');
  const rest = parts.pop() ?? '';
  for (const block of parts) {
    let event = 'message';
    const data: string[] = [];
    for (const line of block.split('\n')) {
      if (line.startsWith(':')) continue;
      if (line.startsWith('event:')) event = line.slice(6).trim();
      else if (line.startsWith('data:')) data.push(line.slice(5).trimStart());
    }
    if (event !== 'change' || data.length === 0) continue;
    try {
      changes.push(JSON.parse(data.join('\n')) as LiveChange);
    } catch {
      /* skip malformed payload */
    }
  }
  return { changes, rest };
}

// nextBackoff doubles the reconnect delay up to a 30 s ceiling.
export function nextBackoff(ms: number): number {
  return Math.min(ms * 2, 30000);
}

// useLiveChanges subscribes to GET /api/v1/stream while the tab is visible
// and calls onChange for each Machine/Migration change. It returns whether
// the stream is currently connected so callers can slow their polling down.
// fetch streaming is used (not EventSource) because the route needs the
// Authorization header.
export function useLiveChanges(onChange: (c: LiveChange) => void): boolean {
  const [connected, setConnected] = useState(false);
  const cb = useRef(onChange);
  cb.current = onChange;

  useEffect(() => {
    let stopped = false;
    let ctrl: AbortController | null = null;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let delay = 1000;

    async function run() {
      if (stopped || document.hidden) return;
      ctrl = new AbortController();
      try {
        const r = await fetch('/api/v1/stream', { headers: authHeaders(), signal: ctrl.signal });
        if (r.status === 401) {
          window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
          return;
        }
        if (!r.ok || !r.body) throw new Error(r.statusText);
        setConnected(true);
        delay = 1000;
        const reader = r.body.getReader();
        const dec = new TextDecoder();
        let buf = '';
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          const parsed = parseSSE(buf);
          buf = parsed.rest;
          parsed.changes.forEach((c) => cb.current(c));
        }
      } catch {
        /* aborted or network error: fall through to reconnect */
      }
      setConnected(false);
      if (stopped || document.hidden) return;
      timer = setTimeout(run, delay);
      delay = nextBackoff(delay);
    }

    const onVisibility = () => {
      if (document.hidden) {
        ctrl?.abort();
        clearTimeout(timer);
      } else if (!stopped) {
        delay = 1000;
        clearTimeout(timer);
        run();
      }
    };
    document.addEventListener('visibilitychange', onVisibility);
    run();
    return () => {
      stopped = true;
      document.removeEventListener('visibilitychange', onVisibility);
      clearTimeout(timer);
      ctrl?.abort();
      setConnected(false);
    };
  }, []);

  return connected;
}
