// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useEffect, useRef, useState } from 'react';
import { X } from 'lucide-react';
import RFB from '@novnc/novnc';
import { getConsoleWebSocketURL } from '../api';

type Status = 'connecting' | 'connected' | 'error' | 'disconnected';

// Console is a full-screen VNC viewer for one Machine: browser <-> kairon-ui
// <-> kairon-node <-> the VM's local QEMU VNC socket (see
// internal/uiapi/console.go, internal/consoleproxy). Graphical VNC is
// QEMU-backend-only and requires the Machine to be Running -- both are
// enforced server-side, so failures here are reported plainly rather than
// guessed at client-side.
export default function Console({ namespace, name, onClose }: { namespace: string; name: string; onClose: () => void }) {
  const containerRef = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<Status>('connecting');
  const [msg, setMsg] = useState('');

  useEffect(() => {
    let rfb: RFB | null = null;
    let cancelled = false;

    (async () => {
      try {
        const url = await getConsoleWebSocketURL(namespace, name);
        if (cancelled || !containerRef.current) return;
        rfb = new RFB(containerRef.current, url);
        rfb.scaleViewport = true;
        rfb.addEventListener('connect', () => setStatus('connected'));
        rfb.addEventListener('disconnect', (e) => {
          const clean = (e as CustomEvent<{ clean: boolean }>).detail?.clean;
          setStatus('disconnected');
          setMsg(clean ? 'Console disconnected.' : 'Console connection lost.');
        });
        rfb.addEventListener('securityfailure', (e) => {
          const reason = (e as CustomEvent<{ reason: string }>).detail?.reason;
          setStatus('error');
          setMsg(reason || 'Console security handshake failed.');
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
      rfb?.disconnect();
    };
  }, [namespace, name]);

  return (
    <div className="consoleOverlay">
      <div className="consoleBar">
        <span>
          Console &middot; <strong>{name}</strong>
        </span>
        <span className={`consoleStatus consoleStatus-${status}`}>
          {status === 'connecting' && 'Connecting…'}
          {status === 'connected' && 'Connected'}
          {status === 'error' && `Error: ${msg}`}
          {status === 'disconnected' && msg}
        </span>
        <button className="linklike" onClick={onClose} aria-label="Close console">
          <X size={20} />
        </button>
      </div>
      <div className="consoleScreen" ref={containerRef} />
    </div>
  );
}
