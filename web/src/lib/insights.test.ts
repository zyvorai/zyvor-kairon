// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { insightCounts, insightHref, Insight } from './insights';

const base: Insight = { id: 'x', severity: 'warning', title: 't', page: 'machines', target: 'prod/web 1' };

describe('insights', () => {
  it('links to the page and encoded target', () => {
    expect(insightHref(base)).toBe('#/machines/prod/web%201');
  });
  it('falls back to overview for unknown pages', () => {
    expect(insightHref({ ...base, page: 'nope', target: undefined })).toBe('#/overview');
  });
  it('counts by severity', () => {
    expect(insightCounts([base, { ...base, severity: 'critical' }, { ...base, severity: 'critical' }])).toEqual({ critical: 2, warning: 1 });
  });
});
