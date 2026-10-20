// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { isPage, Page } from './nav';

// Hash routing: #/machines, #/machines/default/web01 (inspector target).
// No router dependency -- the dashboard has ~20 flat pages.

export interface Route {
  page: Page;
  // Optional "namespace/name" the page should open in its inspector.
  target?: string;
}

// A malformed escape (a bare '%') must not throw during render.
function safeDecode(s: string): string {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

export function parseHash(hash: string): Route {
  const raw = hash.replace(/^#\/?/, '');
  const [head, ...rest] = raw.split('/').filter(Boolean);
  if (head && isPage(head)) {
    return { page: head, target: rest.length ? rest.map(safeDecode).join('/') : undefined };
  }
  return { page: 'overview' };
}

export function formatHash(r: Route): string {
  const base = `#/${r.page}`;
  if (!r.target) return base;
  return `${base}/${r.target.split('/').map(encodeURIComponent).join('/')}`;
}
