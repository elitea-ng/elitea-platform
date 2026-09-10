/**
 * Admin › Features › Help Center (`resources` section) — the card-count
 * inventory, the Information card's manual version field, and a card's
 * enable/disable toggle reaching the public `/help-center` page.
 *
 * Ported by use case from the w1-admin-portal package's
 * `admin-portal/resources-configuration` cases (ELITEA-0025, ELITEA-0029,
 * ELITEA-0030, ELITEA-0031). `admin.features.spec.ts`'s "Help Center round
 * trip" (J36g/J36h/J36j) already proves the identical add-link /
 * remove-link / server-refuses-`javascript:` / reaches-`/help-center`
 * mechanism for the DOCUMENTATION and TUTORIALS cards — ELITEA-0026
 * (remove a link), ELITEA-0027 (edit a link) and ELITEA-0028 (add a link)
 * are the same PUT/GET round trip over the same section and are recorded
 * COVERED-EXISTING against those journeys rather than repeated here for a
 * third and fourth card.
 *
 * What is NOT the same mechanism, and is new here:
 *  - the Information card's OWN fields (`resources_information_version`),
 *    which no existing journey touches;
 *  - the enable/disable TOGGLE, a boolean field the round trip never
 *    flips (it only ever edits links and a title).
 *
 * This file uses the RELEASE NOTES and VIDEO LIBRARY cards, deliberately
 * different from the two `admin.features.spec.ts` owns per browser project
 * (`ownedCard`: Documentation for chromium, Tutorials for webkit) — the
 * platform-flag lock already serialises every writer against this one
 * platform-wide section, but a different card keeps the two files' fixture
 * data disjoint even from a stale `finally`.
 *
 * ELITEA-0024 (environment-wide effect across projects) and ELITEA-0032
 * (Resources appears before Banner in Configuration) are NOT ported — see
 * `S/port/ledger-P10-admin-ops.tsv` / `not-applicable.md`: the "Resources
 * subsection" this platform actually built lives on the FEATURES page as
 * "Help Center" (`config_schemas.go`'s `resourcesSection`, `page:
 * configPageFeatures`), not on the Configuration page at all, and it is ONE
 * environment-wide `/help-center` route rather than a per-project "Resources
 * page" — so there is no per-project variation to prove identical, and no
 * ordering against Banner to assert.
 */
import { expect, test as adminTest, type APIRequestContext, type Page } from '@playwright/test';

import { API_BASE, AUTOTEST_PREFIX } from '../../fixtures/api';
import { withPlatformFlagLock } from '../../fixtures/platformFlags';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

const SECTION_URL = `${API_BASE}/admin/plugin_config_values/administration/resources`;
const RUN_ID = `${AUTOTEST_PREFIX}rc_${Date.now()}`;

/** The six card blocks `resourcesSection()` (`config_schemas.go`) declares — one more than the manual case's five: Interactive Tours joined the set after ELITEA-0030 was authored. */
const ALL_CARD_TITLES = [
  'Information',
  'Documentation',
  'Release Notes',
  'Video Library',
  'Tutorials',
  'Interactive Tours',
] as const;

async function putResources(
  request: APIRequestContext,
  values: Record<string, unknown>,
): Promise<{ status: number; body: string }> {
  const response = await request.put(SECTION_URL, { data: { values } });
  return { status: response.status(), body: await response.text() };
}

/** Restores the fields this file touches to the schema defaults — the Release Notes / Video Library enabled flags and the Information version/upgrade-date pair. */
async function restoreOwnedFields(request: APIRequestContext): Promise<void> {
  const deadline = Date.now() + 6_000;
  let last = { status: 0, body: 'no attempt was made' };
  for (;;) {
    last = await putResources(request, {
      resources_release_notes_enabled: true,
      resources_video_library_enabled: true,
      resources_information_version: '',
      resources_information_upgrade_date: '',
    });
    if (last.status === 200) return;
    if (Date.now() > deadline) break;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  expect(
    last.status,
    `the Help Center fields this file owns were NOT restored, so they are left changed for every other journey. Last response: ${last.status} ${last.body}`,
  ).toBe(200);
}

async function openHelpCenter(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/features', { waitUntil: 'domcontentloaded' });
  expect(response?.status(), 'the admin SPA must serve the features route, not 404').toBeLessThan(400);
  await expect(page.getByRole('switch', { name: 'Enable MCP' })).toBeVisible({ timeout: 20_000 });
  await page.getByRole('button', { name: /Help Center/ }).click();
  await expect(page.getByRole('switch', { name: 'Information Card Enabled' })).toBeVisible({ timeout: 15_000 });
}

/**
 * True only while THIS test itself is inside `withPlatformFlagLock` writing
 * the fields this file owns.
 *
 * The net below must NOT run unconditionally: `ELITEA-0030` never writes at
 * all, and an unconditional restore in its `afterEach` fired mid-run against
 * a WRITE test in another worker (`fullyParallel`), clobbering
 * `resources_release_notes_enabled` back to the default while that other
 * test's own assertion was mid-flight — measured, not hypothetical (the
 * exact failure `admin.features.spec.ts`'s own `heldPlatformFlags` guard
 * documents for the identical reason).
 */
let heldLock = false;

adminTest.beforeEach(() => {
  heldLock = false;
});

adminTest.afterEach(async ({ request }) => {
  if (!heldLock) return;
  await restoreOwnedFields(request).catch(() => undefined);
});

/* ── ELITEA-0030: all six card blocks are present ────────────────────────── */

adminTest('ELITEA-0030: every card configuration block is present on the Help Center section', async ({ page }) => {
  await openHelpCenter(page);
  for (const title of ALL_CARD_TITLES) {
    await expect(
      page.getByRole('switch', { name: `${title} Card Enabled` }),
      `the ${title} card block must be present`,
    ).toBeVisible();
  }
});

/* ── ELITEA-0025: the Information card's manual version field ───────────── */

adminTest(
  'ELITEA-0025: the Information card’s version field persists and reaches the public Help Center page',
  async ({ page, request }) => {
    heldLock = true;
    await withPlatformFlagLock(async () => {
      const probeVersion = `E2E-${RUN_ID}`;
      try {
        const saved = await putResources(request, { resources_information_version: probeVersion });
        expect(saved.status, saved.body).toBe(200);

        await openHelpCenter(page);
        const versionField = page.getByRole('textbox', { name: 'Information Card - ELITEA Version Value' });
        await expect(versionField).toBeVisible();
        await expect(versionField).toHaveValue(probeVersion);

        // Reload — the field must be seeded from the STORED value, not from
        // whatever this page happened to fetch a moment ago.
        await page.reload({ waitUntil: 'domcontentloaded' });
        await openHelpCenter(page);
        await expect(
          page.getByRole('textbox', { name: 'Information Card - ELITEA Version Value' }),
        ).toHaveValue(probeVersion);

        // …and the public, user-facing Help Center page shows it.
        await page.goto(BASE_URL + '/app/help-center', { waitUntil: 'domcontentloaded' });
        // `.first()`: `ResourceVersionInfo.tsx` renders the label twice (the
        // visible text and a `copy-version-info` clone for the copy button),
        // both showing the same string — either is proof enough.
        await expect(
          page.getByText(new RegExp(`Version:.*${probeVersion}`)).first(),
        ).toBeVisible({ timeout: 20_000 });
      } finally {
        await restoreOwnedFields(request);
      }
    });
  },
);

/* ── ELITEA-0029 + ELITEA-0031: card enable/disable persists, and reaches /help-center ── */

adminTest(
  'ELITEA-0029 + ELITEA-0031: disabling a card persists and hides it on /help-center; re-enabling restores it',
  async ({ page, request }) => {
    heldLock = true;
    await withPlatformFlagLock(async () => {
      try {
        // ── disable, save, and prove BOTH halves: the admin field's own
        // persistence (0029) and the public page's reaction (0031) ────────
        const disabled = await putResources(request, { resources_release_notes_enabled: false });
        expect(disabled.status, disabled.body).toBe(200);

        await openHelpCenter(page);
        await expect(page.getByRole('switch', { name: 'Release Notes Card Enabled' })).not.toBeChecked();

        // 0029: persists across a reload.
        await page.reload({ waitUntil: 'domcontentloaded' });
        await openHelpCenter(page);
        await expect(page.getByRole('switch', { name: 'Release Notes Card Enabled' })).not.toBeChecked();

        // 0031: the card is gone from the public page.
        await page.goto(BASE_URL + '/app/help-center', { waitUntil: 'domcontentloaded' });
        // The card TITLE is a plain `Typography`, not a heading element
        // (`ResourceCard.tsx` renders it as `variant="subtitle"` with no
        // heading role), so it is located by text, exact — a substring match
        // on "Release Notes" would also match nothing else here, but exact
        // keeps this honest against a future card whose name contains it.
        await expect(page.getByText('Release Notes', { exact: true })).toHaveCount(0, { timeout: 20_000 });
        // The control, so a `visibleCards` regression that hid every card
        // does not pass this test by accident.
        await expect(page.getByText('Video Library', { exact: true })).toBeVisible();

        // ── re-enable, and both halves reverse ──────────────────────────
        const reenabled = await putResources(request, { resources_release_notes_enabled: true });
        expect(reenabled.status, reenabled.body).toBe(200);

        await page.goto(BASE_URL + '/admin/app/features', { waitUntil: 'domcontentloaded' });
        await openHelpCenter(page);
        await expect(page.getByRole('switch', { name: 'Release Notes Card Enabled' })).toBeChecked();

        await page.goto(BASE_URL + '/app/help-center', { waitUntil: 'domcontentloaded' });
        await expect(page.getByText('Release Notes', { exact: true })).toBeVisible({ timeout: 20_000 });
      } finally {
        await restoreOwnedFields(request);
      }
    });
  },
);
