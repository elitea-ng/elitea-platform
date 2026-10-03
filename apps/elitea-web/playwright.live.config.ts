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
import { defineConfig, devices } from '@playwright/test';
import path from 'path';
import { fileURLToPath } from 'url';

import {
  LIVE_ADMIN_READONLY,
  LIVE_SAFETY_EXCLUDED,
  livePathPattern,
  liveRunRefusal,
  liveTestIgnore,
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

const IGNORED = liveTestIgnore(process.env);
const SAFETY = LIVE_SAFETY_EXCLUDED.map(livePathPattern);
const includeEnvDependent = process.env['LIVE_INCLUDE_ENV_DEPENDENT'] === '1';

/** Admin specs that only READ; the audit trail joins them only for triage (seeded rows). */
const ADMIN_READONLY = [
  ...LIVE_ADMIN_READONLY,
  ...(includeEnvDependent ? ['journeys/admin/admin.audit-trail.spec.ts'] : []),
].map(livePathPattern);

/** OPT-IN streaming lane (chat persona, its personal project, the instance's echo model).
 *  Only the streaming specs that do NOT read the mock journal or name
 *  E2E-MOCK-MODEL; chat.admin-providers is left out (it publishes PLATFORM
 *  provider credentials). Every turn here is a real worker execution — keep the
 *  run small and never point E2E_MOCK_MODEL at a real model. */
const STREAM_ALLOWLIST = [
  /streaming\/chat\.(agent|agent-tools|nested-agent|messageRefusals|project-context-injection)\.spec\.ts$/,
  /streaming\/chat\.pipeline(-authored|-execution|-multinode|-triggers)?\.spec\.ts$/,
  /streaming\/index\.lifecycle\.spec\.ts$/,
];

const chrome = { ...devices['Desktop Chrome'], launchOptions: { args: ['--no-sandbox'] } };
// Under the gitignored playwright-results/ unless the run points it elsewhere.
const OUT = process.env['LIVE_OUTPUT_DIR'] ?? path.join(__dirname, 'playwright-results', 'live');

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
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'off',
  },
  projects: [
    {
      name: 'live-api',
      testMatch: /journeys\/api\/.+\.spec\.ts$/,
      testIgnore: IGNORED,
      use: { ...chrome, storageState: state('member') },
    },
    {
      name: 'live-journeys',
      testMatch: /journeys\/.+\.spec\.ts$/,
      testIgnore: [/journeys\/api\//, /journeys\/admin\//, ...IGNORED],
      use: { ...chrome, storageState: state('member') },
    },
    {
      name: 'live-admin-readonly',
      testMatch: ADMIN_READONLY,
      testIgnore: SAFETY,
      use: { ...chrome, storageState: state('member') },
      fullyParallel: false,
    },
    {
      name: 'live-stream',
      testMatch: STREAM_ALLOWLIST,
      use: { ...chrome, storageState: state('chat') },
      fullyParallel: false,
    },
  ],
});
