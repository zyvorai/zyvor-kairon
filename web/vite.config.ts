// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Dev proxy target: the kairon-ui to talk to (default https://127.0.0.1:18082;
// override with KAIRON_UI_URL=https://host:port npm run dev). kairon-ui serves
// HTTPS with a self-signed certificate unless given one, so the dev proxy does
// not verify it (loopback development only); a kairon-ui started with
// -insecure-http works with KAIRON_UI_URL=http://127.0.0.1:18082.
const uiURL = process.env.KAIRON_UI_URL || 'https://127.0.0.1:18082';

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      // ws: true so `npm run dev` also tunnels the VNC console's
      // WebSocket upgrade (GET /api/v1/machines/.../console); the string
      // shorthand form doesn't enable that.
      '/api': { target: uiURL, ws: true, secure: false },
      '/healthz': { target: uiURL, secure: false },
    },
  },
});
