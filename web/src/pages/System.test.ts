// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { podStatus } from './System';

describe('podStatus', () => {
  const base = { namespace: 'n', name: 'p', ready: true, restarts: 0 };
  it('reports Running only when ready', () => {
    expect(podStatus({ ...base, phase: 'Running' })).toBe('Running');
    expect(podStatus({ ...base, phase: 'Running', ready: false })).toBe('NotReady');
  });
  it('passes other phases through', () => {
    expect(podStatus({ ...base, phase: 'Pending', ready: false })).toBe('Pending');
  });
});
