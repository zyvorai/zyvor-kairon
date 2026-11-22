// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Count-up animation for KPI numbers (zorvia's useCountUp). The easing and
// frame maths are pure so they can be unit-tested without a DOM.

export function easeOutCubic(t: number): number {
  const c = Math.min(1, Math.max(0, t));
  return 1 - Math.pow(1 - c, 3);
}

export function countUpValue(from: number, to: number, elapsedMs: number, durationMs: number): number {
  if (durationMs <= 0) return to;
  return Math.round(from + (to - from) * easeOutCubic(elapsedMs / durationMs));
}
