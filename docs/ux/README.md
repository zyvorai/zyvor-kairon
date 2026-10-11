# Dashboard screenshots, clips and diagrams

Everything the README and the GitHub Pages gallery show of the dashboard is
generated from the real `web/` build against a **sample-data stack**, not from a
production or lab cluster. Captions say so.

| Path | What |
|---|---|
| `day-*.png`, `night-*.png` | Page screenshots (1440x900), light and dark theme |
| `clips/*.gif`, `clips/*.webm` | Short interaction clips (bulk Stop, namespace switch + filter) |
| `anim/hero.gif` | Tour built from the day shots |
| `anim/*.svg` | Hand-written animated diagrams: how it works, Machine lifecycle, live migration, no pod per VM |
| `sample-api.go` | Fake Kubernetes API with sample Kairon objects (in-memory; power actions take effect) |
| `capture.mjs` | Playwright script that signs in, walks the pages and records the clips |
| `build-hero-gif.sh` | Encodes `anim/hero.gif` (needs `ffmpeg`) |

## Regenerate

```bash
# 1. build the dashboard and the UI server
(cd web && npm ci && npm run build)
go build -o /tmp/kairon-ui ./cmd/kairon-ui

# 2. sample API + dashboard (loopback only; the password is only for this stack)
go run docs/ux/sample-api.go -listen 127.0.0.1:18999 &
KAIRON_KUBE_URL=http://127.0.0.1:18999 \
KAIRON_UI_DEFAULT_ADMIN_PASSWORD=sample-stack-2026 \
KAIRON_UI_SESSION_SECRET=$(head -c 24 /dev/urandom | base64) \
  /tmp/kairon-ui -listen 127.0.0.1:18082 -web-dir web/dist &

# 3. capture (Playwright's Chromium; or CHROME=/path/to/chrome)
npm i --no-save playwright-core
node docs/ux/capture.mjs            # shots + clips; `shots` or `clips` for one
docs/ux/build-hero-gif.sh
```

Restart `sample-api.go` between runs: it keeps power state in memory, so a
second run would start with Machines already stopped.

Keep the set small (about 5 MB): add a page to `PAGES` in `capture.mjs` only
when the README or gallery uses it. Re-run after dashboard UI changes.
