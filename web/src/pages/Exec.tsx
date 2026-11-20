// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useState } from 'react';
import { X } from 'lucide-react';
import { execInMachine, ExecResult } from '../api';

// Exec is a one-shot "run a command in the guest" panel: kairon-ui ->
// kairon-node -> FluxVM's real qemu-guest-agent guest-exec
// (internal/uiapi/exec.go). Unlike Console.tsx, this isn't interactive or
// streaming -- one request, one complete result, since that's exactly the
// contract FluxVM's own guest-exec already has (it does its own
// guest-exec-status polling internally and only ever returns once the
// command has finished or timed out).
export default function Exec({ namespace, name, onClose }: { namespace: string; name: string; onClose: () => void }) {
  const [path, setPath] = useState('');
  const [args, setArgs] = useState('');
  const [powershell, setPowershell] = useState('');
  const [mode, setMode] = useState<'path' | 'powershell'>('path');
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<ExecResult | null>(null);
  const [err, setErr] = useState('');

  async function run(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setErr('');
    setResult(null);
    try {
      const out =
        mode === 'powershell'
          ? await execInMachine(namespace, name, { powershell })
          : await execInMachine(namespace, name, { path, args: args.trim() ? args.trim().split(/\s+/) : undefined });
      setResult(out);
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
          Exec &middot; <strong>{name}</strong>
        </span>
        <button className="linklike" onClick={onClose} aria-label="Close exec">
          <X size={20} />
        </button>
      </div>
      <div className="execBody">
        <form onSubmit={run} className="execForm">
          <div className="execModeRow">
            <label>
              <input type="radio" checked={mode === 'path'} onChange={() => setMode('path')} /> Command
            </label>
            <label>
              <input type="radio" checked={mode === 'powershell'} onChange={() => setMode('powershell')} /> PowerShell (Windows guests)
            </label>
          </div>
          {mode === 'path' ? (
            <>
              <input placeholder="/bin/echo" value={path} onChange={(e) => setPath(e.target.value)} required />
              <input placeholder="arguments (space-separated)" value={args} onChange={(e) => setArgs(e.target.value)} />
            </>
          ) : (
            <input placeholder="Get-Process" value={powershell} onChange={(e) => setPowershell(e.target.value)} required />
          )}
          <button type="submit" disabled={busy}>
            {busy ? 'Running…' : 'Run'}
          </button>
        </form>
        {err && <div className="execError">{err}</div>}
        {result && (
          <div className="execResult">
            <div className="execExitCode">Exit code: {result.exitCode}</div>
            {result.stdout && (
              <>
                <div className="execStreamLabel">stdout</div>
                <pre className="execStream">{result.stdout}</pre>
              </>
            )}
            {result.stderr && (
              <>
                <div className="execStreamLabel">stderr</div>
                <pre className="execStream">{result.stderr}</pre>
              </>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
