// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// @novnc/novnc ships no TypeScript types; this declares just the surface
// web/src/pages/Console.tsx actually uses.
declare module '@novnc/novnc' {
  export default class RFB extends EventTarget {
    constructor(
      target: HTMLElement,
      urlOrChannel: string,
      options?: {
        credentials?: { username?: string; password?: string; target?: string };
        shared?: boolean;
        wsProtocols?: string[];
      }
    );
    viewOnly: boolean;
    scaleViewport: boolean;
    resizeSession: boolean;
    clipViewport: boolean;
    background: string;
    disconnect(): void;
    sendCtrlAltDel(): void;
    focus(): void;
    blur(): void;
  }
}
