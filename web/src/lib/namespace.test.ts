// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { withNamespace } from './namespace';

describe('withNamespace', () => {
  it('stamps the namespace on namespaced collections', () => {
    expect(withNamespace('/api/v1/machines', 'prod')).toBe('/api/v1/machines?namespace=prod');
    expect(withNamespace('/api/v1/snapshot-schedules', 'prod')).toBe('/api/v1/snapshot-schedules?namespace=prod');
    expect(withNamespace('/api/v1/usage.csv', 'prod')).toBe('/api/v1/usage.csv?namespace=prod');
  });

  it('keeps other query parameters and does not override an explicit namespace', () => {
    expect(withNamespace('/api/v1/machines?limit=5', 'prod')).toBe('/api/v1/machines?limit=5&namespace=prod');
    expect(withNamespace('/api/v1/usage.csv?namespace=staging', 'prod')).toBe('/api/v1/usage.csv?namespace=staging');
  });

  it('leaves cluster-scoped and path-scoped routes alone', () => {
    for (const p of [
      '/api/v1/nodes',
      '/api/v1/overview',
      '/api/v1/namespaces',
      '/api/v1/images',
      '/api/v1/auth/config',
      '/api/v1/machines/default/web-1',
      '/api/v1/machines/default/web-1/stop',
      '/api/v1/fleet/hapolicies?namespace=x',
      '/healthz',
    ]) {
      expect(withNamespace(p, 'prod')).toBe(p);
    }
  });
});

describe('withNamespace all-namespaces view', () => {
  it('lists every namespace for machines GETs only', () => {
    expect(withNamespace('/api/v1/machines', 'prod', { all: true })).toBe('/api/v1/machines?namespace=_all');
    expect(withNamespace('/api/v1/machines', 'prod', { all: true, method: 'get' })).toBe('/api/v1/machines?namespace=_all');
  });

  it('keeps creates and other collections in the concrete namespace', () => {
    expect(withNamespace('/api/v1/machines', 'prod', { all: true, method: 'POST' })).toBe('/api/v1/machines?namespace=prod');
    expect(withNamespace('/api/v1/migrations', 'prod', { all: true })).toBe('/api/v1/migrations?namespace=prod');
    expect(withNamespace('/api/v1/snapshots', 'prod', { all: true })).toBe('/api/v1/snapshots?namespace=prod');
  });

  it('is off by default', () => {
    expect(withNamespace('/api/v1/machines', 'prod', { all: false })).toBe('/api/v1/machines?namespace=prod');
  });
});
