// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

export type PowerAction = 'start' | 'stop' | 'pause' | 'resume' | 'halt';

// undoFor returns the power action that reverses a just-completed one, or
// undefined when there is no clean reverse. Start is not undoable with one
// click (stopping asks first), and halt has no graceful reverse.
export function undoFor(action: PowerAction): PowerAction | undefined {
  switch (action) {
    case 'pause':
      return 'resume';
    case 'resume':
      return 'pause';
    case 'stop':
      return 'start';
    default:
      return undefined;
  }
}
