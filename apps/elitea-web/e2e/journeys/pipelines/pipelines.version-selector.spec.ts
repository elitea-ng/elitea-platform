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
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline, type CreatedPipeline } from '../../fixtures/pipelines';

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

/* onetest: ELITEA-3279 — product gap: the dropdown row shows neither the creator's name/email nor a "MMM DD, YYYY, hh:mm AM/PM" timestamp. */
test('a version row names its creator and shows a full date+time — PRODUCT GAP', async ({ page }) => {
  test.fail(true, 'ELITEA-3279: product gap — AgentPipelineVersionSelector.tsx:formatVersionDisplayText renders only "<name> – DD.MM.YYYY" (day.month.year, no time, no AM/PM) and the menu row renders no creator name or email anywhere');
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

  // The spec's required format: "MMM DD, YYYY, hh:mm AM/PM" (e.g. "Sep 10, 2026, 03:45 PM").
  expect(text, 'the row must carry a month-name date with a time and AM/PM').toMatch(/[A-Za-z]{3}\s\d{1,2},\s\d{4},\s\d{1,2}:\d{2}\s(AM|PM)/);
  // The spec requires the creator's name and/or email to be visible.
  expect(text.toLowerCase(), 'the row must name its creator').toMatch(/@|\b[a-z]+\s[a-z]+\b/);
});
