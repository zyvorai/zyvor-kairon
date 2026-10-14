// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { approvalIDOf } from './AgentSessions';

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
