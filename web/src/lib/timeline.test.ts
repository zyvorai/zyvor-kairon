// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { relativeTime, timelineTone } from './timeline';

describe('timelineTone', () => {
  it('flags Warning events', () => {
    expect(timelineTone({ time: '', source: 'event', type: 'Warning' })).toBe('warn');
    expect(timelineTone({ time: '', source: 'event', type: 'Normal' })).toBe('idle');
  });
  it('maps condition status', () => {
    expect(timelineTone({ time: '', source: 'condition', type: 'Ready', status: 'True' })).toBe('ok');
    expect(timelineTone({ time: '', source: 'condition', type: 'Ready', status: 'False' })).toBe('error');
    expect(timelineTone({ time: '', source: 'condition', type: 'Ready', status: 'Unknown' })).toBe('warn');
  });
});

describe('relativeTime', () => {
  const now = Date.parse('2026-10-11T12:00:00Z');
  it('buckets by magnitude', () => {
    expect(relativeTime('2026-10-11T11:59:50Z', now)).toBe('just now');
    expect(relativeTime('2026-10-11T11:55:00Z', now)).toBe('5m ago');
    expect(relativeTime('2026-10-11T09:00:00Z', now)).toBe('3h ago');
    expect(relativeTime('2026-10-09T12:00:00Z', now)).toBe('2d ago');
  });
  it('hides zero and invalid times', () => {
    expect(relativeTime('0001-01-01T00:00:00Z', now)).toBe('');
    expect(relativeTime('nope', now)).toBe('');
  });
});
