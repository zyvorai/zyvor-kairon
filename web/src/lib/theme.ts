// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

export type Theme = 'dark' | 'light' | 'system';
const KEY = 'kairon_theme';

export function storedTheme(): Theme {
  try {
    const v = localStorage.getItem(KEY);
    if (v === 'dark' || v === 'light' || v === 'system') return v;
  } catch {
    /* private mode */
  }
  return 'light';
}

export function resolveTheme(t: Theme, prefersDark: boolean): 'dark' | 'light' {
  return t === 'system' ? (prefersDark ? 'dark' : 'light') : t;
}

export function applyTheme(t: Theme) {
  const prefersDark = typeof window !== 'undefined' && window.matchMedia?.('(prefers-color-scheme: dark)').matches;
  document.documentElement.dataset.theme = resolveTheme(t, !!prefersDark);
  try {
    localStorage.setItem(KEY, t);
  } catch {
    /* ignore */
  }
}
