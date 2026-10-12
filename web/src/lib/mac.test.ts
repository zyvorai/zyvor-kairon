// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { macDetails, platformSummary } from './mac';

const studio = {
  metadata: {
    name: 'studio-1',
    labels: {
      'kubernetes.io/arch': 'arm64',
      'kairon.zyvor.dev/backend.vz': 'true',
      'kairon.zyvor.dev/apple-chip': 'm4-max',
      'kairon.zyvor.dev/macos-version': '27',
      'kairon.zyvor.dev/vz.rosetta': 'true',
      'kairon.zyvor.dev/thunderbolt-fabric': 'tb-a',
    },
    annotations: { 'kairon.zyvor.dev/thunderbolt-interface': 'bridge0', 'kairon.zyvor.dev/thunderbolt-addresses': '169.254.20.1' },
  },
};

describe('platformSummary', () => {
  it('names a Mac by chip, macOS and fabric', () => {
    expect(platformSummary(studio)).toBe('Mac · M4 Max · macOS 27 · TB tb-a');
  });
  it('names a Linux node by architecture', () => {
    expect(platformSummary({ metadata: { name: 'n1', labels: { 'kubernetes.io/arch': 'amd64' } } })).toBe('Linux amd64');
  });
});

describe('macDetails', () => {
  it('lists Apple capabilities for a Mac only', () => {
    const rows = Object.fromEntries(macDetails(studio));
    expect(rows['Rosetta']).toBe('yes');
    expect(rows['Nested virtualization']).toBe('no');
    expect(rows['Thunderbolt']).toBe('bridge0 169.254.20.1');
    expect(macDetails({ metadata: { name: 'n1' } })).toEqual([]);
  });
});
