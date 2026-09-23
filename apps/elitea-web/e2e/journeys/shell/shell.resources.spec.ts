/**
 * Shell: Help Center / Resources (onetest package w1-resources).
 *
 * Ported from the onetest cases in `S/port/pkgs/w1-resources.md`. The page
 * (`src/pages/help-center/HelpCenterPage.tsx`, route `/app/help-center`) was
 * renamed from "Resources" to "Help Center" when its admin config section
 * moved from Admin › Configuration to Admin › Features (`admin.features.spec.ts`'s
 * header) — several onetest cases assert the OLD name and an OLD 4-card
 * layout; this file asserts what actually renders (5 cards, "Help Center").
 *
 * Admin-driven content (per-card links, the version label) is covered
 * end-to-end already by `admin.features.spec.ts`'s "Help Center round trip"
 * (J36g/h/j) — not repeated here (COVERED-EXISTING, see the ledger). This
 * file does not write that shared `resources` admin-config section at all,
 * so it cannot race those tests. The version-info tooltip (ELITEA-0971,
 * RES07) is the one Help Center case that DOES need a write — it lives in
 * its own file, `shell.resources-version-info.spec.ts`, precisely so it can
 * be its own `PLATFORM_FLAG_JOURNEYS` entry without dragging every read-only
 * case here into that single-worker project too.
 *
 * See `S/port/ledger-P3-settings.tsv` for the full per-case disposition and
 * `S/port/defects.md` for the product gaps the FAIL-MARKED tests below pin.
 */
import { test, expect, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { PUBLISH_AUTHOR_PROJECT_NAME } from '../../fixtures/api';
import { DEFAULT_PROJECT_NAME, ensureProjectSelected } from '../../fixtures/project';

const HELP_CENTER_PAGE = `${BASE_URL}/app/help-center`;
const PERSONALIZATION_PAGE = `${BASE_URL}/app/settings/personalization`;

const CARD_TESTIDS = {
  documentation: 'resources-documentation-card',
  releaseNotes: 'resources-release-notes-card',
  videoLibrary: 'resources-video-library-card',
  tutorials: 'resources-tutorials-card',
  interactiveTours: 'resources-interactive-tours-card',
} as const;

function card(page: Page, name: keyof typeof CARD_TESTIDS) {
  return page.locator(`[data-tour="${CARD_TESTIDS[name]}"]`);
}

async function gotoHelpCenter(page: Page): Promise<void> {
  await page.goto(HELP_CENTER_PAGE, { waitUntil: 'domcontentloaded' });
  await expect(card(page, 'documentation')).toBeVisible({ timeout: 20_000 });
}

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-0976 — the page loads with every card, correct titles,
 * subtitles and icons. Asserted against the REAL rendered copy (see file
 * header for the "Explore Resources." vs "Explore Help Center" rename, and
 * the 4-vs-5-card count).
 * ──────────────────────────────────────────────────────────────────────── */
test('RES01: the Help Center page loads with all five cards, correctly labelled', async ({ page }) => {
  await gotoHelpCenter(page);

  await expect(page.getByRole('heading', { name: 'Explore Help Center' })).toBeVisible();
  await expect(
    page.getByText('Guides, documentation, and release notes to support your work.'),
  ).toBeVisible();

  // Title Case in the DOM (`ResourceCardConfig.ts`'s `defaultTitle`) — the
  // all-caps look on screen is `text-transform: uppercase` styling, not the
  // actual text content, so an exact match must use the real casing.
  const expectations: readonly [keyof typeof CARD_TESTIDS, string, string][] = [
    ['documentation', 'Documentation', 'API reference, guides, and platform concepts'],
    ['releaseNotes', 'Release Notes', 'Product updates, improvements, and fixes'],
    ['videoLibrary', 'Video Library', 'Product walkthroughs and recorded sessions'],
    ['tutorials', 'Tutorials', 'Step-by-step guides and use cases'],
    ['interactiveTours', 'Interactive Tours', 'Guided tours to explore key features and workflows'],
  ];
  for (const [key, title, description] of expectations) {
    const c = card(page, key);
    await expect(c).toBeVisible();
    await expect(c.getByText(title, { exact: true })).toBeVisible();
    await expect(c.getByText(description, { exact: true })).toBeVisible();
  }

  // NOT `checkA11y(page)` here: this page fails axe's `scrollable-region-
  // focusable` rule (the `overflowY: auto` content Box in `HelpCenterPage.
  // tsx` has no `tabIndex`/focusable descendant a keyboard user could use to
  // scroll it) — a real, pre-existing gap, but a DIFFERENT claim from this
  // case's own (ELITEA-0976: the cards load with correct copy). Recorded as
  // a bonus finding in `S/port/defects.md` rather than folded into this
  // test's pass/fail, matching that file's own "ELITEA-NONE" precedent for
  // findings with no dedicated onetest id.
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-0960 — the page is strictly read-only for a standard
 * (non-admin) member: no edit/configure/manage affordance anywhere, and
 * double-clicking a title/subtitle never opens an inline editor.
 * ──────────────────────────────────────────────────────────────────────── */
test('RES02: the Help Center page is read-only for a standard member', async ({ page }) => {
  await gotoHelpCenter(page);

  for (const name of ['Edit', 'Configure', 'Manage'] as const) {
    await expect(page.getByRole('button', { name: new RegExp(name, 'i') })).toHaveCount(0);
  }

  const title = card(page, 'documentation').getByText('Documentation', { exact: true });
  await title.dblclick();
  // No inline edit surface appears — the title stays static text, and the
  // page carries no text input at all (the only inputs on this page would
  // be an editor's, and there is none).
  await expect(page.getByRole('textbox')).toHaveCount(0);
  await expect(title).toHaveText('Documentation');

  const before = await card(page, 'documentation').innerText();
  await card(page, 'tutorials').getByText('Step-by-step guides and use cases').dblclick();
  await expect(page.getByRole('textbox')).toHaveCount(0);
  expect(await card(page, 'documentation').innerText()).toBe(before);
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-0966 — the page's content is environment-wide: switching
 * the active project changes nothing about it (the config it reads,
 * `resources`, is a platform-wide admin section, not project-scoped).
 * ──────────────────────────────────────────────────────────────────────── */
test('RES03: Help Center content is identical across projects', async ({ page }) => {
  // Scoped to the Help Center's OWN root (`data-tour="resources-page"`,
  // `HelpCenterPage.tsx`'s outermost `Box`) — NOT `body`, which also holds
  // the sidebar. The sidebar legitimately differs per project (its own
  // name, and which nav items a role sees there), so comparing the whole
  // page would fail on THAT difference while asserting nothing about the
  // Help Center content this case is actually about.
  const helpCenterRoot = () => page.locator('[data-tour="resources-page"]');

  await gotoHelpCenter(page);
  const before = await helpCenterRoot().innerText();

  await ensureProjectSelected(page, PUBLISH_AUTHOR_PROJECT_NAME);
  await gotoHelpCenter(page);
  const afterSwitch = await helpCenterRoot().innerText();
  expect(afterSwitch).toBe(before);

  // Leave the shared session's project as this test found it.
  await ensureProjectSelected(page, DEFAULT_PROJECT_NAME);
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-0968 — switching theme does not navigate away from the
 * page, and all cards remain visible and intact after the switch (both
 * directions).
 * ──────────────────────────────────────────────────────────────────────── */
test('RES04: a theme switch preserves the Help Center page and its cards', async ({ page }) => {
  await gotoHelpCenter(page);

  await page.goto(PERSONALIZATION_PAGE, { waitUntil: 'domcontentloaded' });
  const dark = page.getByRole('button', { name: 'Dark', exact: true });
  const light = page.getByRole('button', { name: 'Light', exact: true });
  await expect(dark).toBeVisible({ timeout: 20_000 });

  await light.click();
  await gotoHelpCenter(page);
  for (const key of Object.keys(CARD_TESTIDS) as (keyof typeof CARD_TESTIDS)[]) {
    await expect(card(page, key)).toBeVisible();
  }

  await page.goto(PERSONALIZATION_PAGE, { waitUntil: 'domcontentloaded' });
  await page.getByRole('button', { name: 'Dark', exact: true }).click();
  await gotoHelpCenter(page);
  for (const key of Object.keys(CARD_TESTIDS) as (keyof typeof CARD_TESTIDS)[]) {
    await expect(card(page, key)).toBeVisible();
  }
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-0969, ELITEA-0970 — each card has a distinct, legible
 * background in both Light and Dark mode, and switching theme changes it.
 * Ported as an OBSERVABLE-STATE check (distinct, non-empty gradients that
 * change with theme) rather than the manual case's named swatches ("peach",
 * "lavender", …), which this design system does not expose as strings —
 * the tokens are gradients (`shared/brand/tokens/default.pack.json`'s
 * `background.resourceCard.<scheme>.card`), not flat named colours.
 * ──────────────────────────────────────────────────────────────────────── */
test('RES05: each card has a distinct background, correct in Light and Dark mode', async ({ page }) => {
  const readBackgrounds = async (): Promise<Record<string, string>> => {
    const out: Record<string, string> = {};
    for (const key of Object.keys(CARD_TESTIDS) as (keyof typeof CARD_TESTIDS)[]) {
      out[key] = await card(page, key).evaluate((el) => getComputedStyle(el).backgroundImage);
    }
    return out;
  };

  await page.goto(PERSONALIZATION_PAGE, { waitUntil: 'domcontentloaded' });
  await page.getByRole('button', { name: 'Dark', exact: true }).click();
  await gotoHelpCenter(page);
  const dark = await readBackgrounds();
  for (const [key, bg] of Object.entries(dark)) {
    expect(bg, `${key} card must have a real background in dark mode`).not.toBe('none');
  }
  // Every card's background is distinct from every other card's.
  expect(new Set(Object.values(dark)).size).toBe(Object.values(dark).length);
  // Text is legible — the icon/title text nodes are visible against it.
  await expect(card(page, 'documentation').getByText('Documentation')).toBeVisible();

  await page.goto(PERSONALIZATION_PAGE, { waitUntil: 'domcontentloaded' });
  await page.getByRole('button', { name: 'Light', exact: true }).click();
  await gotoHelpCenter(page);
  const light = await readBackgrounds();
  for (const [key, bg] of Object.entries(light)) {
    expect(bg, `${key} card must have a real background in light mode`).not.toBe('none');
  }
  expect(new Set(Object.values(light)).size).toBe(Object.values(light).length);
  await expect(card(page, 'documentation').getByText('Documentation')).toBeVisible();

  // And the theme switch itself changed every card's background.
  for (const key of Object.keys(CARD_TESTIDS) as (keyof typeof CARD_TESTIDS)[]) {
    expect(light[key], `${key} card background must differ between themes`).not.toBe(dark[key]);
  }

  // Restore Dark, the compiled default other journeys assume.
  await page.goto(PERSONALIZATION_PAGE, { waitUntil: 'domcontentloaded' });
  await page.getByRole('button', { name: 'Dark', exact: true }).click();
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-0967 — #891 fixed: `ResourceCardConfig.ts` now ships real
 * `defaultLinks` for the Documentation card (real embedded-docs pages, see
 * that file's own header), so a fresh deployment renders an actual `<a>` to
 * hover — `linkSx`'s `&:hover` rule (`HelpCenterPage.tsx`) already applied,
 * unchanged; the fix was giving the card something to apply it to.
 * ──────────────────────────────────────────────────────────────────────── */
test('RES06: a card link shows a hover highlight', async ({ page }) => {
  await gotoHelpCenter(page);
  const link = card(page, 'documentation').getByRole('link').first();
  await expect(link, 'a Documentation link must be present by default').toBeVisible({ timeout: 5_000 });
  const before = await link.evaluate((el) => getComputedStyle(el).color);
  await link.hover();
  await expect
    .poll(async () => link.evaluate((el) => getComputedStyle(el).color), {
      message: 'hovering a resource link must change its colour (linkSx\'s &:hover rule)',
      timeout: 3_000,
    })
    .not.toBe(before);
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-0971 — #892 fixed. See `shell.resources-version-info.spec.ts`
 * (RES07) for the acceptance test — moved to its own file because it is the
 * one Help Center case that writes the shared platform-wide `resources`
 * config section, which belongs in the serial `platform-flags` project, not
 * here alongside ten read-only cases.
 * ──────────────────────────────────────────────────────────────────────── */

/* ────────────────────────────────────────────────────────────────────────
 * onetest: ELITEA-0972, ELITEA-0973, ELITEA-0975 — #891 fixed (partial): the
 * manual cases assume each card ships default, named links. `ResourceCardConfig
 * .ts` now ships real `defaultLinks` for Documentation, Tutorials and Release
 * Notes, pointing at REAL pages of the embedded docs SPA (verified against
 * `content-registry.ts`'s slug rule — no invented paths), plus the "Latest"
 * badge concept (`ResourceLink.badge`, rendered by `HelpCenterPage.tsx`).
 * ELITEA-0974 (Video Library, RES10 below) stays a product gap: this app
 * hosts no video content anywhere, so there is nothing real to default a
 * "video" link to — see `ResourceCardConfig.ts`'s own note.
 * ──────────────────────────────────────────────────────────────────────── */
test('RES08: the Documentation card offers Getting Started/How-To Guides/Integrations/Migration & Update', async ({ page }) => {
  await gotoHelpCenter(page);
  const doc = card(page, 'documentation');
  for (const label of ['Getting Started', 'How-To Guides', 'Integrations', 'Migration & Update']) {
    await expect(doc.getByRole('link', { name: label }), `${label} link must be present by default`).toBeVisible({
      timeout: 3_000,
    });
  }
});

test('RES09: the Tutorials card lists 3 tutorial links and a working More… link', async ({ page }) => {
  await gotoHelpCenter(page);
  const tutorials = card(page, 'tutorials');
  await expect(tutorials.getByRole('link')).toHaveCount(4, { timeout: 3_000 }); // 3 entries + "More…"
  await expect(tutorials.getByRole('link', { name: 'More…' })).toBeVisible();
});

test('RES10: the Video Library card lists 4 video links and a working More… link — product gap', async ({ page }) => {
  test.fail(
    true,
    'ELITEA-0974 (#891): product gap — this app hosts no video content anywhere; every other card in this ' +
      'issue got real default links (#891, fixed), but inventing "video" links with no video behind them ' +
      'would misrepresent the medium, the same reasoning #892 already applied to the hardcoded plugin list.',
  );
  await gotoHelpCenter(page);
  const videos = card(page, 'videoLibrary');
  await expect(videos.getByRole('link')).toHaveCount(5, { timeout: 3_000 }); // 4 entries + "More…"
});

test('RES11: the Release Notes card marks the latest entry "Latest" over historical releases', async ({ page }) => {
  await gotoHelpCenter(page);
  await expect(card(page, 'releaseNotes').getByText('Latest', { exact: true })).toBeVisible({ timeout: 3_000 });
});
