/**
 * SKILL-2 (#874): a skill's version history through the UI — save a NAMED
 * version, edit `base` and the named version independently, compare them,
 * and restore the named version's content back onto `base`.
 *
 * Mirrors `e2e/journeys/pipelines/pipelines.versioning.spec.ts`'s own
 * shape — pipelines and agents share `AgentVersionControls`, so that file is
 * the closest sibling to port BY USE CASE, not by component (skills use
 * their own `SkillEditorHeader`/`SkillCompareModal`, described in
 * `pages/skills/EditSkill.tsx` and `features/skills/ui/SkillCompareModal.tsx`).
 *
 * Every assertion that matters reads the SERVER back (`fetchSkillVersion`
 * below), not just the screen — the same discipline
 * `skills.lifecycle.spec.ts`'s own header note explains.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

/** A name unique per run, so two runs on one stack cannot collide. */
function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
}

interface SkillVersionRow {
  readonly id: string;
  readonly name: string;
  readonly instructions: string;
  readonly tags: readonly string[];
}

interface SkillRow {
  readonly id: string;
  readonly versions?: readonly SkillVersionRow[];
  readonly version_details?: SkillVersionRow;
}

async function fetchSkill(
  request: import('@playwright/test').APIRequestContext,
  skillId: string,
): Promise<SkillRow> {
  const response = await request.get(`${API_BASE}/elitea_core/skill/prompt_lib/${DEFAULT_PROJECT_ID}/${skillId}`);
  expect(response.ok(), `GET skill ${skillId} returned ${response.status()}`).toBe(true);
  return response.json();
}

test('SKILL-2: create a version, edit it independently, compare, and restore it onto base', async ({
  page,
  request,
}) => {
  test.setTimeout(90_000);
  const name = uniqueName('skill-versioning');
  let skillId: string | undefined;
  try {
    // ---- Setup: create the skill on the form (same path SKILL-1 drives). ----
    await page.goto(BASE_URL + '/app/skills/all');
    await page.getByRole('button', { name: 'Create skill' }).click();
    await page.waitForURL(/\/skills\/create/, { timeout: 15_000 });
    await page.getByTestId('skill-name-input').fill(name);
    await page.getByTestId('skill-description-input').fill('Created by the skills versioning journey.');
    await page.getByTestId('skill-instructions-input').fill('base instructions');
    const createResponse = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' && /\/elitea_core\/skills\/prompt_lib\//.test(response.url()),
    );
    await page.getByRole('button', { name: 'Save' }).click();
    const created = await (await createResponse).json();
    skillId = String(created.id);
    await page.waitForURL(new RegExp(`/skills/all/${skillId}`), { timeout: 15_000 });

    // ---- "Save As Version" stores a NAMED version with the current content. ----
    const versionResponse = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' &&
        new RegExp(`/elitea_core/skill/prompt_lib/[^/]+/${skillId}$`).test(response.url()),
    );
    await page.getByRole('button', { name: 'New version' }).click();
    await page.getByLabel('Version name').fill('v2');
    await page.getByRole('button', { name: 'Confirm' }).click();
    await versionResponse;

    await expect
      .poll(async () => (await fetchSkill(request, skillId!)).versions?.map((v) => v.name).sort(), {
        timeout: 20_000,
      })
      .toEqual(['base', 'v2']);

    // ---- Switch to v2 and edit its instructions — base must stay untouched. ----
    await page.getByRole('combobox').click();
    await page.getByRole('option', { name: 'v2' }).click();
    await page.waitForURL(new RegExp(`/skills/all/${skillId}/`), { timeout: 15_000 });

    const instructions = page.getByTestId('skill-instructions-input');
    await expect(instructions).toHaveValue('base instructions');
    await instructions.fill('v2 instructions — much better');
    const savedV2 = page.waitForResponse(
      (response) => response.request().method() === 'PUT' && response.url().includes(`/skill/prompt_lib/`),
    );
    await page.getByRole('button', { name: 'Save' }).click();
    await savedV2;

    let server = await fetchSkill(request, skillId);
    const v2 = server.versions?.find((v) => v.name === 'v2');
    const base = server.versions?.find((v) => v.name === 'base');
    expect(v2?.instructions).toBe('v2 instructions — much better');
    expect(base?.instructions).toBe('base instructions');

    // ---- Compare shows the two versions' content, not just their names. ----
    await page.getByRole('button', { name: 'Compare versions' }).click();
    const compareModal = page.getByTestId('skill-compare-versions-modal');
    await expect(compareModal).toBeVisible({ timeout: 10_000 });
    await expect(compareModal).toContainText('v2');
    await expect(page.getByTestId('skill-compare-diff-Instructions')).toBeVisible();
    await page.keyboard.press('Escape');

    // ---- Restore copies v2's content back onto base — the rollback #874 adds. ----
    const restoreResponse = page.waitForResponse(
      (response) => response.request().method() === 'POST' && response.url().includes('/skill_version_restore/'),
    );
    await page.getByRole('button', { name: 'Restore' }).click();
    await page.getByRole('button', { name: 'Confirm' }).click();
    await restoreResponse;
    await page.waitForURL(new RegExp(`/skills/all/${skillId}/${base?.id}$`), { timeout: 15_000 });

    server = await fetchSkill(request, skillId);
    expect(server.version_details?.instructions).toBe('v2 instructions — much better');
    expect(server.versions?.find((v) => v.name === 'v2')?.instructions).toBe('v2 instructions — much better');
  } finally {
    if (skillId !== undefined) {
      await request.delete(`${API_BASE}/elitea_core/skill/prompt_lib/${DEFAULT_PROJECT_ID}/${skillId}`);
    }
  }
});
