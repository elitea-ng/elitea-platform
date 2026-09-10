/**
 * Onetest port — the Skills version selector (ELITEA-3289 through ELITEA-3296).
 *
 * The hint sheet describes the RICH dropdown `AgentPipelineVersionSelector`
 * gives Agents and Pipelines (search, a checkmark on the selected row, a pin
 * icon + tooltip on the default version, "MMM DD, YYYY, hh:mm AM/PM"
 * timestamps, creator name/email). Skills does not share that component —
 * `src/pages/skills/SkillEditorHeader.tsx` is a plain MUI `<Select>` with
 * `<MenuItem>` rows reading `{version.name}{is_default ? ' (default)' : ''}`
 * — no search input, no checkmark, no pin icon, no timestamp, no creator.
 * That gap is real and is written up case by case below, each confirmed by
 * reading the component before writing the assertion that fails against it.
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

  await page.getByRole('combobox').click();
  await page.getByRole('option', { name: versionName }).click();
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

      const select = page.getByRole('combobox');
      await select.click();
      await expect(page.getByRole('option', { name: /base/i })).toBeVisible();
      await expect(page.getByRole('option', { name: /draft-v1/i })).toBeVisible();
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

      await page.getByRole('combobox').click();
      await page.getByRole('option', { name: /^base$/i }).click();
      await expect(page.getByTestId('skill-instructions-input')).toHaveValue(baseInstructions, { timeout: 20_000 });
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3290 — product gap: the Skills version dropdown draws every row as a plain MenuItem with no
     checkmark and no distinct highlight styling for the selected version — MUI's own Select shows the CLOSED
     value, but the open menu carries no per-row selected indicator the way the Agents/Pipelines version
     selector's `CheckIcon` does. */
  test('the selected version shows a checkmark and highlight in the open dropdown', async ({ page, request }) => {
    test.fail(
      true,
      'ELITEA-3290: product gap — the Skills version dropdown has no checkmark icon or highlight on the selected row (SkillEditorHeader.tsx uses a bare MenuItem, unlike AgentPipelineVersionSelector.tsx)',
    );
    const name = uniqueName('checkmark');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      await addNamedVersion(page, skillId, 'draft-v1', 'Draft instructions.');
      await page.getByRole('combobox').click();
      const selectedOption = page.getByRole('option', { name: /draft-v1/i });
      await expect(selectedOption.locator('svg')).toBeVisible();
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3291 — product gap: the default version shows a plain text " (default)" suffix inside its
     MenuItem label, not a distinct pin icon with a "Default version" tooltip independent of selection. */
  test('the default version shows a pin icon with a "Default version" tooltip', async ({ page, request }) => {
    test.fail(
      true,
      'ELITEA-3291: product gap — the default version has no pin icon or tooltip in Skills, only a " (default)" text suffix appended to the version name',
    );
    const name = uniqueName('pin');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      await page.getByRole('combobox').click();
      const defaultOption = page.getByRole('option', { name: /base/i });
      const pinIcon = defaultOption.getByTestId('skill-version-default-pin');
      await expect(pinIcon).toBeVisible();
      await pinIcon.hover();
      await expect(page.getByText('Default version')).toBeVisible();
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3293 — product gap: there is no search input anywhere in the Skills version dropdown. */
  test('a search field in the version dropdown filters by name and creator', async ({ page, request }) => {
    test.fail(true, 'ELITEA-3293: product gap — the Skills version dropdown has no search input at all');
    const name = uniqueName('search');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      await addNamedVersion(page, skillId, 'Production-v1', 'Prod instructions.');
      await page.getByRole('combobox').click();
      const searchInput = page.getByRole('textbox', { name: /search versions/i });
      await expect(searchInput).toBeVisible();
      await searchInput.fill('prod');
      await expect(page.getByRole('option', { name: /production-v1/i })).toBeVisible();
      await expect(page.getByRole('option', { name: /^base$/i })).toBeHidden();
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3294 — product gap: no version row in the Skills dropdown shows a creation timestamp or a
     creator name/email at all — the row is the version name plus an optional "(default)" suffix, nothing else. */
  test('version metadata shows a "MMM DD, YYYY, hh:mm AM/PM" timestamp and the creator', async ({ page, request }) => {
    test.fail(
      true,
      'ELITEA-3294: product gap — Skills version rows show no timestamp and no creator; MenuItem renders only the version name and an optional "(default)" suffix',
    );
    const name = uniqueName('metadata');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      await addNamedVersion(page, skillId, 'draft-v1', 'Draft instructions.');
      await page.getByRole('combobox').click();
      const row = page.getByRole('option', { name: /draft-v1/i });
      await expect(row.getByText(/\d{1,2}:\d{2}\s?(AM|PM)/i)).toBeVisible();
      await expect(row.getByTestId('skill-version-creator')).toBeVisible();
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3295 — product gap: the version list is not demonstrably sorted at all (`versions` is
     rendered in whatever order `skill.versions` arrives in — `useEditSkillVersionControls.ts` applies no
     sort), so a "newest-first order maintained during search" claim has nothing to stand on; there is also no
     search to maintain the order during (see ELITEA-3293). */
  test('versions are sorted newest-first, and stay sorted while searching', async ({ page, request }) => {
    test.fail(
      true,
      'ELITEA-3295: product gap — the Skills version list applies no sort at all (raw API order) and has no search to maintain a sort order during',
    );
    const name = uniqueName('sortorder');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      await addNamedVersion(page, skillId, 'older-named', 'v1');
      await addNamedVersion(page, skillId, 'newer-named', 'v2');
      await page.getByRole('combobox').click();
      const optionTexts = await page.getByRole('option').allTextContents();
      const newerIndex = optionTexts.findIndex((text) => text.includes('newer-named'));
      const olderIndex = optionTexts.findIndex((text) => text.includes('older-named'));
      expect(newerIndex, `expected newer-named before older-named in: ${optionTexts.join(' | ')}`).toBeLessThan(olderIndex);
    } finally {
      await deleteSkill(request, skillId);
    }
  });

  /* onetest: ELITEA-3296 — product gap: Skills uses a materially different, simpler component
     (`SkillEditorHeader.tsx`'s bare MUI Select) than the Agents/Pipelines version selector
     (`AgentPipelineVersionSelector.tsx`'s custom Menu with a trigger box, refresh icon, checkmark and default
     marker) — confirmed by reading both files, not by a styling measurement. */
  test('the Skills version selector uses the same trigger control as Agents and Pipelines', async ({ page, request }) => {
    test.fail(
      true,
      'ELITEA-3296: product gap — the Skills version selector (bare MUI Select) and the Agents/Pipelines one (custom Menu with trigger/checkmark/default-marker) are different components, not shared styling with drift',
    );
    const name = uniqueName('consistency');
    const skillId = await createSkillViaForm(page, name, 'Base instructions.');
    try {
      // The control Agents/Pipelines use for the same job.
      await expect(page.getByTestId('version-selector-trigger')).toBeVisible({ timeout: 5_000 });
    } finally {
      await deleteSkill(request, skillId);
    }
  });
});
