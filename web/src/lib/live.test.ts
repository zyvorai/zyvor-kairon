// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { nextBackoff, parseSSE } from './live';

describe('parseSSE', () => {
  it('parses complete change events and keeps the partial tail', () => {
    const buf =
      ': connected\n\n' +
      'event: change\ndata: {"type":"ADDED","kind":"Machine","namespace":"default","name":"a","phase":"Running"}\n\n' +
      ': ping\n\n' +
      'event: change\ndata: {"type":"DELETED","kind":"Machine"';
    const { changes, rest } = parseSSE(buf);
    expect(changes).toHaveLength(1);
    expect(changes[0]).toMatchObject({ type: 'ADDED', name: 'a', phase: 'Running' });
    expect(rest).toContain('"DELETED"');
  });
  it('completes the tail once the rest arrives', () => {
    const first = parseSSE('event: change\ndata: {"type":"MODIFIED","kind":"Machine","namespace":"d","name":"b"}\n');
    expect(first.changes).toHaveLength(0);
    const second = parseSSE(first.rest + '\n');
    expect(second.changes).toHaveLength(1);
  });
  it('skips malformed json and unknown events', () => {
    const { changes } = parseSSE('event: change\ndata: {oops\n\nevent: other\ndata: {}\n\n');
    expect(changes).toHaveLength(0);
  });
  it('handles CRLF', () => {
    const { changes } = parseSSE('event: change\r\ndata: {"type":"ADDED","kind":"Machine","namespace":"d","name":"c"}\r\n\r\n');
    expect(changes).toHaveLength(1);
  });
});

describe('nextBackoff', () => {
  it('doubles and caps at 30s', () => {
    expect(nextBackoff(1000)).toBe(2000);
    expect(nextBackoff(20000)).toBe(30000);
    expect(nextBackoff(30000)).toBe(30000);
  });
});
