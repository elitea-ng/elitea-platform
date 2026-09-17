/**
 * elitea_issues package C-settings — three independent CLOSED feature
 * requests confirmed absent by reading the current implementation, each too
 * small to carry its own file but none sharing a root cause with the others.
 *
 *  - #6242: `ProjectContextBody.tsx`/`EditorSection.tsx` have no
 *    `useBlocker`/`beforeunload` wiring at all (grepped both files —
 *    zero hits) — editing Project Context and navigating to another
 *    Settings tab discards the draft silently.
 *  - #6217: `ProjectGeneral.tsx` renders exactly three accordions (General,
 *    AI Configurations, Agent & Pipeline Builder) — no "Backup & Restore"
 *    section exists anywhere in Settings.
 *  - #6122: same page, same three accordions — no "Modules" /
 *    "Default modules" section (Attachments/Image Creation/etc. × Chats/
 *    Agents/Pipelines toggle table) exists.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { createScratchProject, deleteScratchProject, type ScratchProject } from '../../fixtures/scratchProject';
import { ensureProjectSelected } from '../../fixtures/project';

const PROJECT_PARAMS_PAGE = `${BASE_URL}/app/settings/project-params`;
const PROJECT_GENERAL_PAGE = `${BASE_URL}/app/settings/project-general`;

/* elitea_issues: #6242 — product gap: editing Project Context and navigating to another Settings tab discards the draft with no warning */
test('J-settings-gap: Project Context has no unsaved-changes warning on navigation away', async ({ page }) => {
  test.fail(
    true,
    '#6242: product gap — Project Context (ProjectContextBody.tsx/EditorSection.tsx) has no useBlocker/beforeunload guard; clicking another Settings tab while editing silently discards the draft',
  );

  let project: ScratchProject | undefined;
  try {
    project = await createScratchProject('J-settings-gap-6242');
    await page.goto(`${BASE_URL}/app/`, { waitUntil: 'domcontentloaded' });
    await ensureProjectSelected(page, project.name);
    await page.goto(PROJECT_PARAMS_PAGE, { waitUntil: 'domcontentloaded' });

    const body = page.getByTestId('project-context-body');
    const empty = page.getByTestId('project-context-empty-state');
    await expect(body.or(empty)).toBeVisible({ timeout: 20_000 });
    if (await empty.isVisible().catch(() => false)) {
      await page.getByTestId('project-context-create-button').click();
      await expect(body).toBeVisible({ timeout: 10_000 });
    }
    const toggle = page.getByRole('switch');
    if (await toggle.isVisible().catch(() => false)) await toggle.click();

    const editor = body.locator('.cm-content');
    await expect(editor).toBeVisible({ timeout: 10_000 });
    await editor.click();
    await page.keyboard.type('AUTOTEST unsaved draft — should trigger a leave-page warning');

    // Navigate to a different Settings tab via the in-app drawer (client-side route change).
    await page.getByRole('button', { name: 'Preferences' }).click();

    // The gap: no confirmation dialog appears — navigation proceeds immediately.
    await expect(page.getByRole('dialog').filter({ hasText: /unsaved changes/i })).toBeVisible({ timeout: 2_000 });
  } finally {
    await deleteScratchProject(project);
  }
});

/* elitea_issues: #6217 — product gap: no "Backup & Restore" section exists anywhere in Settings */
test('J-settings-gap: Settings has no Backup & Restore section', async ({ page }) => {
  test.fail(
    true,
    '#6217: product gap — Settings > General renders no "Backup & Restore" section/accordion; there is no project-level export/restore of any kind',
  );

  await page.goto(PROJECT_GENERAL_PAGE, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('project-general-body')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('backup-restore-section')).toBeVisible({ timeout: 2_000 });
});

/* elitea_issues: #6122 — product gap: no "Modules" / "Default modules" configuration section exists in Settings */
test('J-settings-gap: Settings has no Default Modules configuration', async ({ page }) => {
  test.fail(
    true,
    '#6122: product gap — Settings > General has exactly three accordions (General/AI Configurations/Agent & Pipeline Builder); there is no per-project "Modules" table to default Attachments/Image Creation/etc. for new Chats, Agents and Pipelines',
  );

  await page.goto(PROJECT_GENERAL_PAGE, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('project-general-body')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('default-modules-section')).toBeVisible({ timeout: 2_000 });
});
