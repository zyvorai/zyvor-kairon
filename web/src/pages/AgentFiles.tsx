// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useState } from 'react';
import { X } from 'lucide-react';
import { getAgentFile, putAgentFile, toBase64, fromBase64 } from '../api';

// AgentFiles is a one-shot "read/write a guest file" panel -- kairon-ui
// -> kairon-node -> FluxVM's own bespoke vsock guest agent
// (internal/uiapi/agentfile.go, docs/guides/machine-guest-agent-files.md).
// Text content only in this first cut: content is encoded/decoded as
// UTF-8 client-side (toBase64/fromBase64 in ../api), so an arbitrary
// binary file round-trips only if it happens to be valid UTF-8.
export default function AgentFiles({ namespace, name, onClose }: { namespace: string; name: string; onClose: () => void }) {
  const [mode, setMode] = useState<'get' | 'put'>('get');
  const [path, setPath] = useState('');
  const [content, setContent] = useState('');
  const [fileMode, setFileMode] = useState('');
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState('');
  const [err, setErr] = useState('');

  async function run(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setErr('');
    setMsg('');
    try {
      if (mode === 'get') {
        const out = await getAgentFile(namespace, name, path);
        setContent(out.contentBase64 ? fromBase64(out.contentBase64) : '');
        setMsg(`Read ${path}${out.mode !== undefined ? ` (mode ${out.mode.toString(8)})` : ''}.`);
      } else {
        const modeNum = fileMode.trim() ? parseInt(fileMode.trim(), 8) : undefined;
        await putAgentFile(namespace, name, path, toBase64(content), modeNum);
        setMsg(`Wrote ${path}.`);
      }
    } catch (e) {
      setErr(String(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="consoleOverlay">
      <div className="consoleBar">
        <span>
          Guest files &middot; <strong>{name}</strong>
        </span>
        <button className="linklike" onClick={onClose} aria-label="Close guest files">
          <X size={20} />
        </button>
      </div>
      <div className="execBody">
        <form onSubmit={run} className="execForm">
          <div className="execModeRow">
            <label>
              <input type="radio" checked={mode === 'get'} onChange={() => setMode('get')} /> Read file
            </label>
            <label>
              <input type="radio" checked={mode === 'put'} onChange={() => setMode('put')} /> Write file
            </label>
          </div>
          <input placeholder="/etc/app/config.yaml" value={path} onChange={(e) => setPath(e.target.value)} required />
          {mode === 'put' && <input placeholder="mode, e.g. 644 (octal, optional)" value={fileMode} onChange={(e) => setFileMode(e.target.value)} />}
          <button type="submit" disabled={busy}>
            {busy ? 'Working…' : mode === 'get' ? 'Read' : 'Write'}
          </button>
        </form>
        {mode === 'put' && (
          <textarea
            className="execStream"
            style={{ width: '100%', minHeight: '30vh' }}
            placeholder="File content (UTF-8 text)"
            value={content}
            onChange={(e) => setContent(e.target.value)}
          />
        )}
        {err && <div className="execError">{err}</div>}
        {msg && <div className="execExitCode">{msg}</div>}
        {mode === 'get' && content && (
          <>
            <div className="execStreamLabel">content</div>
            <pre className="execStream">{content}</pre>
          </>
        )}
      </div>
    </div>
  );
}
