// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect, vi } from 'vitest';
import { machineVerbEntries } from './CommandPalette';
import type { Machine } from '../types';

const vm = (name: string, phase: string): Machine => ({
  metadata: { name, namespace: 'default' },
  spec: { image: { path: '' }, resources: { cpu: '1', memory: '1Gi' } },
  status: { phase },
});

describe('machineVerbEntries', () => {
  it('offers migrate and snapshot only for Running machines', () => {
    const e = machineVerbEntries([vm('a', 'Running'), vm('b', 'Stopped')], () => {}, () => {});
    expect(e.map((x) => x.label)).toEqual(['Migrate a', 'Snapshot a']);
  });
  it('routes each verb to its handler', () => {
    const mig = vi.fn();
    const snap = vi.fn();
    const [m, s] = machineVerbEntries([vm('a', 'Running')], mig, snap);
    m.run();
    s.run();
    expect(mig).toHaveBeenCalledWith('a');
    expect(snap).toHaveBeenCalledWith('a');
  });
});
