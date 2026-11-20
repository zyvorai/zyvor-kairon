// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useState } from 'react';
import { changeOwnPassword, isAdmin, resetUserPassword, username } from '../api';

// Account replaces "regenerate a bcrypt hash and redeploy" with in-place
// password management (see POST /api/v1/auth/password and
// POST /api/v1/users/{username}/password in internal/uiapi/auth.go). Only
// reachable when signed in via a per-operator session (App.tsx only shows
// the nav entry that leads here when username() is set) -- the legacy
// shared token has no per-user identity to change a password for.
export default function Account() {
  const [currentPassword, setCurrentPassword] = useState('');
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [msg, setMsg] = useState('');
  const [busy, setBusy] = useState(false);

  const [resetTarget, setResetTarget] = useState('');
  const [resetNewPassword, setResetNewPassword] = useState('');
  const [resetMsg, setResetMsg] = useState('');
  const [resetBusy, setResetBusy] = useState(false);

  async function submitChange(e: React.FormEvent) {
    e.preventDefault();
    setMsg('');
    if (newPassword !== confirmPassword) {
      setMsg('New password and confirmation do not match.');
      return;
    }
    setBusy(true);
    try {
      await changeOwnPassword(currentPassword, newPassword);
      setCurrentPassword('');
      setNewPassword('');
      setConfirmPassword('');
      setMsg('Password changed.');
    } catch (err) {
      setMsg(String(err));
    } finally {
      setBusy(false);
    }
  }

  async function submitReset(e: React.FormEvent) {
    e.preventDefault();
    setResetMsg('');
    if (!resetTarget || !resetNewPassword) return;
    setResetBusy(true);
    try {
      await resetUserPassword(resetTarget, resetNewPassword);
      setResetMsg(`Password reset for ${resetTarget}. Their current sessions have been signed out.`);
      setResetTarget('');
      setResetNewPassword('');
    } catch (err) {
      setResetMsg(String(err));
    } finally {
      setResetBusy(false);
    }
  }

  return (
    <div className="grid">
      <div className="card span4">
        <span className="eyebrow">ACCOUNT</span>
        <h3>Change your password</h3>
        <form onSubmit={submitChange}>
          <div className="formgrid">
            <label>
              Current password
              <input required type="password" autoComplete="current-password" value={currentPassword} onChange={(e) => setCurrentPassword(e.target.value)} />
            </label>
            <label>
              New password
              <input required type="password" minLength={8} autoComplete="new-password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} />
            </label>
            <label>
              Confirm new password
              <input required type="password" minLength={8} autoComplete="new-password" value={confirmPassword} onChange={(e) => setConfirmPassword(e.target.value)} />
            </label>
          </div>
          <div className="formactions">
            <button className="primary" type="submit" disabled={busy}>
              {busy ? 'Changing...' : 'Change password'}
            </button>
            {msg && <span className={msg.toLowerCase().includes('error') || msg.toLowerCase().includes('incorrect') || msg.toLowerCase().includes('match') ? 'msg error' : 'msg'}>{msg}</span>}
          </div>
        </form>
        <p className="hint">
          Signed in as <strong>{username()}</strong>.
        </p>
      </div>

      {isAdmin() && (
        <div className="card span4">
          <span className="eyebrow">ADMIN</span>
          <h3>Reset another operator&rsquo;s password</h3>
          <form onSubmit={submitReset}>
            <div className="formgrid">
              <label>
                Username
                <input required value={resetTarget} onChange={(e) => setResetTarget(e.target.value)} placeholder="the account to reset" />
              </label>
              <label>
                New password
                <input required type="password" minLength={8} value={resetNewPassword} onChange={(e) => setResetNewPassword(e.target.value)} />
              </label>
            </div>
            <div className="formactions">
              <button className="primary" type="submit" disabled={resetBusy}>
                {resetBusy ? 'Resetting...' : 'Reset password'}
              </button>
              {resetMsg && <span className={resetMsg.toLowerCase().includes('error') ? 'msg error' : 'msg'}>{resetMsg}</span>}
            </div>
          </form>
          <p className="hint">Immediately signs that operator&rsquo;s current sessions out.</p>
        </div>
      )}
    </div>
  );
}
