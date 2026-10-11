// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { describe, it, expect } from 'vitest';
import { undoFor } from './undo';

describe('undoFor', () => {
  it('reverses pause, resume and stop', () => {
    expect(undoFor('pause')).toBe('resume');
    expect(undoFor('resume')).toBe('pause');
    expect(undoFor('stop')).toBe('start');
  });
  it('has no one-click reverse for start or halt', () => {
    expect(undoFor('start')).toBeUndefined();
    expect(undoFor('halt')).toBeUndefined();
  });
});
