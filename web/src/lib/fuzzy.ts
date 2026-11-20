// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Tiny subsequence scorer for the command palette. Higher is better;
// null means "no match". Consecutive and word-start hits score more, and
// shorter candidates win ties.
export function fuzzyScore(query: string, text: string): number | null {
  const q = query.trim().toLowerCase();
  if (!q) return 0;
  const t = text.toLowerCase();
  let ti = 0;
  let score = 0;
  let run = 0;
  for (const ch of q) {
    const idx = t.indexOf(ch, ti);
    if (idx === -1) return null;
    const wordStart = idx === 0 || /[\s\-_/]/.test(t[idx - 1]);
    run = idx === ti ? run + 1 : 1;
    score += 1 + run * 2 + (wordStart ? 3 : 0);
    ti = idx + 1;
  }
  return score - t.length * 0.01;
}

export function fuzzyFilter<T>(items: T[], query: string, textOf: (i: T) => string): T[] {
  if (!query.trim()) return items;
  return items
    .map((i) => ({ i, s: fuzzyScore(query, textOf(i)) }))
    .filter((x): x is { i: T; s: number } => x.s !== null)
    .sort((a, b) => b.s - a.s)
    .map((x) => x.i);
}
