// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useEffect, useState } from 'react';
import { AlertCircle } from 'lucide-react';
import { completeSSOCallback } from '../api';

// OIDCCallback is the SPA-side landing spot for the backend's OIDC
// redirect (internal/uiapi/oidc.go's oidcCallbackPath) -- there's no
// router in this app (App.tsx picks pages from a plain state field, not a
// URL), so App.tsx renders this component directly whenever
// window.location.pathname is /oidc/callback, instead of adding a general
// routing library for one page.
export default function OIDCCallback({ onDone }: { onDone: () => void }) {
  const [error, setError] = useState('');

  useEffect(() => {
    const err = completeSSOCallback();
    if (err) {
      setError(err);
      return;
    }
    onDone();
    // Only ever run once, against the fragment this page loaded with.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  if (!error) return null;

  return (
    <div className="loginwrap">
      <div className="loginsplit">
        <div className="loginsplit-right" style={{ margin: '0 auto' }}>
          <div className="card logincard">
            <span className="eyebrow">SIGN IN</span>
            <h3>SSO sign-in failed</h3>
            <div className="loginerror">
              <AlertCircle size={16} />
              <span>{error}</span>
            </div>
            <div className="formactions">
              <button className="primary" type="button" onClick={() => window.location.replace('/')}>
                Back to sign in
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
