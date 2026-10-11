// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { LogOut, Menu, Moon, Search, Sun, UserCog, X } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { api } from '../api';
import { currentNamespace, setNamespace } from '../lib/namespace';
import { NAV, NavItem, Page } from '../lib/nav';
import { applyTheme, storedTheme, Theme } from '../lib/theme';
import { iconFor } from './navIcons';

export default function GlobalNav({
  page,
  go,
  username,
  health,
  onSearch,
  onSignOut,
}: {
  page: Page;
  go: (p: Page) => void;
  username?: string;
  health: 'ok' | 'bad' | 'unknown';
  onSearch: () => void;
  onSignOut: () => void;
}) {
  const [open, setOpen] = useState<string | null>(null);
  const [user, setUser] = useState(false);
  const [mobile, setMobile] = useState(false);
  const [theme, setTheme] = useState<Theme>(storedTheme());
  const closeTimer = useRef<number | undefined>(undefined);
  const [namespaces, setNamespaces] = useState<string[]>([]);
  const namespace = currentNamespace();

  // The namespaces this operator may use, for the nav selector.
  useEffect(() => {
    let alive = true;
    api<string[]>('/api/v1/namespaces')
      .then((list) => alive && setNamespaces(list))
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, []);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && (setOpen(null), setUser(false), setMobile(false));
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, []);

  function pick(p: Page) {
    setOpen(null);
    setUser(false);
    setMobile(false);
    go(p);
  }
  function setT(t: Theme) {
    setTheme(t);
    applyTheme(t);
  }
  const openGroup = NAV.find((g) => g.group === open);
  const showMenu = (g: string) => {
    window.clearTimeout(closeTimer.current);
    setOpen(g);
  };
  const hideMenu = () => {
    window.clearTimeout(closeTimer.current);
    closeTimer.current = window.setTimeout(() => setOpen(null), 120);
  };
  const activeGroup = NAV.find((g) => (g.items as readonly NavItem[]).some((i) => i.id === page))?.group;

  return (
    <>
      <nav className="gn" aria-label="Primary" onMouseLeave={hideMenu}>
        <button className="gn-burger icon ghost" onClick={() => setMobile((m) => !m)} aria-label="Menu">
          {mobile ? <X size={18} /> : <Menu size={18} />}
        </button>
        <button className="gn-brand" onClick={() => pick('overview')} aria-label="Kairon home">
          <img src="/zyvor-logomark.svg" alt="" width={22} height={22} className="gn-mark" />
          KAIRON <small>by Zyvor</small>
        </button>

        <div className="gn-links">
          {NAV.map((g) =>
            g.items.length === 1 ? (
              <button
                key={g.group}
                className={'gn-link' + (activeGroup === g.group ? ' active' : '')}
                onClick={() => pick(g.items[0].id as Page)}
                onMouseEnter={() => (window.clearTimeout(closeTimer.current), setOpen(null))}
              >
                {g.group === 'Overview' ? 'Overview' : g.items[0].label}
              </button>
            ) : (
              <button
                key={g.group}
                className={'gn-link' + (open === g.group ? ' open' : '') + (activeGroup === g.group ? ' active' : '')}
                aria-expanded={open === g.group}
                onMouseEnter={() => showMenu(g.group)}
                onFocus={() => showMenu(g.group)}
                onClick={() => setOpen(open === g.group ? null : g.group)}
              >
                {g.group}
              </button>
            ),
          )}
        </div>

        <div className="gn-right">
          <label className="gn-ns" title="Namespace the dashboard works in">
            <span>ns</span>
            <select aria-label="Namespace" value={namespace} onChange={(e) => setNamespace(e.target.value)}>
              {[...new Set([namespace, ...namespaces])].map((ns) => (
                <option key={ns} value={ns}>
                  {ns}
                </option>
              ))}
            </select>
          </label>
          <button className="gn-search" onClick={onSearch} aria-label="Search (Command K)">
            <Search size={14} />
            <span>Search</span>
            <kbd>⌘K</kbd>
          </button>
          <span className={'live-dot ' + health} title={health === 'ok' ? 'Control plane reachable' : health === 'bad' ? 'Control plane unreachable' : 'Checking…'} />
          <button className="icon ghost" onClick={() => setUser((u) => !u)} aria-label="Account menu" aria-expanded={user}>
            <span className="loginavatar" style={{ width: 26, height: 26, fontSize: 12 }}>{username ? username.charAt(0).toUpperCase() : <UserCog size={14} />}</span>
          </button>
        </div>
      </nav>

      {openGroup && (
        <>
          <div className="mega-scrim" onMouseEnter={hideMenu} onClick={() => setOpen(null)} />
          <div className="mega" data-tone={openGroup.tone} onMouseEnter={() => showMenu(openGroup.group)} onMouseLeave={hideMenu}>
            <div className="mega-inner">
              <span className="eyebrow">{openGroup.group}</span>
              <div className="mega-grid">
                {(openGroup.items as readonly NavItem[]).map((it, i) => (
                  <button key={it.id} className="mega-item" style={{ ['--i' as string]: i }} onClick={() => pick(it.id as Page)}>
                    {iconFor(it.id)}
                    <div>
                      <b>{it.label}</b>
                      <span>{it.blurb}</span>
                    </div>
                  </button>
                ))}
              </div>
            </div>
          </div>
        </>
      )}

      {user && (
        <>
          <div style={{ position: 'fixed', inset: 0, zIndex: 250 }} onClick={() => setUser(false)} />
          <div className="menu" role="menu">
            <div className="menu-head">
              <b>{username || 'Signed in'}</b>
              <span>{window.location.host}</span>
            </div>
            <button className="menu-item" onClick={() => pick('account')}>
              <UserCog size={16} /> Account
            </button>
            <div className="seg" role="group" aria-label="Theme">
              {(['dark', 'light', 'system'] as Theme[]).map((t) => (
                <button key={t} className={theme === t ? 'on' : ''} onClick={() => setT(t)}>
                  {t === 'dark' ? <Moon size={12} /> : t === 'light' ? <Sun size={12} /> : null} {t}
                </button>
              ))}
            </div>
            <button className="menu-item" onClick={onSignOut}>
              <LogOut size={16} /> Sign out
            </button>
          </div>
        </>
      )}

      {mobile && (
        <div className="mobile-sheet">
          {NAV.map((g) => (
            <div key={g.group} data-tone={g.tone}>
              <h4>{g.group}</h4>
              {(g.items as readonly NavItem[]).map((it) => (
                <button key={it.id} className="mega-item" style={{ opacity: 1, animation: 'none' }} onClick={() => pick(it.id as Page)}>
                  {iconFor(it.id)}
                  <div><b>{it.label}</b><span>{it.blurb}</span></div>
                </button>
              ))}
            </div>
          ))}
        </div>
      )}
    </>
  );
}
