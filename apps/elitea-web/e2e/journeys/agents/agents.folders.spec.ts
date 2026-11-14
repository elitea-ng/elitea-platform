/**
 * Project folders of the Agents list (issue 2048; the product gap #5194 and the
 * closed bugs behind it #6485, #6484, #6482 stood for this feature before it
 * existed).
 *
 * The journey: a folder is made from the Filters panel, an agent is moved into
 * it from its card, the list narrows to that folder and back, the table view
 * names the folder in its own column, and deleting the folder returns the agent
 * to the list without deleting it. Agents and the second folder come from the
 * API; every claim is about the list UI and the server's folder rules.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createAgent, deleteAgent } from '../../fixtures/api';
import { DEFAULT_PROJECT_NAME, ensureProjectSelected } from '../../fixtures/project';

const FOLDERS = `${API_BASE}/elitea_core/entity_folders/prompt_lib/${DEFAULT_PROJECT_ID}/agent`;

const agentIds: string[] = [];
const folderIds: string[] = [];

test.afterEach(async ({ request }) => {
  for (const id of folderIds.splice(0)) await request.delete(`${FOLDERS}/${id}`);
  for (const id of agentIds.splice(0)) await deleteAgent(request, id);
});

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
}

async function openAgentsFilters(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/app/agents/all`);
  await ensureProjectSelected(page, DEFAULT_PROJECT_NAME);
  await page.waitForURL('**/app/agents/all**', { timeout: 20_000 });
  await expect(page.getByTestId('agent-search-input')).toBeVisible({ timeout: 30_000 });
  await page.getByTestId('page-toolbar-filters').click();
  await expect(page.getByTestId('entity-folder-panel')).toBeVisible({ timeout: 20_000 });
}

test('J-entity-folders: create a folder, move an agent into it, filter, and delete the folder', async ({ page, request }) => {
  const insideName = uniqueName('infolder');
  const outsideName = uniqueName('atroot');
  const inside = await createAgent(request, insideName);
  const outside = await createAgent(request, outsideName);
  agentIds.push(inside.id, outside.id);
  const folderName = uniqueName('folder');

  await openAgentsFilters(page);

  // Create the folder.
  await page.getByTestId('entity-folder-create').click();
  await page.getByTestId('entity-folder-name-input').fill(folderName);
  const created = page.waitForResponse((r) => r.url().includes('/entity_folders/') && r.request().method() === 'POST');
  await page.getByTestId('entity-folder-name-dialog').getByRole('button', { name: 'Create' }).click();
  const createdBody = (await (await created).json()) as { id: string };
  folderIds.push(createdBody.id);
  await expect(page.getByTestId('entity-folder-row').filter({ hasText: folderName })).toBeVisible();

  // The same name again is refused, and the dialog says why.
  await page.getByTestId('entity-folder-create').click();
  await page.getByTestId('entity-folder-name-input').fill(folderName.toUpperCase());
  await page.getByTestId('entity-folder-name-dialog').getByRole('button', { name: 'Create' }).click();
  await expect(page.getByTestId('entity-folder-name-dialog')).toContainText('already exists');
  await page.getByTestId('entity-folder-name-dialog').getByRole('button', { name: 'Cancel' }).click();

  // Move one agent into it from its card.
  const card = page.getByTestId('entity-card').filter({ hasText: insideName });
  await card.getByTestId('move-to-folder-button').click();
  const moved = page.waitForResponse((r) => r.url().includes('/items/') && r.request().method() === 'PUT');
  await page.getByTestId('move-to-folder-option').filter({ hasText: folderName }).click();
  expect((await moved).status()).toBe(204);

  // Open the folder: only that agent is listed, and the address carries the folder.
  await page.getByTestId('entity-folder-row').filter({ hasText: folderName }).click();
  await expect(page.getByTestId('entity-card').filter({ hasText: insideName })).toBeVisible();
  await expect(page.getByTestId('entity-card').filter({ hasText: outsideName })).toHaveCount(0);
  await expect(page).toHaveURL(/entity_folder=/);

  // The table view names the folder in its own column.
  await page.getByTestId('entity-folder-all').click();
  await page.goto(`${BASE_URL}/app/agents/all?view=table`);
  const row = page.getByTestId('entity-list-row').filter({ hasText: insideName });
  await expect(row.getByTestId('entity-folder-cell-name')).toHaveText(folderName);

  // Deleting the folder returns the agent to the list and deletes nothing.
  await page.getByTestId('page-toolbar-filters').click();
  await page.getByTestId('entity-folder-row').filter({ hasText: folderName }).getByTestId('entity-folder-more').click();
  await page.getByRole('menuitem', { name: 'Delete…' }).click();
  await page.getByTestId('entity-folder-delete-dialog').getByRole('button', { name: 'Delete' }).click();
  await expect(page.getByTestId('entity-folder-row').filter({ hasText: folderName })).toHaveCount(0);
  folderIds.length = 0;
  await expect(page.getByTestId('entity-list-row').filter({ hasText: insideName })).toBeVisible();
  await expect(page.getByTestId('entity-folder-cell-name')).toHaveCount(0);
});
