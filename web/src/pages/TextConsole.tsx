// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useEffect, useRef, useState } from 'react';
import { X } from 'lucide-react';
import { Terminal } from '@xterm/xterm';
import '@xterm/xterm/css/xterm.css';
import { getTextConsoleWebSocketURL } from '../api';

type Status = 'connecting' | 'connected' | 'error' | 'disconnected';

// TextConsole is a full-screen interactive shell for one Machine: browser
// (xterm.js) <-> kairon-ui <-> kairon-node <-> FluxVM's own WebSocket-
// upgraded console endpoint (see internal/uiapi/console.go,
// internal/consoleproxy, docs/guides/machine-text-console.md). Requires
// spec.guestAgent.console -- a different, FluxVM-proprietary vsock channel
// from the graphical VNC console (Console.tsx), and unlike VNC it works
// on every backend, not just qemu.
export default function TextConsole({ namespace, name, onClose }: { namespace: string; name: string; onClose: () => void }) {
  const containerRef = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<Status>('connecting');
  const [msg, setMsg] = useState('');

  useEffect(() => {
    let term: Terminal | null = null;
    let ws: WebSocket | null = null;
    let cancelled = false;

    (async () => {
      try {
        term = new Terminal({ cursorBlink: true, convertEol: true });
        if (cancelled || !containerRef.current) return;
        term.open(containerRef.current);

        const url = await getTextConsoleWebSocketURL(namespace, name, term.cols, term.rows);
        if (cancelled) {
          term.dispose();
          return;
        }
        ws = new WebSocket(url);
        ws.binaryType = 'arraybuffer';
        ws.onopen = () => setStatus('connected');
        ws.onclose = (e) => {
          setStatus('disconnected');
          setMsg(e.wasClean ? 'Console disconnected.' : 'Console connection lost.');
        };
        ws.onerror = () => {
          setStatus('error');
          setMsg('Console connection failed.');
        };
        ws.onmessage = (e) => {
          const data = e.data instanceof ArrayBuffer ? new Uint8Array(e.data) : e.data;
          term?.write(typeof data === 'string' ? data : new TextDecoder().decode(data));
        };
        term.onData((data) => {
          if (ws?.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(data));
        });
      } catch (err) {
        if (!cancelled) {
          setStatus('error');
          setMsg(String(err));
        }
      }
    })();

    return () => {
      cancelled = true;
      ws?.close();
      term?.dispose();
    };
  }, [namespace, name]);

  return (
    <div className="consoleOverlay">
      <div className="consoleBar">
        <span>
          Text console &middot; <strong>{name}</strong>
        </span>
        <span className={`consoleStatus consoleStatus-${status}`}>
          {status === 'connecting' && 'Connecting…'}
          {status === 'connected' && 'Connected'}
          {status === 'error' && `Error: ${msg}`}
          {status === 'disconnected' && msg}
        </span>
        <button className="linklike" onClick={onClose} aria-label="Close text console">
          <X size={20} />
        </button>
      </div>
      <div className="textConsoleScreen" ref={containerRef} />
    </div>
  );
}
