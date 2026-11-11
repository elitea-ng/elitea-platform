/**
 * The desktop shell's journeys (`e2e/desktop`), against its dev harness: the
 * desktop build's entry served by Vite (`--mode desktop`) and opened at
 * `/?harness`, which mounts the shell with the in-memory host fakes and canned
 * API answers (`src/entries/desktop/devHarness.tsx`). No compose stack, no
 * Tauri host, no deployment: the host's own side is D0's Rust tests.
 *
 * Kept apart from playwright.config.ts, whose webServer brings the E2E stack
 * up: these journeys need none of it. CI: ci-web.yml's `desktop-journeys`.
 *
 *   npx playwright test -c playwright.desktop.config.ts
 */
import { defineConfig, devices } from '@playwright/test';

const PORT = Number(process.env['DESKTOP_HARNESS_PORT'] ?? 5179);

export default defineConfig({
  testDir: './e2e/desktop',
  testMatch: /.+\.spec\.ts/,
  fullyParallel: true,
  forbidOnly: process.env['CI'] !== undefined,
  retries: 0,
  reporter: process.env['CI'] === undefined ? 'list' : [['list'], ['html', { open: 'never', outputFolder: 'playwright-report-desktop' }]],
  outputDir: 'test-results-desktop',
  use: {
    baseURL: `http://localhost:${String(PORT)}`,
    trace: 'retain-on-failure',
  },
  // `DESKTOP_HARNESS_CHANNEL=chrome`: an installed Chrome instead of Playwright's own Chromium (a local run without the download).
  projects: [{ name: 'desktop-harness', use: { ...devices['Desktop Chrome'], viewport: { width: 1280, height: 860 }, ...(process.env['DESKTOP_HARNESS_CHANNEL'] === undefined ? {} : { channel: process.env['DESKTOP_HARNESS_CHANNEL'] }) } }],
  webServer: {
    command: `npx vite --mode desktop --port ${String(PORT)} --strictPort`,
    url: `http://localhost:${String(PORT)}/?harness`,
    reuseExistingServer: process.env['CI'] === undefined,
    timeout: 180_000,
  },
});
