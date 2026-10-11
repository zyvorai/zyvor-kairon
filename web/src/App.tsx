// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { useCallback, useEffect, useMemo, useState } from 'react';
import GlobalNav from './components/GlobalNav';
import CommandPalette, { pageEntries, PaletteEntry } from './components/CommandPalette';
import { ToastProvider } from './components/Toast';
import { namespaceView, NAMESPACE_EVENT } from './lib/namespace';
import { formatHash, parseHash } from './lib/route';
import { Page } from './lib/nav';
import { applyTheme, storedTheme } from './lib/theme';
import Overview from './pages/Overview';
import Fleet from './pages/Fleet';
import Machines from './pages/Machines';
import Migrations from './pages/Migrations';
import Snapshots from './pages/Snapshots';
import Restores from './pages/Restores';
import Quotas from './pages/Quotas';
import DisruptionBudgets from './pages/DisruptionBudgets';
import MachineSets from './pages/MachineSets';
import InstanceTypes from './pages/InstanceTypes';
import MigrationPolicies from './pages/MigrationPolicies';
import SnapshotSchedules from './pages/SnapshotSchedules';
import NetworkPolicies from './pages/NetworkPolicies';
import SecurityGroups from './pages/SecurityGroups';
import Nodes from './pages/Nodes';
import System from './pages/System';
import Account from './pages/Account';
import Assistant from './pages/Assistant';
import Login from './pages/Login';
import OIDCCallback from './pages/OIDCCallback';
import { api, logout, SESSION_CHANGED_EVENT, token, UNAUTHORIZED_EVENT, username, usingDefaultPassword } from './api';
import { Machine } from './types';
import Storage from './pages/Storage';
import Pools from './pages/Pools';
import Images from './pages/Images';
import Backups from './pages/Backups';
import { PageHero } from './components/ui';
import { groupOf, NAV, NavItem } from './lib/nav';

export default function App() {
  const initial = parseHash(window.location.hash);
  const [page, setPageState] = useState<Page>(initial.page);
  const [signedIn, setSignedIn] = useState(!!token());
  const [weakPassword, setWeakPassword] = useState(usingDefaultPassword());
  useEffect(() => {
    const sync = () => setWeakPassword(usingDefaultPassword());
    window.addEventListener(SESSION_CHANGED_EVENT, sync);
    return () => window.removeEventListener(SESSION_CHANGED_EVENT, sync);
  }, []);
  const [prefillMachine, setPrefillMachine] = useState('');
  const [prefillSnapshot, setPrefillSnapshot] = useState('');
  const [palette, setPalette] = useState(false);
  // The selected namespace view (a namespace, or all); changing it remounts the page (see <main key>).
  const [ns, setNs] = useState(namespaceView());
  useEffect(() => {
    const onNs = () => setNs(namespaceView());
    window.addEventListener(NAMESPACE_EVENT, onNs);
    return () => window.removeEventListener(NAMESPACE_EVENT, onNs);
  }, []);
  const [machines, setMachines] = useState<Machine[]>([]);
  const [health, setHealth] = useState<'ok' | 'bad' | 'unknown'>('unknown');
  // No client-side router elsewhere in this app -- this one path is the
  // sole exception, since the OIDC callback (see internal/uiapi/oidc.go)
  // has to land somewhere real rather than at whatever page the operator
  // happened to be on when they clicked "Sign in with SSO". Mutable (not
  // a one-time lazy useState) because onDone() below must be able to turn
  // it back off once handled -- otherwise this stays true for the rest of
  // the page's life (window.location.pathname doesn't get re-read after
  // the initial render just because history.replaceState changed it), and
  // OIDCCallback renders nothing on success, leaving the screen blank
  // forever until a manual reload.
  const [onOIDCCallback, setOnOIDCCallback] = useState(() => window.location.pathname === '/oidc/callback');

  useEffect(() => applyTheme(storedTheme()), []);

  // Hash routing: keep `page` and location.hash in sync both ways so the
  // back button and shared links work.
  const setPage = useCallback((p: Page) => {
    setPageState(p);
    const h = formatHash({ page: p });
    if (window.location.hash !== h) window.location.hash = h;
    window.scrollTo({ top: 0 });
  }, []);
  useEffect(() => {
    const onHash = () => setPageState(parseHash(window.location.hash).page);
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  useEffect(() => {
    // Fired by api.ts on any 401, from anywhere in the app -- a stale or
    // revoked session bounces straight back to the login screen instead of
    // leaving the dashboard up with every action silently failing.
    const onUnauthorized = () => setSignedIn(false);
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
  }, []);

  // Control-plane heartbeat (nav dot) + machine list for the palette.
  useEffect(() => {
    if (!signedIn) return;
    let cancelled = false;
    const tick = () => {
      api('/api/v1/overview')
        .then(() => !cancelled && setHealth('ok'))
        .catch(() => !cancelled && setHealth('bad'));
      api<Machine[]>('/api/v1/machines')
        .then((m) => !cancelled && setMachines(m))
        .catch(() => {});
    };
    tick();
    const t = setInterval(tick, 15000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [signedIn, ns]);

  // Global shortcuts: ⌘/Ctrl+K or "/" palette, ⌘/Ctrl+J assistant, Esc closes.
  useEffect(() => {
    if (!signedIn) return;
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey;
      const typing = /^(INPUT|TEXTAREA|SELECT)$/.test((e.target as HTMLElement)?.tagName) || (e.target as HTMLElement)?.isContentEditable;
      if (mod && e.key.toLowerCase() === 'k') (e.preventDefault(), setPalette((p) => !p));
      else if (mod && e.key.toLowerCase() === 'j') (e.preventDefault(), setPage('assistant'));
      else if (e.key === '/' && !typing && !mod) (e.preventDefault(), setPalette(true));
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [signedIn, setPage]);

  const entries = useMemo<PaletteEntry[]>(() => {
    const pages = pageEntries(setPage);
    const ms = machines.map<PaletteEntry>((m) => ({
      key: 'machine:' + m.metadata.namespace + '/' + m.metadata.name,
      group: 'Machines',
      label: m.metadata.name,
      hint: m.status?.phase ?? 'Unknown',
      tone: 'violet',
      run: () => {
        window.location.hash = formatHash({ page: 'machines', target: `${m.metadata.namespace}/${m.metadata.name}` });
      },
    }));
    const actions: PaletteEntry[] = [
      {
        key: 'action:new-machine',
        group: 'Actions',
        label: 'New machine',
        hint: 'Create a VM',
        tone: 'violet',
        run: () => {
          setPage('machines');
          setTimeout(() => window.dispatchEvent(new Event('kairon:new-machine')), 60);
        },
      },
    ];
    return [...actions, ...pages, ...ms];
  }, [machines, setPage]);

  function goMigrate(machine: string) {
    setPrefillMachine(machine);
    setPage('migrations');
  }
  function goSnapshot(machine: string) {
    setPrefillMachine(machine);
    setPage('snapshots');
  }
  function goRestore(snapshot: string) {
    setPrefillSnapshot(snapshot);
    setPage('restores');
  }

  async function signOut() {
    await logout().catch(() => {});
    setSignedIn(false);
  }

  if (onOIDCCallback) {
    return (
      <OIDCCallback
        onDone={() => {
          window.history.replaceState(null, '', '/');
          setSignedIn(true);
          setOnOIDCCallback(false);
        }}
      />
    );
  }

  if (!signedIn) {
    return <Login onSignedIn={() => setSignedIn(true)} />;
  }

  const body = {
    overview: <Overview />,
    fleet: <Fleet />,
    machines: <Machines onMigrate={goMigrate} onSnapshot={goSnapshot} />,
    migrations: <Migrations prefillMachine={prefillMachine} />,
    snapshots: <Snapshots prefillMachine={prefillMachine} onRestore={goRestore} />,
    restores: <Restores prefillSnapshot={prefillSnapshot} />,
    quotas: <Quotas />,
    'disruption-budgets': <DisruptionBudgets />,
    machinesets: <MachineSets />,
    instancetypes: <InstanceTypes />,
    'migration-policies': <MigrationPolicies />,
    'snapshot-schedules': <SnapshotSchedules />,
    'network-policies': <NetworkPolicies />,
    'security-groups': <SecurityGroups />,
    nodes: <Nodes />,
    system: <System />,
    storage: <Storage />,
    pools: <Pools />,
    images: <Images />,
    backups: <Backups />,
    assistant: <Assistant />,
    account: <Account />,
  }[page];

  const group = groupOf(page);
  const item = group && (group.items as readonly NavItem[]).find((i) => i.id === page);
  const title = page === 'account' ? 'Account' : item?.label ?? 'Kairon';
  const lede = page === 'account' ? 'Your session, password and sign-out.' : item?.blurb;
  void NAV;

  return (
    <ToastProvider>
      <GlobalNav page={page} go={setPage} username={username()} health={health} onSearch={() => setPalette(true)} onSignOut={signOut} />
      {weakPassword && page !== 'account' && (
        <div className="pwbanner" role="alert">
          <span>You signed in with the default lab password. Change it before exposing this dashboard.</span>
          <button className="sm" onClick={() => setPage('account')}>Change password</button>
        </div>
      )}
      <main className="stage" key={ns}>
        <PageHero
          kicker={page === 'overview' ? 'VM orchestration · no KubeVirt · no libvirt' : group?.group ?? 'Account'}
          title={page === 'overview' ? 'Run, migrate, and snapshot VMs on FluxVM.' : title}
          lede={page === 'overview' ? 'Kairon orchestrates FluxVM hosts as Kubernetes-native Machines, with secure peer live migration and operator-attested recovery.' : lede}
          glow={page === 'overview'}
          tone={group?.tone}
        />
        {body}
      </main>
      {palette && <CommandPalette entries={entries} onClose={() => setPalette(false)} />}
    </ToastProvider>
  );
}
