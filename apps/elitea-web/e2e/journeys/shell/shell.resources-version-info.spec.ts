/**
 * Shell: Help Center — the version-info tooltip (onetest ELITEA-0971, #892).
 *
 * Split out of `shell.resources.spec.ts` deliberately, not merged into it:
 * this is the one Help Center journey that writes the shared platform-wide
 * `resources` admin-config section (`resources_information_version`, to give
 * the version-info icon something to gate on), so it must run in the
 * `platform-flags`/`platform-flags-webkit` projects (`playwright.config.ts`'s
 * `PLATFORM_FLAG_JOURNEYS`, enforced by `scripts/e2e-journey-shape.test.mjs`'s
 * #957 rule). Folding it into `shell.resources.spec.ts` would drag that
 * file's other ten, purely-read journeys into the same single-worker,
 * serial project for no reason of their own.
 *
 * ## What this proves, and what it does not
 *
 * `ELITEA-0971` (the manual case) expected the tooltip to list six named
 * Pylon plugins. That was never true of this service and the fix must not
 * make it look true: `useResourcesConfig.ts` hardcoded `components: []`
 * because `GET /admin/system_info/prompt_lib` answered 501 unconditionally,
 * and issue #219 is the reason it did — this service loads no plugins and has
 * no Arbiter bus to ask about other processes. #892 closes the OTHER half of
 * that 501: the two real, local facts this service CAN report about itself
 * (its own build version, and the highest applied shared-scope migration).
 * This test asserts exactly that shape, and separately asserts none of the
 * six Pylon names reappear — the regression #219 guards against.
 *
 * `hasVersion` (`ResourceVersionInfo.tsx`) gates the icon on the Information
 * card's admin-configured version label, which this stack does not set by
 * default. That half of the original gap is unrelated to #892 and already has
 * its own owner and its own round-trip proof:
 * `admin.resources-help-center.spec.ts`'s ELITEA-0025. This file writes the
 * SAME field the SAME way (lock, write, restore) rather than inventing a
 * second mechanism for it — purely so the icon renders at all.
 */
import { expect, test, type APIRequestContext } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { withPlatformFlagLock } from '../../fixtures/platformFlags';

test.use({ storageState: STORAGE_STATE.admin });

const HELP_CENTER_PAGE = `${BASE_URL}/app/help-center`;
const RESOURCES_SECTION_URL = `${BASE_URL}/api/v2/admin/plugin_config_values/administration/resources`;

async function putResourcesInformationVersion(
  request: APIRequestContext,
  version: string,
): Promise<{ status: number; body: string }> {
  const response = await request.put(RESOURCES_SECTION_URL, {
    data: { values: { resources_information_version: version } },
  });
  return { status: response.status(), body: await response.text() };
}

// Every Pylon plugin name the ELITEA-0971 manual case pinned. `admin` is
// distinct from `elitea-main` (the real component this fix reports) and both
// must be checked so a partial-name match cannot hide either bug.
const PYLON_PLUGIN_NAMES = ['elitea_core', 'admin', 'notifications', 'configurations', 'sdk_plugin', 'indexer_worker'];

test('RES07: the (i) icon lists this service’s own real component versions on hover', async ({ page, request }) => {
  // Same lock arithmetic as admin.resources-help-center.spec.ts's ELITEA-0025:
  // up to STALE_MS (90s) to take the writer, up to STALE_MS again for readers
  // to drain, then time for the assertions themselves.
  test.setTimeout(210_000);
  const probeVersion = `E2E-RES07-${Date.now()}`;

  await withPlatformFlagLock(async () => {
    try {
      const saved = await putResourcesInformationVersion(request, probeVersion);
      expect(saved.status, saved.body).toBe(200);

      await page.goto(HELP_CENTER_PAGE, { waitUntil: 'domcontentloaded' });
      const icon = page.getByTestId('resource-version-info-icon');
      await expect(icon, 'the version info icon must be visible once a version label is configured').toBeVisible({
        timeout: 20_000,
      });
      await icon.hover();

      const elitea = await page.getByText(/^elitea-main: /).textContent({ timeout: 5_000 });
      expect(elitea, 'the tooltip must report this binary’s own version, not a Pylon plugin name').toMatch(
        /^elitea-main: \S+$/,
      );
      const migrations = await page.getByText(/^migrations: /).textContent({ timeout: 5_000 });
      expect(migrations, 'the tooltip must report a real 4-digit migration head').toMatch(/^migrations: \d{4}$/);

      // The #219 regression this fix must never reintroduce.
      for (const name of PYLON_PLUGIN_NAMES) {
        await expect(page.getByText(new RegExp(`^${name}: `)), `must not fake the ${name} plugin (#219)`).toHaveCount(
          0,
        );
      }
    } finally {
      const restored = await putResourcesInformationVersion(request, '');
      expect(restored.status, 'the probe version must be restored for every other journey').toBe(200);
    }
  });
});
