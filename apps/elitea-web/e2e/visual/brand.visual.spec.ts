/**
 * Second-pack visual baselines (ADR-0024 WP6, decision 9).
 *
 * Every other `@visual` shot photographs the product default: the stack has
 * no brand pack of its own, `bootstrap.js` serves the inert body, and the
 * compiled-in pack renders. These shots photograph the SAME screens under a
 * second pack — a different hue, a serif family, tighter radii, custom logos —
 * so a change to how a pack is APPLIED (the hue rotation in `toMuiPalette`,
 * the asset slots in `BrandLogo`, the favicon in `BrandDocumentHead`) has a
 * reference the default-pack shots cannot give it: under the default, all of
 * that code is on its no-op path.
 *
 * HOW THE PACK GETS IN, and why not through the database.
 *
 * The real delivery path is channel C: rows in `centry.platform_config` →
 * the resolver → `GET /api/v2/branding/bootstrap.js` → `window.elitea_brand`.
 * Journey J38e (`journeys/admin/admin.branding.spec.ts`) drives exactly that
 * path end to end and asserts the DOM effects. It cannot be the mechanism
 * HERE: the branding rows are one row set for the whole deployment, and the
 * `visual` project runs four workers over one stack. A shot that wrote a
 * pack into the database would have every other `@visual` test that happened
 * to be loading at that moment photograph a green serif product, and the
 * failures would land in THEIR baselines, not this file.
 *
 * So the pack is placed on `window.elitea_brand` by `addInitScript`, which is
 * the value the bootstrap script would have set, one step later in the same
 * document. Everything downstream of the global — `channelC.ts`'s parse and
 * trial build, the theme, every consumer — is the production code path, and
 * the asset URLs are the same-origin paths a served pack states, answered by
 * a route handler because nothing was uploaded. The one thing this cannot
 * prove is that the SERVER publishes the pack; J38e proves that.
 *
 * `assertSecondPackActive` guards the assumption: on a stack that DOES serve a
 * pack, `bootstrap.js` overwrites the global and the shot would silently
 * photograph that pack under this name. The guard reads the pack id back
 * from the page and fails loudly instead.
 *
 * Shared helpers are in `lib/settle.ts`; they are deliberately not imported
 * from `routes.visual.spec.ts`, because importing a `*.spec.ts` re-registers
 * its tests.
 */
import { test, expect, type Page, type Route } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { SNAPSHOT_TOLERANCE, selectProject, settle, shellSettled, volatileRegions } from './lib/settle';

/** The pack id the guard reads back; nothing on any stack states it. */
const SECOND_PACK_ID = 'e2e-second-pack';

/**
 * A complete pack, because `parseBrandPack` validates the whole shape and
 * degrades to the compiled default on ANY failure — a partial pack would
 * photograph the default under this file's name, which is the failure mode
 * the guard below exists for.
 *
 * `schemes` are empty on purpose: that is what the server publishes for a
 * pack whose hue differs from the product default's (the resolver drops the
 * stated tokens), so every `--el-palette-*` here is derived from `brand.hue`
 * by the same rotation a served pack gets.
 */
const SECOND_PACK = {
  $schema: 'https://elitea.ai/schemas/brand-pack/1.json',
  id: SECOND_PACK_ID,
  version: '1.0.0',
  product: {
    name: 'Second Pack',
    shortName: 'Second',
    tagline: 'A tenant that is not the product default',
    docsUrl: 'https://docs.example.com/second-pack',
    supportEmail: 'support@example.com',
  },
  assets: {
    logoFull: '/api/v2/branding/assets/logo-full/e2e-second-pack.svg',
    logoMark: '/api/v2/branding/assets/logo-mark/e2e-second-pack.svg',
    favicon: '/api/v2/branding/assets/favicon/e2e-second-pack.svg',
  },
  typography: {
    // A family the Playwright image resolves to its serif fallback: the point
    // is a visibly different face, not a specific one, and the baseline is
    // taken and compared inside one pinned image.
    fontFamily: 'Georgia, "Times New Roman", serif',
    fontFamilyMono: '"Courier New", Courier, monospace',
    baseSize: 14,
    scale: 1.2,
  },
  shape: {
    radiusSm: 2,
    radiusMd: 4,
    radiusLg: 8,
    radiusPill: 9999,
    density: 'comfortable',
  },
  locale: {
    default: 'en-GB',
    dateLocale: 'en-GB',
  },
  brand: { hue: '#2e7d32' },
  schemes: { light: {}, dark: {} },
} as const;

/** The logo every asset slot answers with: a rounded square in the pack's hue. */
const LOGO_SVG =
  '<svg xmlns="http://www.w3.org/2000/svg" width="40" height="40" viewBox="0 0 40 40">' +
  `<rect width="40" height="40" rx="8" fill="${SECOND_PACK.brand.hue}"/>` +
  '<circle cx="20" cy="20" r="9" fill="#ffffff"/>' +
  '</svg>';

/**
 * Installs the pack before the first document script runs, and answers the
 * pack's asset paths so `<img>` and `<link rel="icon">` resolve to something
 * rather than a 404 (which would render the alt text and photograph that).
 */
async function applySecondPack(page: Page): Promise<void> {
  await page.route('**/api/v2/branding/assets/**', (route: Route) =>
    route.fulfill({
      status: 200,
      contentType: 'image/svg+xml',
      headers: { 'cache-control': 'no-store' },
      body: LOGO_SVG,
    }),
  );
  await page.addInitScript((pack) => {
    (window as unknown as Record<string, unknown>)['elitea_brand'] = pack;
  }, SECOND_PACK);
}

/**
 * The pack this shot claims to photograph is the one on the page. Reads the
 * id straight off the global (a served pack would have replaced it) and one
 * DOM effect (the mark's `src`), so both halves — delivered and applied — are
 * checked before a pixel is compared.
 */
async function assertSecondPackActive(page: Page): Promise<void> {
  const id = await page.evaluate(
    () => (window as unknown as { elitea_brand?: { id?: string } }).elitea_brand?.id,
  );
  expect(
    id,
    'the injected pack was replaced — this stack serves a pack of its own, and this shot would photograph it',
  ).toBe(SECOND_PACK_ID);
  await expect(page.getByTestId('brand-logo-mark').first()).toHaveAttribute(
    'src',
    SECOND_PACK.assets.logoMark,
  );
}

test('@visual brand-second-pack-chat', async ({ page }) => {
  await applySecondPack(page);
  await page.goto(BASE_URL + '/app/chat', { waitUntil: 'domcontentloaded' });
  await shellSettled(page);
  // The same landmark `chat-empty-state` uses: it resolves only once the
  // conversation listing answered, never on a stalled shell.
  await expect(page.getByText('Still no conversations created.')).toBeVisible({ timeout: 20_000 });
  await assertSecondPackActive(page);
  await settle(page);

  await expect(page).toHaveScreenshot('brand-second-pack-chat.png', {
    fullPage: false,
    mask: volatileRegions(page),
    ...SNAPSHOT_TOLERANCE,
  });
});

test('@visual brand-second-pack-deepwiki', async ({ page }) => {
  await applySecondPack(page);
  // The sub-application screen (ADR-0024 decision 8): the seeded wiki, in its
  // own project, exactly as `deepwiki-browser` photographs it.
  const projectName = await selectProject(page, { id: '90200', name: 'e2e-deepwiki' });
  await page.goto(BASE_URL + '/app/deepwiki/9001', { waitUntil: 'domcontentloaded' });
  await shellSettled(page, projectName);
  await expect(page.getByText('E2E Service Wiki')).toBeVisible({ timeout: 20_000 });
  await assertSecondPackActive(page);
  await settle(page);

  await expect(page).toHaveScreenshot('brand-second-pack-deepwiki.png', {
    fullPage: false,
    mask: volatileRegions(page),
    ...SNAPSHOT_TOLERANCE,
  });
});
