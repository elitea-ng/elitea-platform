/**
 * Drag and drop into project folders, the keyboard path and Move to folder in
 * the selection section (ADR-0051 M2, issue 1628; usability task T6: put four
 * agents in a new folder, one of them by keyboard only).
 *
 * Three agents are dragged into the folder with the pointer; the fourth is
 * picked up with Space, carried with the arrow keys and dropped with Space, and
 * the screen-reader announcements (dnd-kit's live region, and the status line
 * the page adds once the server answered) say each step. The same result comes
 * from the selection section's Move to folder. A pin is the caller's own and
 * narrows the list to what is pinned.
 */
import { expect, test, type Locator, type Page } from '@playwright/test';

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

async function openAgents(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/app/agents/all`);
  await ensureProjectSelected(page, DEFAULT_PROJECT_NAME);
  await page.waitForURL('**/app/agents/all**', { timeout: 20_000 });
  await expect(page.getByTestId('agent-search-input')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('entity-folder-panel')).toBeVisible({ timeout: 20_000 });
}

async function createFolder(page: Page, name: string): Promise<string> {
  await page.getByTestId('entity-folder-create').click();
  await page.getByTestId('entity-folder-name-input').fill(name);
  const created = page.waitForResponse((r) => r.url().includes('/entity_folders/') && r.request().method() === 'POST');
  await page.getByTestId('entity-folder-name-dialog').getByRole('button', { name: 'Create' }).click();
  const body = (await (await created).json()) as { id: string };
  folderIds.push(body.id);
  await expect(page.getByTestId('entity-folder-row').filter({ hasText: name })).toBeVisible();
  return body.id;
}

async function centre(locator: Locator): Promise<{ x: number; y: number }> {
  const box = await locator.boundingBox();
  if (box === null) throw new Error('element has no box');
  return { x: box.x + box.width / 2, y: box.y + box.height / 2 };
}

/** A pointer drag of a card's handle onto a folder row, in steps so dnd-kit's activation distance is crossed. */
async function dragCardToFolder(page: Page, agentName: string, folderName: string): Promise<void> {
  const handle = page.getByTestId('entity-card').filter({ hasText: agentName }).getByTestId('entity-drag-handle');
  const target = page.getByTestId('entity-folder-drop').filter({ hasText: folderName });
  // Both ends must be on screen and laid out before the pointer is measured: a list still re-rendering from the previous drop moves them.
  await expect(handle).toBeVisible();
  await expect(target).toBeVisible();
  await handle.scrollIntoViewIfNeeded();
  const from = await centre(handle);
  const to = await centre(target);
  // Armed before the drag, resolved only by the PUT the drop makes.
  const moved = page.waitForResponse((r) => r.url().includes('/items/') && r.request().method() === 'PUT');
  await page.mouse.move(from.x, from.y);
  await page.mouse.down();
  await page.mouse.move(from.x + 12, from.y + 12, { steps: 4 });
  await page.mouse.move(to.x, to.y, { steps: 14 });
  // The drop target has registered the drag when dnd-kit announces it; releasing earlier drops on nothing.
  await expect(page.locator('[aria-live]').filter({ hasText: `is over ${folderName}` }).first()).toBeAttached();
  await page.mouse.up();
  expect((await moved).status()).toBe(204);
  await expect(page.getByTestId('entity-folder-dnd-status')).toContainText(`Moved ${agentName} to ${folderName}`);
}

/**
 * Carries a picked-up keyboard drag onto the folder row named `folderName`, one arrow key at a time.
 *
 * The panel lists the newest folder first, and other journeys add folders to the same project while
 * this one runs (the selection test below starts on another worker at the same moment), so the row
 * ArrowRight lands on, and the side the new folder is on, are not fixed. Each step reads where the drag
 * is (the row marked `data-drop-over`) and where the folder is, presses toward it, and waits until
 * dnd-kit announces the next row before it reads again: a read straight after the key press can still
 * see the previous announcement, and a walk steered by it steps past the folder.
 */
async function keyboardDragOnto(page: Page, live: Locator, folderName: string): Promise<void> {
  const rows = page.getByTestId('entity-folder-drop');
  const target = rows.filter({ hasText: folderName });
  const over = page.locator('[data-testid="entity-folder-drop"][data-drop-over="true"]');
  // Right moves the drag onto the nearest folder row in the side panel.
  await page.keyboard.press('ArrowRight');
  await expect(live.first()).toContainText(' is over ');
  let said = (await live.first().textContent()) ?? '';
  const steps = await rows.count();
  for (let step = 0; step < steps && !said.includes(`is over ${folderName}`); step += 1) {
    const from = await centre(over);
    const to = await centre(target);
    await page.keyboard.press(to.y < from.y ? 'ArrowUp' : 'ArrowDown');
    await expect(live.first()).not.toHaveText(said);
    said = (await live.first().textContent()) ?? '';
  }
  await expect(live.first()).toContainText(`is over ${folderName}`);
}

async function folderItems(request: import('@playwright/test').APIRequestContext, folderId: string): Promise<number[]> {
  const response = await request.get(FOLDERS);
  const body = (await response.json()) as { items: { id: string; item_ids: number[] }[] };
  return body.items.find((folder) => folder.id === folderId)?.item_ids ?? [];
}

test('T6: four agents into a new folder, three by pointer and one by keyboard with announcements', async ({ page, request }) => {
  const names = [uniqueName('a'), uniqueName('b'), uniqueName('c'), uniqueName('d')];
  const agents = [];
  for (const name of names) agents.push(await createAgent(request, name));
  agentIds.push(...agents.map((agent) => agent.id));
  const folderName = uniqueName('folder');

  await openAgents(page);
  const folderId = await createFolder(page, folderName);

  // Three by pointer.
  for (const name of names.slice(0, 3)) await dragCardToFolder(page, name, folderName);

  // The fourth by keyboard: focus its handle, Space, arrows toward the folder, Space.
  const handle = page.getByTestId('entity-card').filter({ hasText: names[3] ?? '' }).getByTestId('entity-drag-handle');
  await handle.focus();
  await page.keyboard.press('Space');
  const live = page.locator('[aria-live]').filter({ hasText: /Picked up|is over|dropped/ });
  await expect(live.first()).toContainText('Picked up');
  await keyboardDragOnto(page, live, folderName);
  const moved = page.waitForResponse((r) => r.url().includes('/items/') && r.request().method() === 'PUT');
  await page.keyboard.press('Space');
  await expect(live.first()).toContainText(`dropped on ${folderName}`);
  expect((await moved).status()).toBe(204);
  await expect(page.getByTestId('entity-folder-dnd-status')).toContainText(`Moved ${names[3]} to ${folderName}`);

  // All four are in the folder, and opening it lists them.
  expect((await folderItems(request, folderId)).sort()).toEqual(agents.map((agent) => Number(agent.id)).sort());
  await page.getByTestId('entity-folder-row').filter({ hasText: folderName }).click();
  for (const name of names) await expect(page.getByTestId('entity-card').filter({ hasText: name })).toBeVisible();
});

test('Move to folder in the selection section, and the personal Pinned group', async ({ page, request }) => {
  const names = [uniqueName('s1'), uniqueName('s2')];
  const agents = [];
  for (const name of names) agents.push(await createAgent(request, name));
  agentIds.push(...agents.map((agent) => agent.id));
  const folderName = uniqueName('folder');

  await openAgents(page);
  const folderId = await createFolder(page, folderName);

  // Select both (⌘-click selects; a plain click once something is selected adds to it), then Move to folder.
  const first = page.getByTestId('entity-card').filter({ hasText: names[0] ?? '' });
  const second = page.getByTestId('entity-card').filter({ hasText: names[1] ?? '' });
  await first.getByRole('checkbox').check();
  await second.getByRole('checkbox').check();
  await expect(page.getByTestId('agent-selection')).toBeVisible();
  await page.getByTestId('agent-selection-move').click();
  const moves = page.waitForResponse((r) => r.url().includes('/items/') && r.request().method() === 'PUT');
  await page.getByTestId('move-selection-option').filter({ hasText: folderName }).click();
  expect((await moves).status()).toBe(204);
  await expect.poll(async () => (await folderItems(request, folderId)).length).toBe(2);

  // Pin one: the Pinned group narrows the list to it, and it is the caller's own.
  await page.getByTestId('agent-selection-clear').click();
  await first.getByTestId('entity-pin-toggle').click();
  await expect(first.getByTestId('entity-pin-toggle')).toHaveAttribute('aria-pressed', 'true');
  await page.getByTestId('entity-folder-pinned').click();
  await expect(page.getByTestId('entity-card').filter({ hasText: names[0] ?? '' })).toBeVisible();
  await expect(page.getByTestId('entity-card').filter({ hasText: names[1] ?? '' })).toHaveCount(0);
  await expect(page).toHaveURL(/entity_folder=pinned-agent/);
});
