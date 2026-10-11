// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useEffect, useRef, useState } from 'react';
import { AlertCircle, Eye, EyeOff, Loader2 } from 'lucide-react';
import { AuthConfig, getAuthConfig, loginOrToken } from '../api';
import { BENCHMARKS, BENCHMARK_NOTE } from '../lib/brag';
import { CountValue } from '../components/ui';

const SAVED_USER = 'kairon_saved_user';

function savedUser(): string {
  try {
    return localStorage.getItem(SAVED_USER) || '';
  } catch {
    return '';
  }
}

function remember(user: string | null) {
  try {
    if (user) localStorage.setItem(SAVED_USER, user);
    else localStorage.removeItem(SAVED_USER);
  } catch {
    /* private mode */
  }
}

// Two chapters like zorvia's sign-in: a hero, then the credentials card.
function LoginChrome({ children, hint, stats }: { children: React.ReactNode; hint?: string; stats?: AuthConfig['stats'] }) {
  const card = useRef<HTMLElement>(null);
  return (
    <div className="loginwrap force-light">
      <main className="login-scroll">
        <section className="login-chapter login-hero" data-tone="sky">
          <div className="login-chapter-inner">
            <div className="login-lockup">
              <img src="/zyvor-favicon.svg" alt="Zyvor" width={34} height={34} />
              <span><b>Zyvor</b> Kairon</span>
            </div>
            <div className="login-chips">
              <span className="chip">{window.location.hostname}</span>
              <span className="chip">Kubernetes</span>
              <span className="chip">FluxVM</span>
            </div>
            <h1 className="login-title">Real VMs.<br />One console.</h1>
            <p className="loginbrand-tagline">VM orchestration on FluxVM &mdash; no KubeVirt, no libvirt.</p>
            <div className="login-pills">
              <span data-tone="sky">Machines</span>
              <span data-tone="violet">Migrations</span>
              <span data-tone="emerald">Snapshots</span>
              <span data-tone="orange">Fleet</span>
            </div>
            <div className="login-ctas">
              <button type="button" className="primary" onClick={() => {
                  card.current?.scrollIntoView({ behavior: 'smooth' });
                  card.current?.querySelector('input')?.focus({ preventScroll: true });
                }}>
                Sign in
              </button>
              <a className="login-cta-secondary" href="https://zyvor.dev" target="_blank" rel="noopener noreferrer">
                Learn more
              </a>
            </div>
            <div className="login-stats" aria-label="Why Kairon">
              {BENCHMARKS.map((b) => (
                <div key={b.label} className="login-stat">
                  <b>{b.big}</b>
                  <span>{b.label}</span>
                  <small>{b.detail}</small>
                </div>
              ))}
              {stats && (
                <>
                  <div className="login-stat live">
                    <b><CountValue value={stats.crdKinds} /></b>
                    <span>CRD kinds</span>
                    <small>one API, {stats.hypervisors} hypervisors</small>
                  </div>
                  <div className="login-stat live">
                    <b><CountValue value={stats.apiRoutes} /></b>
                    <span>API routes</span>
                    <small>{stats.version ? 'build ' + stats.version : 'built in'}</small>
                  </div>
                </>
              )}
            </div>
            <p className="login-stats-note">{BENCHMARK_NOTE}</p>
          </div>
        </section>
        <section ref={card} id="login-sign-in" className="login-chapter login-signin" aria-label="Credentials">
          <div className="login-chapter-inner">
            <p className="login-form-heading">Sign in to Kairon</p>
            <div className="logincard">{children}</div>
            {hint && <p className="loginhint">{hint}</p>}
          </div>
        </section>
      </main>
      <p className="loginhost">
        &copy; 2026 Zyvor &middot; <a href="https://zyvor.dev" target="_blank" rel="noopener noreferrer">zyvor.dev</a>
      </p>
    </div>
  );
}

function Field({
  label,
  value,
  onChange,
  type = 'text',
  autoFocus,
  autoComplete,
  right,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  type?: string;
  autoFocus?: boolean;
  autoComplete?: string;
  right?: React.ReactNode;
}) {
  return (
    <label className={'siw-field' + (value ? ' filled' : '')}>
      <input
        required
        type={type}
        autoFocus={autoFocus}
        autoComplete={autoComplete}
        value={value}
        placeholder=" "
        onChange={(e) => onChange(e.target.value)}
      />
      <span>{label}</span>
      {right}
    </label>
  );
}

export default function Login({ onSignedIn }: { onSignedIn: () => void }) {
  const [config, setConfig] = useState<AuthConfig | null>(null);
  const [user, setUser] = useState(savedUser() || 'admin');
  const [password, setPassword] = useState('');
  const [showPw, setShowPw] = useState(false);
  const [rememberMe, setRememberMe] = useState(!!savedUser());
  const [msg, setMsg] = useState('');
  const [shake, setShake] = useState(0);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    getAuthConfig()
      .then(setConfig)
      // An old or unreachable server falls back to the token-as-password path.
      .catch(() => setConfig({ loginEnabled: false, tokenEnabled: true }));
  }, []);

  async function signIn(e: React.FormEvent) {
    e.preventDefault();
    if (!config) return;
    setBusy(true);
    setMsg('');
    try {
      const how = await loginOrToken(user.trim(), password, config);
      remember(rememberMe && how === 'password' ? user.trim() : null);
      onSignedIn();
    } catch {
      setPassword('');
      setMsg('Wrong username or password.');
      setShake((n) => n + 1);
    } finally {
      setBusy(false);
    }
  }

  if (!config) {
    return (
      <LoginChrome>
        <div className="loginskeleton" />
      </LoginChrome>
    );
  }

  const sso = config.ssoEnabled && config.ssoLoginURL && (
    <div className="loginstep">
      <button type="button" className="primary wide" onClick={() => (window.location.href = config.ssoLoginURL!)}>
        Sign in with SSO
      </button>
    </div>
  );

  if (!config.loginEnabled && !config.tokenEnabled) {
    return (
      <LoginChrome stats={config.stats}>
        {sso}
        {!sso && <p className="logincopy">No login method is configured on this server.</p>}
      </LoginChrome>
    );
  }

  return (
    <LoginChrome hint={config.loginHint} stats={config.stats}>
      {sso}
      {sso && <p className="loginor">or</p>}
      <div className="loginstep">
        <form onSubmit={signIn} key={shake} className={shake ? 'shake' : undefined}>
          <h3>Sign in.</h3>
          <div className="siw stacked">
            <Field label="Username" autoComplete="username" autoFocus={!user} value={user} onChange={setUser} />
            <Field
              label="Password"
              type={showPw ? 'text' : 'password'}
              autoComplete="current-password"
              autoFocus={!!user}
              value={password}
              onChange={setPassword}
              right={
                <button type="button" className="siw-eye" aria-label={showPw ? 'Hide password' : 'Show password'} onClick={() => setShowPw((v) => !v)}>
                  {showPw ? <EyeOff size={16} /> : <Eye size={16} />}
                </button>
              }
            />
          </div>
          <label className="login-remember">
            <input type="checkbox" checked={rememberMe} onChange={(e) => setRememberMe(e.target.checked)} />
            <span>Remember me on this device</span>
          </label>
          {msg && (
            <div className="loginerror" role="alert">
              <AlertCircle size={16} />
              <span>{msg}</span>
            </div>
          )}
          <div className="formactions">
            <button className="primary wide" type="submit" disabled={busy || !user || !password}>
              {busy ? <Loader2 size={16} className="spin" /> : 'Sign in'}
            </button>
          </div>
        </form>
      </div>
    </LoginChrome>
  );
}
