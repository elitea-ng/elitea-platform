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

/**
 * A pointer drag of a card's handle onto a folder row, in steps so dnd-kit's activation distance is crossed.
 *
 * While a drag is over a zone, dnd-kit auto-scrolls that zone's scroll container whenever the pointer is in the
 * band along its edge (a fifth of its height). A folder row low in the side panel therefore scrolls out from under
 * a pointer that holds still, and the release lands on nothing ("cancelled", issue 3154). Centred in the panel, the
 * row is outside both bands, or the panel is already at the end it would scroll toward. The pointer then reaches
 * the row at the row's own height, and is aimed again at where the row is now until dnd-kit marks it as the one the
 * drag is over: another journey adding a folder while this one runs pushes the rows down.
 */
async function dragCardToFolder(page: Page, agentName: string, folderName: string): Promise<void> {
  const handle = page.getByTestId('entity-card').filter({ hasText: agentName }).getByTestId('entity-drag-handle');
  const target = page.getByTestId('entity-folder-drop').filter({ hasText: folderName });
  // Both ends must be on screen and laid out before the pointer is measured: a list still re-rendering from the previous drop moves them.
  await expect(handle).toBeVisible();
  await expect(target).toBeVisible();
  // The handle first: centring the row last means no later scroll can move it back into a band.
  await handle.scrollIntoViewIfNeeded();
  await target.evaluate((element) => {
    element.scrollIntoView({ block: 'center' });
  });
  await expect(handle).toBeInViewport();
  const from = await centre(handle);
  const start = await centre(target);
  // Armed before the drag, resolved only by the PUT the drop makes.
  const moved = page.waitForResponse((r) => r.url().includes('/items/') && r.request().method() === 'PUT');
  await page.mouse.move(from.x, from.y);
  await page.mouse.down();
  await page.mouse.move(from.x + 12, from.y + 12, { steps: 4 });
  await expect(page.getByTestId('entity-folder-drag-overlay')).toBeVisible();
  // Up or down the list first, then across at the row's height: the pointer never crosses the panel's edge bands.
  // The card list may auto-scroll on the way; that moves only where the card is drawn, the overlay follows the pointer.
  await page.mouse.move(from.x, start.y, { steps: 6 });
  await page.mouse.move(start.x, start.y, { steps: 14 });
  // The row is marked and dnd-kit has announced the drag over it, read together so the release follows straight on.
  const announced = page.locator('[aria-live]').filter({ hasText: `${agentName} is over ${folderName}` });
  await expect
    .poll(async () => {
      const at = await centre(target);
      await page.mouse.move(at.x, at.y, { steps: 2 });
      return (await target.getAttribute('data-drop-over')) === 'true' && (await announced.count()) > 0;
    })
    .toBe(true);
  await page.mouse.up();
  expect((await moved).status()).toBe(204);
  await expect(page.getByTestId('entity-folder-dnd-status')).toContainText(`Moved ${agentName} to ${folderName}`);
}

/** Where a keyboard drag is: the folder row marked `data-drop-over` (-1: over no folder row), the target row, and what dnd-kit said. */
interface DragState {
  readonly over: number;
  readonly target: number;
  readonly overName: string;
  readonly said: string;
  /** The announcement and the highlighted row describe the same place: one can lag the other by a render. */
  readonly agrees: boolean;
}

async function readDragState(page: Page, live: Locator, folderName: string): Promise<DragState> {
  const said = (await live.first().textContent()) ?? '';
  // The row's name is the Typography after the folder icon; the count follows it.
  const rows = await page
    .getByTestId('entity-folder-drop')
    .evaluateAll((elements) =>
      elements.map((element) => ({
        over: element.getAttribute('data-drop-over') === 'true',
        name: element.querySelector('[data-testid="entity-folder-row"]')?.children[1]?.textContent ?? '',
      })),
    );
  const over = rows.findIndex((row) => row.over);
  const target = rows.findIndex((row) => row.name === folderName);
  const overName = rows[over]?.name ?? '';
  const named = rows.some((row) => row.name !== '' && said.includes(`is over ${row.name}`));
  const agrees = over === -1 ? !named : said.includes(`is over ${overName}`);
  return { over, target, overName, said, agrees };
}

function describeDrag(state: DragState): string {
  return state.over === -1 ? `over no folder row ("${state.said}")` : `over row ${state.over + 1}, ${state.overName}`;
}

/** The drag's state once the announcement and the highlighted row agree, and (given `from`) once it has left `from`; undefined when it never does. */
async function nextDragState(page: Page, live: Locator, folderName: string, from?: DragState): Promise<DragState | undefined> {
  let state: DragState | undefined;
  const moved = (s: DragState): boolean => from === undefined || s.over !== from.over || s.said !== from.said;
  try {
    await expect
      .poll(async () => {
        state = await readDragState(page, live, folderName);
        return state.agrees && moved(state);
      })
      .toBe(true);
  } catch {
    return undefined;
  }
  return state;
}

/** The arrow toward the target row: by row order when the drag is on a row, else by where the dragged item is drawn. */
async function arrowToward(page: Page, state: DragState): Promise<'ArrowUp' | 'ArrowDown'> {
  if (state.over !== -1) return state.target < state.over ? 'ArrowUp' : 'ArrowDown';
  const from = await centre(page.getByTestId('entity-folder-drag-overlay'));
  const to = await centre(page.getByTestId('entity-folder-drop').nth(state.target));
  return to.y < from.y ? 'ArrowUp' : 'ArrowDown';
}

/**
 * Carries a picked-up keyboard drag onto the folder row named `folderName`, one arrow key at a time.
 *
 * The panel lists the newest folder first, and other journeys add folders to the same project while
 * this one runs (the selection test below starts on another worker at the same moment), so the row
 * ArrowRight lands on, and the side the new folder is on, are not fixed. Each step reads where the drag
 * is once dnd-kit's announcement and the row marked `data-drop-over` agree, presses toward the folder,
 * and reads again only once the drag has moved: a read straight after the key press can still see the
 * previous announcement, and a walk steered by it steps past the folder. Every step ends: a key that
 * does not move the drag, or a walk longer than the panel, fails naming where the drag is.
 */
async function keyboardDragOnto(page: Page, live: Locator, folderName: string): Promise<void> {
  // dnd-kit's keyboard sensor listens for the arrow keys only from a zero-delay timer it sets at pick-up, and the
  // announcement is in the page before that timer runs: an arrow pressed in between is never seen (issue 3154).
  // Timers of the same delay run in the order they were set, so once this one has run, the sensor is listening.
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        setTimeout(resolve, 0);
      }),
  );
  // Right moves the drag onto the nearest folder row in the side panel.
  await page.keyboard.press('ArrowRight');
  await expect(live.first()).toContainText(' is over ');
  let state = await nextDragState(page, live, folderName);
  const limit = (await page.getByTestId('entity-folder-drop').count()) + 2;
  for (let step = 0; state === undefined || state.over === -1 || state.over !== state.target; step += 1) {
    if (state === undefined) throw new Error('the drag announcement and the highlighted folder row never agreed');
    if (state.target === -1) throw new Error(`the folder panel has no row named ${folderName}`);
    if (step >= limit) throw new Error(`the keyboard drag did not reach ${folderName} (row ${state.target + 1}) in ${limit} keys: it is ${describeDrag(state)}`);
    const key = await arrowToward(page, state);
    await page.keyboard.press(key);
    const next = await nextDragState(page, live, folderName, state);
    if (next === undefined) throw new Error(`${key} did not move the keyboard drag toward ${folderName} (row ${state.target + 1}): it stayed ${describeDrag(state)}`);
    state = next;
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
