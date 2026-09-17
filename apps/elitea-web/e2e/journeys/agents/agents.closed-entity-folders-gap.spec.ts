/**
 * elitea_issues package C-agents — the entity-folders feature cluster.
 *
 * #5194 ("Organize Project Entities into Folders") is the parent story;
 * #6524 (folder-level permissions) depends on it entirely. Both were
 * confirmed absent by code reading, not just by this journey:
 * `apps/elitea-web/src/entities/folder` is scoped to CHAT-CONVERSATION
 * folders only (`features/chat-conversation-list/ui/folders/*`); nothing
 * under `src/pages/agents` or `src/features/agents` renders a "Create
 * Folder" affordance, a folder tree, or reads/writes a folder id beyond the
 * `folder_id` LIST query param `services/elitea-main/internal/api/v2/
 * applications/handler.go` already accepts (filtering only — no folder
 * CRUD endpoint exists to populate it from this app).
 *
 * The five remaining CLOSED bugs in this package that describe entity-folder
 * UI details (#6558 read-only exception false-success, #6485 unfocused
 * Folder Name field, #6484 stale filtered view after delete, #6482 stale
 * folder count, #6423 folder pagination-reset) all presuppose this feature
 * exists; none of their specific UI paths can be reached in this app at all.
 *
 * One journey stands for the whole cluster: the Agents list page renders no
 * folder affordance whatsoever.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { DEFAULT_PROJECT_NAME, ensureProjectSelected } from '../../fixtures/project';

/* elitea_issues: #5194, #6524, #6558, #6485, #6484, #6482, #6423 — product gap: entity folders (Agents/Pipelines/Toolkits/MCPs/Credentials) do not exist in this app */
test('J-folders-gap: the Agents list page has no folder affordance at all', async ({ page }) => {
  test.fail(
    true,
    '#5194: product gap — entity folders (create/rename/delete/move, folder-level permission exceptions, folder counts) are not implemented for Agents/Pipelines/Toolkits/MCPs/Credentials in this app; only chat-conversation folders exist (features/chat-conversation-list)',
  );

  await page.goto(`${BASE_URL}/app/agents/all`);
  await ensureProjectSelected(page, DEFAULT_PROJECT_NAME);
  await page.waitForURL('**/app/agents/all**', { timeout: 20_000 });
  await expect(page.getByTestId('agent-search-input'), 'the agents list must render first').toBeVisible({
    timeout: 30_000,
  });

  // The assertion this test WOULD make once the feature exists: a "Create
  // Folder" control reachable from the Agents list. It is asserted as a
  // positive `toBeVisible`, so it fails now (proving the gap) and starts
  // passing the moment the feature ships — never `test.skip`.
  await expect(page.getByRole('button', { name: /create folder/i })).toBeVisible({ timeout: 2_000 });
});
