// Copyright 2026 Zyvor · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0
//
// Changes the visibility of GHCR container packages through the GitHub web UI.
// GitHub has no REST/gh endpoint for package visibility, so this drives the
// "Package settings -> Danger Zone -> Change visibility" dialog in a browser
// window that YOU sign in to. It never reads or stores credentials or cookies;
// the sign-in lives in a dedicated browser profile directory on your machine.
//
//   npm i --no-save playwright-core          # outside the repo, or --no-save here
//   node scripts/ghcr-set-visibility.mjs --dry-run
//   node scripts/ghcr-set-visibility.mjs                       # the six Kairon packages -> public
//   node scripts/ghcr-set-visibility.mjs --package kairon-ui --visibility private
//
// Defaults are deliberately narrow: only the Kairon packages. Anything else must
// be named with --package, or pass --all-org-packages to include every container
// package in the org (needs `gh` and read:packages). Making a package public is
// effectively irreversible, so the script lists what it will change and asks you
// to type "yes" unless --yes is given.
//
// Requires: the org must allow public packages (Org settings -> Packages ->
// Package creation -> Public), and you need admin rights on each package.
//
// Options:
//   --org NAME             organization (default zyvorai)
//   --visibility V         public | private | internal (default public)
//   --package NAME         package to change; repeatable (e.g. charts/kairon)
//   --all-org-packages     every container package in the org (see above)
//   --dry-run              print the plan only; open no browser
//   --yes                  skip the confirmation prompt
//   --profile DIR          browser profile dir (default ~/.cache/kairon-ghcr-profile)
//   CHROME=/path/to/chrome overrides the browser executable.

import { execFileSync } from 'node:child_process';
import { createInterface } from 'node:readline/promises';
import { homedir } from 'node:os';
import { join } from 'node:path';

const KAIRON_PACKAGES = [
  'charts/kairon',
  'kairon-controller',
  'kairon-node',
  'kairon-ui',
  'kairon-csi-node',
  'kairon-csi-controller',
];
const VISIBILITIES = ['public', 'private', 'internal'];

function parseArgs(argv) {
  const o = {
    org: 'zyvorai',
    visibility: 'public',
    packages: [],
    allOrg: false,
    dryRun: false,
    yes: false,
    profile: join(homedir(), '.cache', 'kairon-ghcr-profile'),
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const next = () => {
      if (i + 1 >= argv.length) die(`${a} needs a value`);
      return argv[++i];
    };
    if (a === '--org') o.org = next();
    else if (a === '--visibility') o.visibility = next();
    else if (a === '--package') o.packages.push(next());
    else if (a === '--all-org-packages') o.allOrg = true;
    else if (a === '--dry-run') o.dryRun = true;
    else if (a === '--yes') o.yes = true;
    else if (a === '--profile') o.profile = next();
    else if (a === '-h' || a === '--help') {
      console.log('See the header of scripts/ghcr-set-visibility.mjs for usage.');
      process.exit(0);
    } else die(`unknown option ${a}`);
  }
  if (!VISIBILITIES.includes(o.visibility)) die(`--visibility must be one of ${VISIBILITIES.join(', ')}`);
  return o;
}

function die(msg) {
  console.error(`ghcr-set-visibility: ${msg}`);
  process.exit(2);
}

// Every container package in the org, via the (read-only) packages API.
function listOrgPackages(org) {
  const out = execFileSync(
    'gh',
    ['api', `orgs/${org}/packages?package_type=container`, '--paginate', '--jq', '.[].name'],
    { encoding: 'utf8' },
  );
  return out.split('\n').map((s) => s.trim()).filter(Boolean);
}

const settingsURL = (org, pkg) =>
  `https://github.com/orgs/${org}/packages/container/${encodeURIComponent(pkg)}/settings`;

async function ask(question) {
  const rl = createInterface({ input: process.stdin, output: process.stdout });
  try {
    return (await rl.question(question)).trim();
  } finally {
    rl.close();
  }
}

// GitHub serves a 404 (not a login redirect) for org pages to signed-out
// visitors, so check the page's own user-login meta tag instead of the URL.
const signedInUser = (page) =>
  page.locator('meta[name="user-login"]').getAttribute('content').catch(() => '');

async function ensureSignedIn(page, org) {
  await page.goto('https://github.com/', { waitUntil: 'domcontentloaded' });
  if (!(await signedInUser(page))) {
    await page.goto('https://github.com/login', { waitUntil: 'domcontentloaded' });
    console.log('Sign in to GitHub in the browser window; waiting up to 5 minutes...');
    const deadline = Date.now() + 5 * 60_000;
    for (;;) {
      await page.waitForTimeout(2000);
      if (Date.now() > deadline) throw new Error('timed out waiting for GitHub sign-in');
      if (!page.url().startsWith('https://github.com/')) continue;
      if (await signedInUser(page)) break;
    }
  }
  console.log(`signed in as ${await signedInUser(page)}`);
  await page.goto(`https://github.com/orgs/${org}/packages`, { waitUntil: 'domcontentloaded' });
}

// Returns "changed", "already", or throws with a reason.
// Escapes a string for literal use inside a RegExp.
const escapeRegExp = (s) => String(s).replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

async function setVisibility(page, org, pkg, visibility) {
  // visibility is interpolated into regular expressions below; accept only the
  // values GitHub knows, and escape it anyway.
  if (!VISIBILITIES.includes(visibility)) {
    throw new Error(`visibility must be one of ${VISIBILITIES.join(', ')} (got ${JSON.stringify(visibility)})`);
  }
  const vis = escapeRegExp(visibility);
  await page.goto(settingsURL(org, pkg), { waitUntil: 'domcontentloaded' });
  const change = page.getByRole('button', { name: 'Change visibility' });
  await change.waitFor({ state: 'visible', timeout: 15_000 }).catch(() => {
    throw new Error('no "Change visibility" button (package missing or you are not an admin of it)');
  });
  const current = page.getByText(/This package is currently (public|private|internal)/i).first();
  const text = (await current.textContent().catch(() => '')) || '';
  if (new RegExp(`currently ${vis}`, 'i').test(text)) return 'already';

  await change.click();
  const radio = page.getByRole('radio', { name: new RegExp(`^${vis}`, 'i') });
  await radio.waitFor({ state: 'visible', timeout: 10_000 });
  if (await radio.isDisabled()) {
    throw new Error(`"${visibility}" is disabled by organization administrators (Org settings -> Packages -> Package creation)`);
  }
  await radio.check();
  await page.getByRole('textbox', { name: /type .* to confirm/i }).fill(pkg);
  await page.getByRole('button', { name: /I understand the consequences/i }).click();
  await page.waitForLoadState('domcontentloaded');
  await page
    .getByText(new RegExp(`currently ${vis}`, 'i'))
    .first()
    .waitFor({ state: 'visible', timeout: 15_000 })
    .catch(() => {
      throw new Error('confirmed, but the page did not report the new visibility; check it manually');
    });
  return 'changed';
}

async function main() {
  const o = parseArgs(process.argv.slice(2));
  let packages = o.packages.length ? o.packages : KAIRON_PACKAGES;
  if (o.allOrg) packages = [...new Set([...packages, ...listOrgPackages(o.org)])].sort();

  console.log(`org: ${o.org}\ntarget visibility: ${o.visibility}\npackages (${packages.length}):`);
  for (const p of packages) console.log(`  ${p}`);
  if (o.dryRun) {
    console.log('dry run: nothing changed');
    return;
  }
  if (!o.yes) {
    const extra = packages.filter((p) => !KAIRON_PACKAGES.includes(p));
    if (extra.length) console.log(`\nNOTE: ${extra.length} package(s) are not Kairon packages.`);
    if (o.visibility === 'public') console.log('Making a package public cannot be undone without GitHub support.');
    const answer = await ask(`\nType "yes" to set ${packages.length} package(s) to ${o.visibility}: `);
    if (answer !== 'yes') die('aborted');
  }

  const { chromium } = await import('playwright-core').catch(() => die('playwright-core is required: npm i --no-save playwright-core'));
  const context = await chromium.launchPersistentContext(o.profile, {
    executablePath: process.env.CHROME || undefined,
    headless: false,
    viewport: { width: 1280, height: 900 },
  });
  const failures = [];
  try {
    const page = context.pages()[0] || (await context.newPage());
    await ensureSignedIn(page, o.org);
    for (const pkg of packages) {
      try {
        const r = await setVisibility(page, o.org, pkg, o.visibility);
        console.log(`${r === 'changed' ? 'changed' : 'unchanged'}  ${pkg} -> ${o.visibility}`);
      } catch (e) {
        failures.push(pkg);
        console.error(`FAILED     ${pkg}: ${e.message}`);
      }
    }
  } finally {
    await context.close();
  }
  if (failures.length) {
    console.error(`\n${failures.length} package(s) failed: ${failures.join(', ')}`);
    process.exit(1);
  }
}

await main();
