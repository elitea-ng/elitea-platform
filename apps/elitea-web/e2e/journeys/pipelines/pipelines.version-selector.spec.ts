/**
 * The pipeline editor's version-selector DROPDOWN (`AgentPipelineVersionSelector`,
 * `src/features/agents/ui/AgentPipelineVersionSelector.tsx`) — the SAME
 * component the agent editor mounts (`agents-version-text`/
 * `version-selector-trigger`), so ELITEA-3281's "compare Pipelines' dropdown
 * against Agents' " is answered by construction: there is one implementation,
 * not two to drift apart (see `not-applicable.md`).
 *
 * `pipelines.versioning.spec.ts` already proves the dropdown SWITCHES the
 * loaded graph and moves the default pointer; it opens the menu but never
 * asserts on the menu's own contents (row count, the checkmark, the default
 * marker). This file closes that gap.
 *
 * Issue 940/A11 (ELITEA-3278/3280/3281) added a SEARCH box to the dropdown,
 * filtering by name and creator while preserving the timestamp sort — see
 * this file's own tests below. Landing that search meaningfully needed real
 * creator data on the version list (`services/elitea-main/internal/api/v2/
 * applications/handler.go`'s `getVersions` now answers `versions[].author`,
 * the same `{id,email,name}` shape `fetchVersionDetails` already gave a
 * single version), which also closed ELITEA-3279 (below) as a side effect —
 * the row's own secondary line now names the creator and a full
 * "MMM D, YYYY, h:mm AM/PM" timestamp, so that case's `test.fail` is now a
 * real, green assertion.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline, type CreatedPipeline } from '../../fixtures/pipelines';

/** The `member` persona's own identity — every version this file creates is authored by it (default project storageState, `playwright.config.ts`). Same constant `artifacts.p13-bucket-permissions.spec.ts` already uses for the identical persona. */
const MEMBER_EMAIL = 'e2e-member@autotest.local';

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline !== undefined) await deletePipeline(page.request, pipeline);
  }
});

async function openEditor(page: Page, pipeline: CreatedPipeline): Promise<void> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
}

/** "Save As Version" through the bar's own dialog — same shape `pipelines.versioning.spec.ts` establishes. */
async function saveAsVersion(page: Page, versionName: string): Promise<string> {
  const createdResponse = page.waitForResponse(
    (response) => response.request().method() === 'POST' && response.url().includes('/versions/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await page.getByRole('button', { name: 'Save As Version' }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible({ timeout: 10_000 });
  await dialog.getByLabel('Version name').fill(versionName);
  await dialog.getByRole('button', { name: 'Save', exact: true }).click();
  const body = (await (await createdResponse).json()) as { id?: string | number };
  expect(body.id).toBeTruthy();
  return String(body.id);
}

/* onetest: ELITEA-3274, ELITEA-3275 — the version dropdown lists every version the pipeline holds, and marks the CURRENTLY OPEN one with a checkmark. */
test('the version selector lists every version and checks the one currently open', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}ver-sel-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  await openEditor(page, pipeline);

  const versionName = `v${Date.now() % 1e5}`;
  const newVersionId = await saveAsVersion(page, versionName);
  await page.waitForURL(new RegExp(`/app/pipelines/\\w+/${pipeline.id}/${newVersionId}`), { timeout: 20_000 });

  await page.getByTestId('version-selector-trigger').click();
  const menu = page.getByRole('menu');
  await expect(menu.getByRole('menuitem', { name: 'base', exact: true })).toBeVisible({ timeout: 10_000 });
  const newVersionRow = menu.getByRole('menuitem', { name: new RegExp(versionName) });
  await expect(newVersionRow).toBeVisible();

  // 3275: the row for the version the editor has open right now carries the
  // checkmark; the other row does not. No `data-testid` names the icon
  // (`@mui/icons-material`'s own dev-only one is stripped under
  // `NODE_ENV=production`, the E2E image's build), so the CHECKMARK ROW is
  // the discriminator instead: the "Set as default"/"Delete version" command
  // rows below it carry icons of their own, but a VERSION row (`base` or a
  // named one) only ever contains an `<svg>` when its checkmark is showing.
  await expect(newVersionRow.locator('svg')).toHaveCount(1);
  await expect(menu.getByRole('menuitem', { name: 'base', exact: true }).locator('svg')).toHaveCount(0);
});

/* onetest: ELITEA-3276 — the default version carries a visible "Default" marker in the dropdown, and the marker moves after "Set as default". */
test('"Set as default" moves the dropdown\'s Default marker to the new version', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}ver-def-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  await openEditor(page, pipeline);

  const versionName = `v${Date.now() % 1e5}`;
  const newVersionId = await saveAsVersion(page, versionName);
  await page.waitForURL(new RegExp(`/app/pipelines/\\w+/${pipeline.id}/${newVersionId}`), { timeout: 20_000 });

  await page.getByTestId('version-selector-trigger').click();
  let menu = page.getByRole('menu');
  // Before "Set as default": a freshly created pipeline's `versions[]` carries
  // no explicit `is_default` row yet (`resolveDefaultVersionId` only reads that
  // flag — it does not fall back to `base` the way the read API's
  // `GetDefaultVersion` route does), so NEITHER row carries the marker yet.
  await expect(menu.getByTestId('agent-version-default-marker')).toHaveCount(0);

  const patched = page.waitForResponse(
    (response) => response.request().method() === 'PATCH' && response.url().includes('/default_version/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await page.getByTestId('agent-version-set-default').click();
  await page.getByRole('button', { name: 'Set as a default' }).click();
  await patched;

  await page.getByTestId('version-selector-trigger').click();
  menu = page.getByRole('menu');
  await expect(menu.getByRole('menuitem', { name: new RegExp(versionName) }).getByTestId('agent-version-default-marker')).toBeVisible({
    timeout: 15_000,
  });
  await expect(menu.getByRole('menuitem', { name: 'base', exact: true }).getByTestId('agent-version-default-marker')).toHaveCount(0);
});

/* onetest: ELITEA-3279 — FIXED (was a product gap, flipped green by issue 940/A11): the dropdown row
 * now names its creator and shows a full "MMM D, YYYY, h:mm AM/PM" timestamp. */
test('a version row names its creator and shows a full date+time', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}ver-meta-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  await openEditor(page, pipeline);

  // `base` (the `LATEST_VERSION_NAME`) renders the literal word "base" with
  // no date at all — a saved, non-"base" version is the row that goes
  // through the real `formatVersionDisplayText` date-formatting path.
  const versionName = `v${Date.now() % 1e5}`;
  await saveAsVersion(page, versionName);

  await page.getByTestId('version-selector-trigger').click();
  const row = page.getByRole('menu').getByRole('menuitem', { name: new RegExp(versionName) });
  const text = (await row.innerText()).trim();

  // The spec's required format: "MMM DD, YYYY, hh:mm AM/PM" (e.g. "Sep 10, 2026, 03:45 PM";
  // `versionMetaLine`'s own formatter does not zero-pad the day/hour, so 1-2 digits are both accepted).
  expect(text, 'the row must carry a month-name date with a time and AM/PM').toMatch(/[A-Za-z]{3}\s\d{1,2},\s\d{4},\s\d{1,2}:\d{2}\s(AM|PM)/);
  // The row must name its creator (name or email) — the authenticated
  // persona's own identity. `[a-z0-9]`, not `[a-z]` alone: this stack's own
  // seeded persona name is "E2E Member" (digits in "E2E"), which a
  // letters-only pattern would not match.
  expect(text.toLowerCase(), 'the row must name its creator').toMatch(/@|\b[a-z0-9]+\s[a-z0-9]+\b/);
  // `versionCreatorLabel` prefers the resolved NAME over the email when a
  // name is known ("E2E Member"), so the visible text names the persona by
  // name, not by the email `MEMBER_EMAIL` itself names it by.
  expect(text.toLowerCase(), 'the creator must be the persona that actually created this version').toContain('e2e member');
});

/* onetest: ELITEA-3278 — the search box filters the version list by NAME and by CREATOR
 * (case-insensitively, partial match), including special characters, and shows "No versions found"
 * for a query that matches nothing. */
test('the version search filters by name and by creator', async ({ page }) => {
  const pipelineName = `${AUTOTEST_PREFIX}ver-search-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, pipelineName);
  created.push(pipeline);
  await openEditor(page, pipeline);

  const versionName = `feature/prod-${Date.now() % 1e5}`;
  await saveAsVersion(page, versionName);

  await page.getByTestId('version-selector-trigger').click();
  const menu = page.getByRole('menu');
  const search = page.getByTestId('version-selector-search');
  await expect(search).toBeVisible({ timeout: 10_000 });

  // By name, case-insensitively and partially.
  await search.fill('PROD');
  await expect(menu.getByRole('menuitem', { name: new RegExp(versionName.replace('/', '\\/')) })).toBeVisible({ timeout: 10_000 });
  await expect(menu.getByRole('menuitem', { name: 'base', exact: true })).toHaveCount(0);

  // Special characters in the name (the slash) are matched literally.
  await search.fill('feature/prod');
  await expect(menu.getByRole('menuitem', { name: new RegExp(versionName.replace('/', '\\/')) })).toBeVisible({ timeout: 10_000 });

  // By creator email — the persona that authored EVERY version in this
  // pipeline, `base` included: `base` is a real row with a real author, its
  // row simply never RENDERS the creator line (a display choice, not a
  // search exclusion — see `AgentPipelineVersionSelector.menu.tsx`'s own
  // `version.isLatest` guard) — so a creator search legitimately still
  // matches it.
  await search.fill(MEMBER_EMAIL);
  await expect(menu.getByRole('menuitem', { name: new RegExp(versionName.replace('/', '\\/')) })).toBeVisible({ timeout: 10_000 });
  await expect(menu.getByRole('menuitem', { name: 'base', exact: true })).toBeVisible();

  // No results.
  await search.fill('nonexistent-xyz-123');
  await expect(menu.getByText('No versions found')).toBeVisible({ timeout: 10_000 });

  // Clearing restores the full list.
  await search.fill('');
  await expect(menu.getByRole('menuitem', { name: 'base', exact: true })).toBeVisible({ timeout: 10_000 });
  await expect(menu.getByRole('menuitem', { name: new RegExp(versionName.replace('/', '\\/')) })).toBeVisible();
});

/* onetest: ELITEA-3280 — the newest-first timestamp sort survives a search: filtered results stay in
 * the same relative order the full, unfiltered list already has. */
test('timestamp sort order is preserved while searching', async ({ page }) => {
  const pipelineName = `${AUTOTEST_PREFIX}ver-sort-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, pipelineName);
  created.push(pipeline);
  await openEditor(page, pipeline);

  // Two versions sharing a "sortv" marker, saved in order — v1 (older) then
  // v2 (newer) — so the newest-first order is `base, v2, v1` before AND
  // after a search that matches only the two "sortv" rows.
  const tag = `sortv-${Date.now() % 1e5}`;
  await saveAsVersion(page, `${tag}-v1`);
  await saveAsVersion(page, `${tag}-v2`);

  await page.getByTestId('version-selector-trigger').click();
  const menu = page.getByRole('menu');
  await page.getByTestId('version-selector-search').fill(tag);

  const rows = menu.getByRole('menuitem').filter({ hasText: tag });
  await expect(rows).toHaveCount(2, { timeout: 10_000 });
  await expect(rows.nth(0)).toContainText(`${tag}-v2`);
  await expect(rows.nth(1)).toContainText(`${tag}-v1`);
});

/* onetest: ELITEA-3281 — the search input's own styling is the SAME component instance the agent
 * editor's dropdown mounts (no separate implementation to drift), so UI consistency holds by
 * construction — verified here by asserting the search box actually renders in this (pipeline)
 * context with the identical placeholder text `AgentPipelineVersionSelector.menu.tsx` renders
 * everywhere it mounts. */
test('the search input is the same shared component pipelines and agents both mount', async ({ page }) => {
  const pipelineName = `${AUTOTEST_PREFIX}ver-ui-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, pipelineName);
  created.push(pipeline);
  await openEditor(page, pipeline);
  await saveAsVersion(page, `v${Date.now() % 1e5}`);

  await page.getByTestId('version-selector-trigger').click();
  const search = page.getByTestId('version-selector-search');
  await expect(search).toBeVisible({ timeout: 10_000 });
  await expect(search).toHaveAttribute('placeholder', 'Search versions');
});
