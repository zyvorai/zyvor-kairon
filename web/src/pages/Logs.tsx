// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useEffect, useRef, useState } from 'react';
import { X } from 'lucide-react';
import { logsURL, authHeaders, UNAUTHORIZED_EVENT } from '../api';

// Logs streams a Machine's captured serial console output --
// internal/uiapi/logs.go's plain chunked text/plain response, not a
// WebSocket -- read directly via fetch()'s own streaming response body.
// Kairon's `kubectl logs`/`kaironctl logs` equivalent.
export default function Logs({ namespace, name, onClose }: { namespace: string; name: string; onClose: () => void }) {
  const [content, setContent] = useState('');
  const [follow, setFollow] = useState(true);
  const [status, setStatus] = useState<'connecting' | 'connected' | 'error' | 'done'>('connecting');
  const [err, setErr] = useState('');
  const preRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;

    (async () => {
      setStatus('connecting');
      setContent('');
      try {
        const resp = await fetch(logsURL(namespace, name, 200, follow), { headers: authHeaders(), signal: controller.signal });
        if (resp.status === 401) window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
        if (!resp.ok || !resp.body) {
          throw new Error((await resp.text()) || resp.statusText);
        }
        setStatus('connected');
        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          if (cancelled) return;
          setContent((c) => c + decoder.decode(value, { stream: true }));
        }
        if (!cancelled) setStatus('done');
      } catch (e) {
        if (!cancelled && (e as Error).name !== 'AbortError') {
          setStatus('error');
          setErr(String(e));
        }
      }
    })();

    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [namespace, name, follow]);

  useEffect(() => {
    preRef.current?.scrollTo({ top: preRef.current.scrollHeight });
  }, [content]);

  return (
    <div className="consoleOverlay">
      <div className="consoleBar">
        <span>
          Logs &middot; <strong>{name}</strong>
        </span>
        <label style={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: '6px' }}>
          <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> Follow
        </label>
        <span className={`consoleStatus consoleStatus-${status === 'connected' || status === 'done' ? 'connected' : status}`}>
          {status === 'connecting' && 'Connecting…'}
          {status === 'connected' && (follow ? 'Streaming' : 'Connected')}
          {status === 'done' && 'Done'}
          {status === 'error' && `Error: ${err}`}
        </span>
        <button className="linklike" onClick={onClose} aria-label="Close logs">
          <X size={20} />
        </button>
      </div>
      <pre ref={preRef} className="execStream" style={{ flex: 1, margin: 0, borderRadius: 0, maxHeight: 'none', overflow: 'auto' }}>
        {content || (status === 'connecting' ? '' : '(no output yet)')}
      </pre>
    </div>
  );
}
