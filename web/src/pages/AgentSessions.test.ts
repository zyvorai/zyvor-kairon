// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { approvalIDOf, screenPoint } from './AgentSessions';

describe('screenPoint', () => {
  it('scales a click on the shown image to screenshot pixels', () => {
    expect(screenPoint(100, 50, 512, 1024)).toEqual({ x: 200, y: 100, screen_width: 1024 });
    expect(screenPoint(10, 20, 0, 800)).toEqual({ x: 10, y: 20, screen_width: 800 });
  });
});

describe('approvalIDOf', () => {
  it('reads the approval id of a 428 body', () => {
    const body = JSON.stringify({ approval_required: true, approval_id: '11111111-2222-4333-8444-555555555555', error: 'approval required' });
    expect(approvalIDOf(new Error(body))).toBe('11111111-2222-4333-8444-555555555555');
  });
  it('ignores other errors', () => {
    expect(approvalIDOf(new Error('{"error":"denied by policy"}'))).toBe('');
    expect(approvalIDOf(new Error('Bad Gateway'))).toBe('');
    expect(approvalIDOf('x')).toBe('');
  });
});
