/**
 * CRASH-RECOVERY config — the browser half of the crash-recovery harness.
 *
 * OPT-IN ONLY. CI never loads it and `playwright.config.ts` never matches
 * `e2e/crash`. It is started by the harness, not by hand:
 *
 *   npx playwright test -c playwright.crash.config.ts
 *
 * driven by scripts/crash-recovery/crashctl.py, which owns the fault (it kills
 * Main, the Worker, ... mid-turn) and the stack. The spec only owns the browser:
 * it signs in, sends or watches one turn, keeps the page open through the
 * fault, and checks the user sees ONE answer (or one failure reference), live
 * and after a reload. No response mocks — `page.route` is not used.
 *
 * ## What the run must provide
 *
 *   PLAYWRIGHT_BASE_URL    the stack's origin, e.g. http://crash.localhost:18140
 *   CRASH_CONTROL_DIR      absolute dir for the JSON control files and artifacts
 *   CRASH_SCENARIO         scenario id
 *   CRASH_PROJECT_ID       numeric project id
 *   CRASH_CONVERSATION_ID  numeric conversation id
 *   CRASH_PROMPT           text to send (CRASH_SEND_VIA=ui)
 *
 * and may set CRASH_OIDC_SUBJECT, CRASH_SEND_VIA (`ui`|`api`),
 * CRASH_RELEASE_TIMEOUT_MS, CRASH_TEST_TIMEOUT_MS, CRASH_STATE_DIR, E2E_OIDC_PORT.
 */
import path from 'path';

import { defineConfig, devices } from '@playwright/test';

const baseURL = process.env['PLAYWRIGHT_BASE_URL'];
if (baseURL === undefined || baseURL === '') {
  throw new Error(
    'playwright.crash.config.ts needs PLAYWRIGHT_BASE_URL (the crash stack origin, ' +
      'e.g. http://crash.localhost:18140). It is set by scripts/crash-recovery/crashctl.py.',
  );
}

const controlDir = process.env['CRASH_CONTROL_DIR'];
const stateDir =
  process.env['CRASH_STATE_DIR'] ??
  (controlDir === undefined || controlDir === '' ? '.crash-state' : path.join(controlDir, '..', '.crash-state'));
export const CRASH_STORAGE_STATE = path.join(stateDir, 'crash-user.json');

export default defineConfig({
  testDir: './e2e/crash',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: Number(process.env['CRASH_TEST_TIMEOUT_MS'] ?? '1200000'),
  expect: { timeout: 15_000 },
  reporter: [['list']],
  outputDir:
    controlDir === undefined || controlDir === ''
      ? 'test-results/crash'
      : path.join(controlDir, 'browser', 'test-results'),
  use: {
    baseURL,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'off',
  },
  projects: [
    { name: 'crash-setup', testMatch: /crash\.setup\.ts/, use: { ...devices['Desktop Chrome'] } },
    {
      name: 'crash',
      testMatch: /recovery-.+\.spec\.ts/,
      dependencies: ['crash-setup'],
      use: { ...devices['Desktop Chrome'], storageState: CRASH_STORAGE_STATE },
    },
  ],
});
