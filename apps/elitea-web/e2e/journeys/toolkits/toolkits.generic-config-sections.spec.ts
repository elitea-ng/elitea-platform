/**
 * Wave-1 package toolkits-credentials/B (toolkits-UI slice, 10 cases) —
 * the toolkit editor's Configuration/Tools/Indexes surfaces.
 *
 * Ported BY USE CASE against the REAL screen, not the legacy one-page
 * three-accordion layout the manual cases describe:
 *
 *  - The new app has NO outer collapsible "Configuration" section — the
 *    left panel's primary fields (Toolkit Name, Description, Credentials,
 *    …) always render. What DOES hide/reveal is a per-type "Advanced
 *    Settings" accordion (`ToolBase.render.tsx`'s `advancedEntries`), wired
 *    for exactly two types: `jira` and `confluence`
 *    (`ToolJira.tsx`/`ToolConfluence.tsx`). Measured against THIS stack's
 *    served schema (`GET /elitea_core/toolkits/prompt_lib/1`): `jira`'s own
 *    advanced-field list (`verify_ssl`/`additional_fields`/`custom_headers`)
 *    names fields the served jira schema does not even declare, so its
 *    accordion can never appear; `confluence`'s list
 *    (`max_pages`/`number_of_retries`/`min_retry_seconds`/
 *    `max_retry_seconds`/`custom_headers`) is fully present. `confluence` is
 *    therefore the type these journeys use for the primary/advanced split.
 *  - "Indexes" is a separate TAB (`IndexesTab.tsx`), not a third accordion
 *    section beside Configuration/Tools on one page.
 *
 * See `S/port/ledger-P9-toolkits-B.tsv` for the full per-case verdict.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

const RUN_ID = String(Date.now()).slice(-6);

/** Ids created by this file, deleted in afterAll. Never a blanket sweep — other specs run concurrently. */
const createdIds: string[] = [];

test.afterAll(async ({ browser }) => {
  if (createdIds.length === 0) return;
  const ctx = await browser.newContext();
  for (const id of createdIds) {
    await ctx.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`).catch(() => {});
  }
  await ctx.close();
});

/** Ids of the placeholder credentials this file creates, deleted in afterAll alongside the toolkits. */
const createdCredentialIds: string[] = [];

test.afterAll(async ({ browser }) => {
  if (createdCredentialIds.length === 0) return;
  const ctx = await browser.newContext();
  for (const id of createdCredentialIds) {
    await ctx.request.delete(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`).catch(() => {});
  }
  await ctx.close();
});

/**
 * A minimal, real `confluence` credential (`base_url` is confluence's only
 * required `data` field — measured against `GET /configurations/available/`)
 * — `validateToolkitCreate` answers 400 `configuration_not_found` for a
 * reference that does not resolve, so the toolkits below need one real row
 * to point at, not a placeholder title.
 */
async function createConfluenceCredential(request: import('@playwright/test').APIRequestContext, eliteaTitle: string): Promise<void> {
  const created = await request.post(`${API_BASE}/configurations/configurations/${DEFAULT_PROJECT_ID}`, {
    data: { elitea_title: eliteaTitle, label: eliteaTitle, type: 'confluence', data: { base_url: 'https://autotest.invalid/wiki' } },
  });
  expect(created.status(), `creating the confluence credential answered ${created.status()}: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const id = String(((await created.json()) as { id?: string | number; data?: { id?: string | number } }).id ?? '');
  if (id !== '') createdCredentialIds.push(id);
}

/**
 * A `confluence` toolkit, created over the API (the create FORM/picker is
 * `toolkits.lifecycle.spec.ts`'s own scope). `space`/`confluence_configuration`
 * are `confluence`'s own required keys (measured against the served
 * schema).
 */
async function createConfluenceToolkit(
  request: import('@playwright/test').APIRequestContext,
  name: string,
  extraSettings: Readonly<Record<string, unknown>> = {},
): Promise<string> {
  const credentialTitle = `${AUTOTEST_PREFIX}cred_confluence_${RUN_ID}_${Date.now()}`;
  await createConfluenceCredential(request, credentialTitle);

  const created = await request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name,
      type: 'confluence',
      description: 'P9-toolkits-B config-sections fixture',
      settings: {
        space: 'AUTOTESTSPACE',
        confluence_configuration: { elitea_title: credentialTitle, private: false },
        selected_tools: [],
        ...extraSettings,
      },
    },
  });
  expect(created.status(), `creating the confluence toolkit answered ${created.status()}: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const id = String(((await created.json()) as { id?: string | number }).id ?? '');
  expect(id, 'the created toolkit must carry an id').not.toBe('');
  createdIds.push(id);
  return id;
}

test('ELITEA-2812/2813: primary fields render immediately; additional settings sit behind an Advanced Settings toggle', async ({ page }) => {
  /* onetest: ELITEA-2812, ELITEA-2813 — primary config fields visible on load; an Advanced Settings toggle reveals/hides the rest (`ToolBase.render.tsx` passes `defaultExpanded={false}` for this accordion — the "Show more"/"Show less" LINK the case describes is a titled accordion here instead) */
  test.setTimeout(120_000);
  const id = await createConfluenceToolkit(page.request, `${AUTOTEST_PREFIX}tkcfg_${RUN_ID}`);

  await page.goto(`${BASE_URL}/app/toolkits/all/${id}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 30_000 });

  // Primary fields — visible without expanding anything.
  await expect(page.getByRole('textbox', { name: 'Toolkit Name' })).toBeVisible({ timeout: 20_000 });
  await expect(page.getByLabel('Description')).toBeVisible();
  await expect(page.getByLabel('Space', { exact: true })).toBeVisible();

  // Advanced-only field: MUI's Accordion keeps its panel MOUNTED even
  // collapsed (a plain `toHaveCount` would see it either way), so the
  // toggle is asserted through VISIBILITY, not presence.
  const maxPages = page.getByLabel('Max Pages');
  await expect(maxPages).not.toBeVisible({ timeout: 20_000 });

  const advancedHeader = page.getByRole('button', { name: 'Advanced Settings' });
  await expect(advancedHeader).toBeVisible();
  await advancedHeader.click();
  await expect(maxPages).toBeVisible({ timeout: 10_000 });
  await expect(page.getByLabel('Custom Headers')).toBeVisible();

  await advancedHeader.click();
  await expect(maxPages).not.toBeVisible();
});

test('ELITEA-2814: an unsaved edit inside Advanced Settings survives collapsing and re-expanding it', async ({ page }) => {
  /* onetest: ELITEA-2814 — a field value typed inside the advanced accordion is not lost when the accordion is toggled closed and open again */
  test.setTimeout(120_000);
  const id = await createConfluenceToolkit(page.request, `${AUTOTEST_PREFIX}tkcfgval_${RUN_ID}`);

  await page.goto(`${BASE_URL}/app/toolkits/all/${id}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 30_000 });

  const advancedHeader = page.getByRole('button', { name: 'Advanced Settings' });
  await advancedHeader.click();

  const maxPages = page.getByLabel('Max Pages');
  await expect(maxPages).toBeVisible({ timeout: 10_000 });
  await maxPages.fill('42');
  await expect(maxPages).toHaveValue('42');

  // Collapse, then re-expand — the value must still be there, unsaved.
  await advancedHeader.click();
  await expect(maxPages).not.toBeVisible();
  await advancedHeader.click();
  await expect(page.getByLabel('Max Pages')).toHaveValue('42', { timeout: 10_000 });
});

test('ELITEA-2815: PRODUCT GAP — the Form/Raw JSON view toggle uses text labels, not icons', async ({ page }) => {
  /* onetest: ELITEA-2815 — the toggle should be icon-only with tooltips; the real one carries visible text labels ("Form"/"Raw Json") */
  test.setTimeout(90_000);
  const id = await createConfluenceToolkit(page.request, `${AUTOTEST_PREFIX}tkview_${RUN_ID}`);

  await page.goto(`${BASE_URL}/app/toolkits/all/${id}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 30_000 });

  const formButton = page.getByRole('button', { name: 'Form', exact: true });
  await expect(formButton).toBeVisible({ timeout: 20_000 });

  // `FormViewToggle` (`ToolkitForm/FormViewToggle.tsx`) passes `label: 'Form'`
  // / `label: 'Raw Json'` with no `icon` — `TabButtonItem` renders the label
  // as visible `Typography` text whenever one is supplied. This asserts the
  // absence of an icon glyph the case expects to be the ONLY visible content.
  test.fail(true, 'ELITEA-2815 (#923): product gap — the view toggle renders "Form"/"Raw Json" as visible text (FormViewToggle.tsx passes label, no icon), not icon-only controls');
  await expect(formButton.locator('svg')).toBeVisible();
});

test('ELITEA-2816 (#924)/2817: PRODUCT GAP — the Tools section header carries no enabled/total count', async ({ page }) => {
  /* onetest: ELITEA-2816, ELITEA-2817 — header should read "Tools <enabled>/<total>" and update live; the real header is the bare word "Tools" */
  test.setTimeout(90_000);
  const id = await createConfluenceToolkit(page.request, `${AUTOTEST_PREFIX}tktools_${RUN_ID}`, {
    selected_tools: ['create_page', 'delete_page'],
  });

  await page.goto(`${BASE_URL}/app/toolkits/all/${id}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 30_000 });

  // `ToolActionsSelector.tsx` hardcodes its accordion title to the bare
  // string `t('...toolActionsSelector.title', 'Tools')` — there is no prop
  // to inject a count, and no count is computed anywhere in that file.
  const toolsHeader = page.getByRole('button', { name: /^Tools/ });
  await expect(toolsHeader).toBeVisible({ timeout: 20_000 });
  test.fail(true, 'ELITEA-2816 (#924)/2817: product gap — ToolActionsSelector.tsx titles the accordion the bare word "Tools", with no enabled/total count and nothing to update live');
  await expect(toolsHeader).toHaveText(/Tools\s*\d+\s*\/\s*\d+/);
});

test('ELITEA-2820/2822: the Tools accordion header opens and closes on click, and its state survives other on-page interactions', async ({ page }) => {
  /* onetest: ELITEA-2820, ELITEA-2822 — clicking a section header toggles it; its expand/collapse state is not reset by switching the Form/JSON view or toggling Advanced Settings */
  test.setTimeout(120_000);
  const id = await createConfluenceToolkit(page.request, `${AUTOTEST_PREFIX}tktoggle_${RUN_ID}`, {
    selected_tools: ['create_page'],
  });

  await page.goto(`${BASE_URL}/app/toolkits/all/${id}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 30_000 });

  const toolsHeader = page.getByRole('button', { name: /^Tools/ });
  await expect(toolsHeader).toBeVisible({ timeout: 20_000 });
  const toolChip = page.getByRole('button', { name: 'Create page', exact: true });

  // Expanded by default (`BasicAccordion`'s own `defaultExpanded = true` —
  // see the 2812/2813 defect note above). The header still toggles it.
  await expect(toolChip).toBeVisible({ timeout: 20_000 });
  await toolsHeader.click();
  await expect(toolChip).not.toBeVisible();
  await toolsHeader.click();
  await expect(toolChip).toBeVisible({ timeout: 10_000 });

  // Switching the Form/Raw JSON view and toggling the (unrelated) Advanced
  // Settings accordion must not collapse Tools back.
  await page.getByRole('button', { name: 'Raw Json', exact: true }).click();
  await page.getByRole('button', { name: 'Form', exact: true }).click();
  const advancedHeader = page.getByRole('button', { name: 'Advanced Settings' });
  await advancedHeader.click();
  await advancedHeader.click();
  await expect(toolChip).toBeVisible();

  // The header itself still toggles, after all that.
  await toolsHeader.click();
  await expect(toolChip).not.toBeVisible();
});

test('ELITEA-2818: the Indexes tab shows the served empty state and an Add index action', async ({ page }) => {
  /* onetest: ELITEA-2818 — no indexes: the empty-state message and the create action are both present */
  test.setTimeout(120_000);
  const id = await createConfluenceToolkit(page.request, `${AUTOTEST_PREFIX}tkidx_${RUN_ID}`, {
    selected_tools: ['index_data'],
  });

  const indexListRequest = page.waitForResponse((r) => /\/elitea_core\/index_meta\/prompt_lib\//.test(r.url()), { timeout: 30_000 });
  await page.goto(`${BASE_URL}/app/toolkits/all/${id}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 30_000 });

  const indexesTab = page.getByRole('tab', { name: 'Indexes' });
  await expect(indexesTab).toBeVisible({ timeout: 20_000 });
  await indexesTab.click();

  const panel = page.getByTestId('edit-toolkit-indexes-tab-panel');
  await expect(panel).toBeVisible();
  await expect(panel.getByText('Still no indexes created')).toBeVisible({ timeout: 20_000 });
  await expect(panel.getByRole('button', { name: 'Add index' })).toBeVisible();
  const indexList = await indexListRequest;
  expect(indexList.status(), await indexList.text()).toBe(200);
});
