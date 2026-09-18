/**
 * elitea_issues package C-agents — the Save-button-dirtiness cluster.
 *
 * `pages/agents/EditApplication.tsx`'s Save button used to gate purely on
 * `form.formState.isValid && !isSaving` — an existing, already-saved agent is
 * necessarily valid, so Save reported "there is something to save" the
 * instant the edit page loaded, before the user touched a single field, and
 * stayed exactly as active after an edit was undone back to its saved value.
 * Fixed at the root: `canSave` now also requires `isDirty` (the same
 * `form.formState.isDirty || versionFields.isDirty` value the page already
 * computed for the #133 unsaved-changes nav blocker, and which
 * `useEditApplicationVersionFields`'s `areEqual` already clears back to
 * false on an undo — the gate just was not wired to the button).
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createAgent, deleteAgent } from '../../fixtures/api';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${String(Date.now()).slice(-9)}`;
}

/* elitea_issues: #5107 — Save stays disabled on load when nothing has been edited yet */
test('J-save-dirty: Save is disabled on load and only enables once a field is actually edited', async ({
  page,
  request,
}) => {
  const name = uniqueName('save-dirty');
  const agent = await createAgent(request, name);
  try {
    await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
    const editPanel = page.getByTestId('edit-application-configuration-tab-panel');
    await expect(editPanel).toBeVisible({ timeout: 20_000 });

    const descriptionInput = editPanel.getByTestId('agent-description-input');
    await expect(descriptionInput).toHaveValue(`${AUTOTEST_PREFIX}e2e test agent`, { timeout: 15_000 });

    const save = page.getByTestId('agent-save-button');
    await expect(save, '#5107 — no edit yet, Save must stay disabled').toBeDisabled();

    const edited = `${AUTOTEST_PREFIX}edited description`;
    await descriptionInput.fill(edited);
    await expect(save, 'an actual edit must enable Save').toBeEnabled({ timeout: 5_000 });

    /* elitea_issues: #5987 — undoing an edit back to the saved value disables Save again */
    await descriptionInput.fill(`${AUTOTEST_PREFIX}e2e test agent`);
    await expect(save, '#5987 — reverting to the saved value must disable Save again').toBeDisabled({
      timeout: 5_000,
    });
  } finally {
    await deleteAgent(request, agent.id);
  }
});
