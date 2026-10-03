/**
 * LIVE config — drives this repo's Playwright suites against a DEPLOYED
 * instance with dedicated regression personas, instead of the local e2e stack
 * and its OIDC mock.
 *
 *   npx playwright test -c playwright.live.config.ts --project=live-api
 *
 * Versioned here rather than written into the tree by an external harness and
 * deleted again on cleanup: what a live run selects, and why, is part of the
 * suite, and a copy that lived outside the repository drifted from the specs it
 * filtered.
 *
 * ## What the run must provide
 *
 *   PLAYWRIGHT_BASE_URL       the instance's https origin (the session cookie is Secure)
 *   E2E_STATE_DIR             storage states `member|admin|chat|viewer.json`,
 *                             minted for the personas outside this config
 *   E2E_PROJECT_ID            the regression project — never 1, which holds real data
 *   E2E_DEFAULT_PROJECT_NAME  that project's name, as the switcher shows it
 *   LIVE_ADMIN_PERSONA_SCOPE  `project` or `platform`: what `admin.json` may
 *                             administer. Specs switch THEMSELVES to it in
 *                             every lane, so this is what the run may do.
 *                             Prefer `project` (an admin of E2E_PROJECT_ID
 *                             only); `platform` adds the admin-readonly lane
 *                             and leaves the safety list as the only guard
 *                             on platform-wide writes.
 *
 * and may set
 *
 *   LIVE_TRACE_WITH_SESSION_COOKIES=1  keep failure traces. Off by default: a
 *                             trace holds the personas' live session cookies.
 *   LIVE_OUTPUT_DIR           where results go (created owner-only, 0700).
 *
 * and may provide the rest of the tenancy `e2e/fixtures/project.ts` documents
 * (`E2E_PUBLIC_PROJECT_ID`, `E2E_CATALOGUE_PROJECT_ID`,
 * `E2E_PUBLISH_AUTHOR_PROJECT_ID`, default `none` here).
 *
 * ## Differences from playwright.config.ts
 *
 *   - no `setup` project (no OIDC mock) and no webServer: nothing starts locally;
 *   - `E2E_TARGET=live`, which lets a journey skip — with a reason — a case
 *     whose prerequisite the DEPLOYMENT lacks (`e2e/fixtures/deployment.ts`);
 *   - a SAFETY exclusion list that is always applied, and an ENV-DEPENDENT list
 *     that `LIVE_INCLUDE_ENV_DEPENDENT=1` re-admits for triage — both in
 *     `scripts/lib/e2e-live-assumptions.mjs`, where a unit test checks that
 *     every entry still names a spec that exists.
 */
import { chmodSync, mkdirSync } from 'fs';
import path from 'path';
import { fileURLToPath } from 'url';

import { defineConfig, devices } from '@playwright/test';

import {
  LIVE_API_MATCH,
  LIVE_JOURNEYS_MATCH,
  LIVE_JOURNEYS_OWN_IGNORE,
  LIVE_SAFETY_EXCLUDED,
  LIVE_STREAM_ALLOWLIST,
  adminPersonaScope,
  liveAdminReadonly,
  livePathPattern,
  liveRunRefusal,
  liveTestIgnore,
  liveTraceMode,
} from './scripts/lib/e2e-live-assumptions.mjs';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const refusal = liveRunRefusal(process.env);
if (refusal !== undefined) throw new Error(refusal);

// Read by the journeys' fixtures in every worker, which inherit this process's
// environment.
process.env['E2E_TARGET'] = 'live';
process.env['E2E_PUBLISH_AUTHOR_PROJECT_ID'] ??= 'none';

const STATE_DIR = process.env['E2E_STATE_DIR'] as string;
const state = (persona: string): string => path.join(STATE_DIR, `${persona}.json`);

// Every lane below is built from the shared definitions in
// scripts/lib/e2e-live-assumptions.mjs, which `liveSelectedSpecs` mirrors so
// the unit test can scan what a live run really selects for shared-state
// writers. Change a lane there, not here.
const IGNORED = liveTestIgnore(process.env);
const SAFETY = LIVE_SAFETY_EXCLUDED.map(livePathPattern);

/** Admin specs that only READ; the audit trail joins them only for triage (seeded rows). */
const ADMIN_READONLY = liveAdminReadonly(process.env).map(livePathPattern);

/** OPT-IN streaming lane (chat persona, its personal project, the instance's echo model).
 *  Every turn here is a real worker execution — keep the run small and never
 *  point E2E_MOCK_MODEL at a real model. */
const STREAM_ALLOWLIST = [...LIVE_STREAM_ALLOWLIST];

const chrome = { ...devices['Desktop Chrome'], launchOptions: { args: ['--no-sandbox'] } };
// Under the gitignored playwright-results/ unless the run points it elsewhere.
// Owner-only: failure screenshots show the live instance's data, and with
// LIVE_TRACE_WITH_SESSION_COOKIES=1 the traces hold working persona sessions
// (the admin's included). Treat the folder and its HTML report as a secret
// until the personas' sessions are revoked; never share it before that.
const OUT = process.env['LIVE_OUTPUT_DIR'] ?? path.join(__dirname, 'playwright-results', 'live');
mkdirSync(OUT, { recursive: true, mode: 0o700 });
chmodSync(OUT, 0o700);

/** `project` scope: the admin pages the admin-readonly lane opens need the platform role it lacks. */
const adminScope = adminPersonaScope(process.env);

export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  // Gentle on a shared instance; override with LIVE_WORKERS.
  workers: Number(process.env['LIVE_WORKERS'] ?? '3'),
  retries: Number(process.env['LIVE_RETRIES'] ?? '0'),
  timeout: 120_000,
  expect: { timeout: 15_000 },
  reporter: [
    ['list'],
    ['json', { outputFile: path.join(OUT, 'results.json') }],
    ['html', { open: 'never', outputFolder: path.join(OUT, 'html') }],
  ],
  outputDir: path.join(OUT, 'artifacts'),
  snapshotDir: 'e2e/snapshots',
  use: {
    baseURL: process.env['PLAYWRIGHT_BASE_URL'] as string,
    timezoneId: process.env['E2E_TZ'] ?? 'UTC',
    // Off by default: a trace records the request Cookie header and the
    // storage state, i.e. a replayable session for the deployed instance.
    trace: liveTraceMode(process.env),
    screenshot: 'only-on-failure',
    video: 'off',
  },
  // `storageState` below is each lane's DEFAULT persona. Specs that
  // `test.use({ storageState: STORAGE_STATE.admin })` switch themselves to
  // `admin.json` in every lane — all five admin-readonly specs, and in
  // live-api / live-journeys api.credentials-toolkits, api.configurations,
  // api.chat-entity-settings, api.webhook-deliveries, api.export-import-*,
  // toolkits.generic-credential-secrets, shell.deeplink among others — and
  // then run with whatever that persona may do (LIVE_ADMIN_PERSONA_SCOPE).
  projects: [
    {
      name: 'live-api',
      testMatch: LIVE_API_MATCH,
      testIgnore: IGNORED,
      use: { ...chrome, storageState: state('member') },
    },
    {
      name: 'live-journeys',
      testMatch: LIVE_JOURNEYS_MATCH,
      testIgnore: [...LIVE_JOURNEYS_OWN_IGNORE, ...IGNORED],
      use: { ...chrome, storageState: state('member') },
    },
    // Admin specs that only READ — but they read as the ADMIN persona (each
    // spec switches to it), and they need the platform administration role,
    // so the lane exists only when the run declared that scope.
    ...(adminScope === 'platform'
      ? [
          {
            name: 'live-admin-readonly',
            testMatch: ADMIN_READONLY,
            testIgnore: SAFETY,
            use: { ...chrome, storageState: state('member') },
            fullyParallel: false,
          },
        ]
      : []),
    {
      name: 'live-stream',
      testMatch: STREAM_ALLOWLIST,
      use: { ...chrome, storageState: state('chat') },
      fullyParallel: false,
    },
  ],
});
