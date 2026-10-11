// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

export interface Point {
  t: number; // ms since epoch
  v: number;
}

// linePath maps points into an SVG path inside a w×h box with a 2px inset.
// A flat or single-point series draws a centred horizontal line rather than
// dividing by zero.
export function linePath(points: Point[], w: number, h: number): string {
  if (points.length === 0) return '';
  const pad = 2;
  const t0 = points[0].t;
  const t1 = points[points.length - 1].t;
  const vs = points.map((p) => p.v);
  const lo = Math.min(...vs);
  const hi = Math.max(...vs);
  const x = (t: number) => (t1 === t0 ? w / 2 : pad + ((t - t0) / (t1 - t0)) * (w - 2 * pad));
  const y = (v: number) => (hi === lo ? h / 2 : h - pad - ((v - lo) / (hi - lo)) * (h - 2 * pad));
  const pts = points.length === 1 ? [points[0], points[0]] : points;
  return pts
    .map((p, i) => `${i === 0 ? 'M' : 'L'}${(points.length === 1 ? (i === 0 ? pad : w - pad) : x(p.t)).toFixed(1)},${y(p.v).toFixed(1)}`)
    .join(' ');
}
