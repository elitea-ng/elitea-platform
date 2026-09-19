/**
 * Onetest port — the Skills version selector (ELITEA-3289 through ELITEA-3296).
 *
 * The hint sheet describes the RICH dropdown `AgentPipelineVersionSelector`
 * gives Agents and Pipelines (search, a checkmark on the selected row, a
 * default marker, "MMM DD, YYYY, hh:mm AM/PM" timestamps, creator
 * name/email). Skills did not share that component: `pages/skills/
 * SkillEditorHeader.tsx` drew a plain MUI `<Select>` of `<MenuItem>` rows
 * reading `{version.name}{is_default ? ' (default)' : ''}` — one cause behind
 * all six reported gaps.
 *
 * #917 fixed that cause: the header renders the SAME
 * `AgentPipelineVersionSelector`, and `skill_versions.author_id` is now
 * joined against `public.auth_core__user` on the read
 * (`internal/infra/db/repos/skills.go`) so a row can name its creator at all.
 * Every assertion below therefore drives the shared control's own vocabulary
 * — `version-selector-trigger`, a `role="menu"` of `role="menuitem"` rows,
 * `version-selector-search`, `agent-version-default-marker` — exactly as
 * `pipelines.version-selector.spec.ts` already drives it for Pipelines.
 *
 * ONE DELIBERATE DEVIATION from the manual case, inherited from the shared
 * component and documented in it: the default version carries a "Default"
 * LABEL, not a pin BUTTON inside the row. A button inside a `role="menuitem"`
 * row is axe's `nested-interactive` (impact "serious"), a rule this suite's
 * own `checkA11y` fixture does not disable.
 *
 * Setup drives the SAME routes `skills.lifecycle.spec.ts` / `skills.
 * versioning.spec.ts` already prove (create on the form, "New version" +
 * Confirm, edit + Save) rather than guessing an API shape those files never
 * document.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

import type { Page } from '@playwright/test';

/** The shared selector's trigger box, and the menu it opens. */
async function openVersionMenu(page: Page) {
  await page.getByTestId('version-selector-trigger').click();
  const menu = page.getByRole('menu');
  await expect(menu).toBeVisible({ timeout: 10_000 });
  return menu;
}

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}skillver-${stem}-${String(Date.now()).slice(-7)}`;
}

/** Creates a skill on the form, the same path SKILL-1/SKILL-2 drive. Returns its id. */
async function createSkillViaForm(page: Page, name: string, instructions: string): Promise<string> {
  await page.goto(BASE_URL + '/app/skills/all');
  await page.getByRole('button', { name: 'Create skill' }).click();
  await page.waitForURL(/\/skills\/create/, { timeout: 15_000 });
  await page.getByTestId('skill-name-input').fill(name);
  await page.getByTestId('skill-description-input').fill(`${AUTOTEST_PREFIX}skill version selector fixture`);
  await page.getByTestId('skill-instructions-input').fill(instructions);
  const createResponse = page.waitForResponse(
    (response) => response.request().method() === 'POST' && /\/elitea_core\/skills\/prompt_lib\//.test(response.url()),
  );
  await page.getByRole('button', { name: 'Save' }).click();
  const created = await (await createResponse).json();
  const skillId = String(created.id);
  await page.waitForURL(new RegExp(`/skills/all/${skillId}`), { timeout: 15_000 });
  return skillId;
}

/**
 * "New version" → name it → Confirm → SELECT it from the dropdown (creating
 * a version does not itself navigate onto it — `skills.versioning.spec.ts`'s
 * own SKILL-2 switches via the dropdown as a separate step too) → edit its
 * instructions → Save. Leaves the new version active.
 */
async function addNamedVersion(page: Page, skillId: string, versionName: string, instructions: string): Promise<void> {
  const versionResponse = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      new RegExp(`/elitea_core/skill/prompt_lib/[^/]+/${skillId}$`).test(response.url()),
  );
  await page.getByRole('button', { name: 'New version' }).click();
  await page.getByLabel('Version name').fill(versionName);
  await page.getByRole('button', { name: 'Confirm' }).click();
  await versionResponse;

  const menu = await openVersionMenu(page);
  await menu.getByRole('menuitem', { name: new RegExp(versionName) }).click();
  await page.waitForURL(new RegExp(`/skills/all/${skillId}/`), { timeout: 15_000 });

  const instructionsField = page.getByTestId('skill-instructions-input');
  await instructionsField.fill(instructions);
  const savedResponse = page.waitForResponse(
    (response) => response.request().method() === 'PUT' && response.url().includes('/skill/prompt_lib/'),
  );
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await savedResponse;
}

async function deleteSkill(request: import('@playwright/test').APIRequestContext, skillId: string): Promise<void> {
  await request.delete(`${API_BASE}/elitea_core/skill/prompt_lib/${DEFAULT_PROJECT_ID}/${skillId}`);
}

test.describe('Skills version selector', () => {
  /* onetest: ELITEA-3289 — the version dropdown opens and lists every stored version, with no console error. */
  test('the version dropdown opens and lists every stored version', async ({ page, request }) => {
    const name = uniqueName('list');
    // Excludes "429 Too Many Requests" resource-load noise — a symptom of
    // this suite's own parallel workers hammering the seeded stack, not a
    // JS error the version-selector UI raised.
    const consoleErrors: string[] = [];
    page.on('console', (message) => {
      if (message.type() === 'error' && !/\b429\b/.test(message.text())) consoleErrors.push(message.text());
    });
    const skillId = await createSkillViaForm(page, name, 'Base instructions for the version list case.');
    try {
      await addNamedVersion(page, skillId, 'draft-v1', 'Draft version instructions.');

      const menu = await openVersionMenu(page);
      await expect(menu.getByRole('menuitem', { name: 'base', exact: true })).toBeVisible();
      await expect(menu.getByRole('menuitem', { name: /draft-v1/ })).toBeVisible();
      expect(consoleErrors, `unexpected console errors: ${consoleErrors.join('\n')}`).toHaveLength(0);
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3292 — switching the version selector swaps the displayed instructions to match the
     newly-selected version's stored content. */
  test("switching versions swaps the editor's instructions to the selected version", async ({ page, request }) => {
    const name = uniqueName('switch');
    const baseInstructions = 'Base version instructions, distinct from the draft.';
    const draftInstructions = 'Draft version instructions, distinct from base.';
    const skillId = await createSkillViaForm(page, name, baseInstructions);
    try {
      await addNamedVersion(page, skillId, 'draft-v1', draftInstructions);
      await expect(page.getByTestId('skill-instructions-input')).toHaveValue(draftInstructions);

      const menu = await openVersionMenu(page);
      await menu.getByRole('menuitem', { name: 'base', exact: true }).click();
      await expect(page.getByTestId('skill-instructions-input')).toHaveValue(baseInstructions, { timeout: 20_000 });
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3290 (#917) — the selected row carries a checkmark and the selected-row styling, which is
     `AgentPipelineVersionSelector`'s `CheckIcon`. The bare `<MenuItem>` the header used before had neither. */
  test('the selected version shows a checkmark and highlight in the open dropdown', async ({ page, request }) => {
    const name = uniqueName('checkmark');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      await addNamedVersion(page, skillId, 'draft-v1', 'Draft instructions.');
      const menu = await openVersionMenu(page);
      const selectedOption = menu.getByRole('menuitem', { name: /draft-v1/ });
      await expect(selectedOption.locator('svg')).toBeVisible();
      // The check is on the SELECTED row only — an icon on every row would
      // pass the assertion above while marking nothing.
      await expect(menu.getByRole('menuitem', { name: 'base', exact: true }).locator('svg')).toHaveCount(0);
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3291 (#917) — the default version is marked independently of selection. The shared
     component marks it with a "Default" LABEL rather than a pin button inside the row; see this file's
     header for the `nested-interactive` reason, which is a deliberate deviation, not a missing affordance.
     elitea_issues: #5650, #5663 asked for a pin in the same place and are answered by the same marker. */
  test('the default version is marked in the dropdown, independently of which row is selected', async ({ page, request }) => {
    test.setTimeout(90_000);
    const name = uniqueName('pin');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      await addNamedVersion(page, skillId, 'draft-v1', 'Draft instructions.');

      // A freshly created skill names NO default version at all
      // (`skills.meta.default_version_id` is empty, and the read only reports
      // the flag — it does not fall back to `base`), so neither row carries
      // the marker yet. The same starting state `pipelines.version-selector.
      // spec.ts` documents for a fresh pipeline.
      let menu = await openVersionMenu(page);
      await expect(menu.getByTestId('agent-version-default-marker')).toHaveCount(0);
      await page.keyboard.press('Escape');

      // "Set default" acts on the version the editor currently has open —
      // draft-v1, the one `addNamedVersion` left active.
      const defaultResponse = page.waitForResponse(
        (response) => response.request().method() === 'PATCH' && response.url().includes('/skill_default_version/'),
      );
      await page.getByRole('button', { name: 'Set default' }).click();
      await defaultResponse;

      menu = await openVersionMenu(page);
      await expect(menu.getByRole('menuitem', { name: /draft-v1/ }).getByTestId('agent-version-default-marker')).toBeVisible({
        timeout: 20_000,
      });
      // The marker follows the DEFAULT, not the selection: `base` never gets it.
      await expect(menu.getByRole('menuitem', { name: 'base', exact: true }).getByTestId('agent-version-default-marker')).toHaveCount(0);
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3293 (#917) — the dropdown carries the shared search box, filtering by name. */
  test('a search field in the version dropdown filters by name', async ({ page, request }) => {
    test.setTimeout(90_000);
    const name = uniqueName('search');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      await addNamedVersion(page, skillId, 'Production-v1', 'Prod instructions.');
      const menu = await openVersionMenu(page);
      const searchInput = page.getByTestId('version-selector-search');
      await expect(searchInput).toBeVisible();
      await searchInput.fill('prod');
      await expect(menu.getByRole('menuitem', { name: /Production-v1/ })).toBeVisible();
      await expect(menu.getByRole('menuitem', { name: 'base', exact: true })).toHaveCount(0);
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3294 (#917) — each named row's secondary line carries the creator and a
     "MMM DD, YYYY, hh:mm AM/PM" timestamp. The creator needed a backend field: `skill_versions.author_id`
     is now joined against `public.auth_core__user` on the read, the same join application versions get. */
  test('version metadata shows a "MMM DD, YYYY, hh:mm AM/PM" timestamp and the creator', async ({ page, request }) => {
    const name = uniqueName('metadata');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      await addNamedVersion(page, skillId, 'draft-v1', 'Draft instructions.');
      const menu = await openVersionMenu(page);
      const rowText = (await menu.getByRole('menuitem', { name: /draft-v1/ }).innerText()).trim();
      expect(rowText, 'the row must carry a month-name date with a time and AM/PM').toMatch(
        /[A-Za-z]{3}\s\d{1,2},\s\d{4},\s\d{1,2}:\d{2}\s(AM|PM)/,
      );
      // The creator half — an e-mail or a name ahead of the timestamp's
      // separator. A row with only a timestamp would still match above.
      expect(rowText, `expected a creator before the timestamp in: ${rowText}`).toMatch(/\S+\s·\s[A-Za-z]{3}\s\d{1,2},/);
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3295 (#917) — `toDisplayVersions` sorts `base` first and the rest newest-first, and the
     search filter preserves that order (`.filter()` never reorders). */
  test('versions are sorted newest-first, and stay sorted while searching', async ({ page, request }) => {
    test.setTimeout(120_000);
    const name = uniqueName('sortorder');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      await addNamedVersion(page, skillId, 'older-named', 'v1');
      await addNamedVersion(page, skillId, 'newer-named', 'v2');
      const menu = await openVersionMenu(page);
      const optionTexts = await menu.getByRole('menuitem').allTextContents();
      const newerIndex = optionTexts.findIndex((text) => text.includes('newer-named'));
      const olderIndex = optionTexts.findIndex((text) => text.includes('older-named'));
      expect(newerIndex, `expected newer-named before older-named in: ${optionTexts.join(' | ')}`).toBeLessThan(olderIndex);

      // The same order survives a search that keeps both rows.
      await page.getByTestId('version-selector-search').fill('named');
      const filtered = await menu.getByRole('menuitem').allTextContents();
      expect(filtered.findIndex((text) => text.includes('newer-named'))).toBeLessThan(
        filtered.findIndex((text) => text.includes('older-named')),
      );
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3296 (#917) — Skills, Agents and Pipelines now use ONE component, so there is nothing
     left to drift: the control Agents and Pipelines are driven by (`version-selector-trigger`) is the
     control the skill editor renders. */
  test('the Skills version selector uses the same trigger control as Agents and Pipelines', async ({ page, request }) => {
    const name = uniqueName('consistency');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      await expect(page.getByTestId('version-selector-trigger')).toBeVisible({ timeout: 15_000 });
    } finally {
      await deleteSkill(request, skillId);
    }
  });
});
