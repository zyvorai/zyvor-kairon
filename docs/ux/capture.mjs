// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
//
// Captures the dashboard screenshots in docs/ux/ (day-*.png, night-*.png) and
// the short interaction clips in docs/ux/clips/ (WebM) from a running
// kairon-ui backed by docs/ux/sample-api.go. See docs/ux/README.md.
//
//   npm i --no-save playwright-core        # outside the repo, or --no-save here
//   node docs/ux/capture.mjs               # BASE=http://127.0.0.1:18082 by default
//
// CHROME=/path/to/chrome overrides the browser; otherwise Playwright's own
// Chromium is used.

import { chromium } from 'playwright-core';
import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const BASE = process.env.BASE || 'http://127.0.0.1:18082';
const USER = process.env.KAIRON_UI_USER || 'admin';
const PASS = process.env.KAIRON_UI_PASSWORD || 'sample-stack-2026'; // matches docs/ux/README.md; sample stack only
const W = 1440;
const H = 900;

// [file stem, hash route, optional action run before the shot]
const PAGES = [
  ['00-overview', '#/overview'],
  ['01-machines', '#/machines/default/db-primary'],
  ['02-migrations', '#/migrations'],
  ['03-snapshots', '#/snapshots'],
  ['04-nodes', '#/nodes'],
  ['05-network-policies', '#/network-policies'],
  ['06-pools', '#/pools'],
  ['07-backups', '#/backups'],
  ['08-quotas', '#/quotas'],
];

const launch = () =>
  chromium.launch({ executablePath: process.env.CHROME || undefined, headless: true });

async function signIn(page) {
  await page.goto(BASE + '/');
  await page.waitForSelector('input[name=password]');
  await page.fill('input[name=username]', USER);
  await page.fill('input[name=password]', PASS);
  await page.click('button[type=submit]');
  await page.waitForSelector('.stage', { timeout: 15000 });
}

async function settle(page, ms = 900) {
  await page.waitForLoadState('networkidle').catch(() => {});
  await page.waitForTimeout(ms); // count-up animations and the 5s poll's first paint
}

async function shots(theme) {
  const browser = await launch();
  const ctx = await browser.newContext({ viewport: { width: W, height: H }, deviceScaleFactor: 1 });
  await ctx.addInitScript((t) => localStorage.setItem('kairon_theme', t), theme);
  const page = await ctx.newPage();
  await signIn(page);
  for (const [stem, hash, act] of PAGES) {
    await page.evaluate((h) => { location.hash = h; }, hash);
    await settle(page);
    if (act) await act(page);
    await page.screenshot({ path: join(HERE, `${theme === 'dark' ? 'night' : 'day'}-${stem}.png`) });
    console.log(theme, stem);
  }
  // All-namespaces Machines view with a bulk selection.
  await page.evaluate(() => {
    localStorage.setItem('kairon-all-namespaces', 'true');
    location.hash = '#/machines';
    location.reload();
  });
  await page.waitForSelector('.stage');
  await settle(page, 1200);
  const boxes = page.locator('tbody input[type=checkbox]');
  await boxes.nth(0).check();
  await boxes.nth(1).check();
  await boxes.nth(3).check();
  await settle(page, 300);
  await page.screenshot({ path: join(HERE, `${theme === 'dark' ? 'night' : 'day'}-09-machines-all-namespaces.png`) });
  console.log(theme, '09-machines-all-namespaces');
  await browser.close();
}

// A short clip: Playwright's built-in video needs its own ffmpeg build, so frames
// are grabbed with page.screenshot() while the scenario runs and encoded with the
// system ffmpeg into a WebM and a GIF. Roughly 8 fps, which is plenty for a UI.
async function clip(name, run) {
  const dir = join(HERE, 'clips');
  const frames = mkdtempSync(join(tmpdir(), 'kairon-clip-'));
  mkdirSync(dir, { recursive: true });
  const browser = await launch();
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await ctx.addInitScript(() => localStorage.setItem('kairon_theme', 'light'));
  const page = await ctx.newPage();
  await signIn(page);
  let n = 0;
  let live = true;
  const t0 = Date.now();
  const grab = (async () => {
    while (live) {
      await page.screenshot({ path: join(frames, `f${String(n++).padStart(5, '0')}.jpg`), type: 'jpeg', quality: 82 });
    }
  })();
  await run(page);
  live = false;
  await grab;
  const fps = Math.max(2, Math.round(n / ((Date.now() - t0) / 1000)));
  await browser.close();
  const input = ['-y', '-loglevel', 'error', '-framerate', String(fps), '-i', join(frames, 'f%05d.jpg')];
  execFileSync('ffmpeg', [...input, '-c:v', 'libvpx-vp9', '-b:v', '0', '-crf', '38', '-pix_fmt', 'yuv420p', join(dir, `${name}.webm`)]);
  execFileSync('ffmpeg', [...input, '-vf', 'scale=960:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=128[p];[b][p]paletteuse=dither=bayer:bayer_scale=3', '-loop', '0', join(dir, `${name}.gif`)]);
  rmSync(frames, { recursive: true, force: true });
  console.log('clip', name, `${n} frames @ ${fps}fps`);
}

const what = process.argv[2] || 'all';
if (what === 'all' || what === 'shots') {
  await shots('light');
  await shots('dark');
}
if (what === 'all' || what === 'clips') {
  await clip('bulk-stop', async (page) => {
    await page.evaluate(() => { location.hash = '#/machines'; });
    await settle(page, 1200);
    page.on('dialog', (d) => d.accept());
    const boxes = page.locator('tbody input[type=checkbox]');
    for (const i of [0, 1, 2]) {
      await boxes.nth(i).check();
      await page.waitForTimeout(450);
    }
    await page.getByRole('button', { name: 'Stop', exact: true }).first().click();
    await page.waitForTimeout(3500);
  });
  await clip('namespaces', async (page) => {
    await page.evaluate(() => { location.hash = '#/machines'; });
    await settle(page, 1200);
    const sel = page.locator('select').first();
    await sel.selectOption({ label: 'prod' }).catch(() => {});
    await page.waitForTimeout(1800);
    await sel.selectOption({ index: 0 }).catch(() => {});
    await page.waitForTimeout(1500);
    const filter = page.getByLabel('Filter rows');
    if (await filter.count()) {
      await filter.type('db', { delay: 160 });
      await page.waitForTimeout(1500);
    }
  });
}
