/**
 * Journey 10: Create folder → drag conversation into it → reorder (JRNY-010)
 *
 * Spec §8.5 acceptance (from parity/manifest/chat.json JRNY-010).
 * Acceptance: the conversation is grouped under the folder and reordering
 * persists; the ordering survives a reload.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * WHAT THIS FILE ASSERTS
 * ─────────────────────────────────────────────────────────────────────────
 * The previous version of this file passed by never testing anything: 12
 * escape hatches (test.skip, two early returns, four `.catch(() => false)`
 * downgrades, three `.or()` fallbacks and two unfalsifiable assertions)
 * meant a blank page satisfied it. Every hatch is gone; what is left are
 * unconditional assertions. Do NOT loosen them.
 *
 * Gap 1 (CLOSED) — the folder UI was not mounted on any route. Folders now
 *   live in the app sidebar's Chats list (`widgets/chat-rail`): "New folder"
 *   on the Chats caption (`shell-chats-new-folder`), a named field for the
 *   folder's name (`shell-chats-new-folder-name`), and the folder rows
 *   (`shell-chats-folder`). The first test drives that path end to end.
 *
 * Gap 2 (CLOSED, #128) — moving a conversation into a folder had no
 *   server-side effect: PUT .../conversation/.../{id} with `folder_id`
 *   returned 200 but the folder's `conversations` stayed [] and `total`
 *   stayed 0. The folder handler was never handed a DB pool, so the grouped
 *   listing read no conversations at all — which is also why `date_groups`
 *   came back empty for a project with nine conversations.
 *
 * Gap 3 (CLOSED, #128) — folder ordering was not modelled: a PUT carrying
 *   position/neighbor_above_id/neighbor_below_id returned 200 with no
 *   `position` in the response and no change in list order. Positions are
 *   now stored, resolved from the neighbour pair, and ordered DESC.
 *
 * What ALSO works, and is asserted green below: folder create / list /
 * rename / delete round-trip through the real backend.
 */
import type { APIRequestContext } from '@playwright/test';
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, API_BASE, DEFAULT_PROJECT_ID, createConversation } from '../../fixtures/api';

/** Suffix unique to this spec file so concurrent agents' fixtures never collide. */
const SUFFIX = '-fold';
const uniq = (label: string): string =>
  `${AUTOTEST_PREFIX}${label}${SUFFIX}-${Math.random().toString(36).slice(2, 8)}`;

const FOLDER_ROOT = `${API_BASE}/elitea_core/folder/prompt_lib/${DEFAULT_PROJECT_ID}`;
const CONVERSATION_ROOT = `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;

interface FolderRow {
  readonly id: string;
  readonly name: string;
  readonly total?: number;
  readonly conversations?: readonly { readonly id?: string; readonly name?: string }[];
}

/** POST a folder and return the server-assigned row. Throws on any non-201. */
async function createFolder(request: APIRequestContext, name: string): Promise<FolderRow> {
  const res = await request.post(FOLDER_ROOT, { data: { name } });
  expect(res.status(), `POST ${FOLDER_ROOT} body=${(await res.text()).slice(0, 300)}`).toBe(201);
  const body = (await res.json()) as FolderRow;
  expect(body.id, 'folder create must return a server-assigned id').toBeTruthy();
  return body;
}

/** GET the grouped folder listing — the exact call useFoldersListQuery makes. */
async function listFolders(request: APIRequestContext): Promise<readonly FolderRow[]> {
  const res = await request.get(`${FOLDER_ROOT}?grouped=true`);
  expect(res.status()).toBe(200);
  const body = (await res.json()) as { folders?: readonly FolderRow[] };
  return body.folders ?? [];
}

/*
 * The sidebar's Chats caption offers "New folder"; it opens a name field at
 * the top of the list. Escape (or leaving the field) cancels without a
 * request; Enter creates the folder, which then shows as a folder row.
 */
test('J10: the chat surface exposes the folder UI this journey needs', async ({ page }) => {
  // Real fixture: a conversation that the journey would drag into a folder.
  const conversationName = uniq('folder-src');
  await createConversation(page.request, conversationName);

  await page.goto(BASE_URL + '/app/chat');
  await page.waitForURL('**/chat**', { timeout: 15_000 });

  // Single real selector — no .or() chain.
  await expect(page.getByTestId('chat-message-input')).toBeVisible({ timeout: 10_000 });

  await checkA11y(page);

  // No fallback, no skip: this is the precondition for every remaining step
  // of JRNY-010.
  const createFolderButton = page.getByTestId('shell-chats-new-folder');
  await expect(createFolderButton, 'the sidebar Chats caption must offer "New folder"').toBeVisible({ timeout: 10_000 });
  await expect(createFolderButton).toHaveAccessibleName('New folder');
  await expect(createFolderButton).toBeEnabled();

  // Cancel first: Escape discards the field and creates nothing.
  const folderPosts: string[] = [];
  page.on('request', (request) => {
    if (request.method() === 'POST' && request.url().includes('/elitea_core/folder/prompt_lib/')) folderPosts.push(request.url());
  });
  await createFolderButton.click();
  const nameField = page.getByTestId('shell-chats-new-folder-name');
  await expect(nameField).toBeVisible();
  // The field HAS an accessible name, so it can be announced and targeted.
  await expect(nameField).toHaveAccessibleName('New folder name');
  await expect(nameField).toHaveValue('');
  await expect(nameField).toBeFocused();
  await nameField.press('Escape');
  await expect(nameField).toBeHidden();
  expect(folderPosts, 'a cancelled folder name must not create a folder').toEqual([]);

  // Then create: Enter saves the typed name, and the folder joins the list.
  const folderName = uniq('j10-folder');
  await createFolderButton.click();
  await expect(nameField).toBeVisible();
  const created = page.waitForResponse(
    (response) => response.request().method() === 'POST' && response.url().includes('/elitea_core/folder/prompt_lib/'),
    { timeout: 20_000 },
  );
  await nameField.fill(folderName);
  await nameField.press('Enter');
  const response = await created;
  expect(response.status(), 'the folder the field names must be created on the server').toBe(201);
  const folderId = ((await response.json()) as { id?: string }).id;
  try {
    await expect(nameField).toBeHidden();
    await expect(
      page.getByTestId('shell-chats').getByTestId('shell-chats-folder').filter({ hasText: folderName }),
      'the new folder must show in the sidebar list',
    ).toBeVisible({ timeout: 15_000 });
    expect((await listFolders(page.request)).map((folder) => folder.name)).toContain(folderName);
  } finally {
    if (folderId !== undefined) await page.request.delete(`${FOLDER_ROOT}/${folderId}`);
  }
});

test('J10: folder create/list/rename/delete round-trip through the backend', async ({ page }) => {
  const request = page.request;
  const name = uniq('crud');

  const created = await createFolder(request, name);
  expect(created.name).toBe(name);

  // Server-derived: the folder is addressable by the id the POST returned.
  const afterCreate = await listFolders(request);
  expect(afterCreate.map((f) => f.id)).toContain(created.id);
  expect(afterCreate.find((f) => f.id === created.id)?.name).toBe(name);

  // Rename persists — PUT is the same call useFolderUpdateMutation issues.
  const renamed = `${name}-renamed`;
  const put = await request.put(`${FOLDER_ROOT}/${created.id}`, { data: { name: renamed } });
  expect(put.status()).toBe(200);

  const afterRename = await listFolders(request);
  expect(afterRename.find((f) => f.id === created.id)?.name).toBe(renamed);

  // Delete removes it from the listing.
  const del = await request.delete(`${FOLDER_ROOT}/${created.id}`);
  expect(del.status()).toBe(204);

  const afterDelete = await listFolders(request);
  expect(afterDelete.map((f) => f.id)).not.toContain(created.id);
});

test('J10: a conversation moved into a folder is grouped under it', async ({ page }) => {
  const request = page.request;
  const conversationName = uniq('grouped-conv');
  const conversationId = await createConversation(request, conversationName);
  const folder = await createFolder(request, uniq('grouped'));

  // The move the drop handler performs: conversationApi.edit({ folder_id })
  // — useMoveToFolderConversation.hooks.ts:163.
  const move = await request.put(`${CONVERSATION_ROOT}/${conversationId}`, {
    data: { folder_id: folder.id },
  });
  expect(move.status()).toBe(200);

  // The literal JRNY-010 acceptance: "the conversation is grouped under the
  // folder".
  const folders = await listFolders(request);
  const row = folders.find((f) => f.id === folder.id);
  expect(row, `folder ${folder.id} missing from grouped listing`).toBeDefined();
  expect(
    (row?.conversations ?? []).map((c) => c.name),
    'conversation moved via folder_id is not returned under its folder',
  ).toContain(conversationName);
  expect(row?.total, 'folder.total must count the moved conversation').toBeGreaterThan(0);

  // The conversation's own details must agree with the grouping — the two
  // used to disagree (the listing knew nothing, and details carried no
  // folder_id field at all), so asserting only the listing would let a
  // half-applied move pass.
  const details = await request.get(`${CONVERSATION_ROOT}/${conversationId}`);
  expect(details.status()).toBe(200);
  expect((await details.json()).folder_id, 'conversation details must report the folder it was moved into').toBe(folder.id);

  // The single-folder pagination fetcher (folderApi.folderConversations) must
  // see it too: it is a separate code path from the grouped listing and used
  // to ignore folder_id entirely, returning the folder list instead.
  const page1 = await request.get(`${FOLDER_ROOT}?grouped=true&folder_id=${folder.id}&limit=10&offset=0`);
  expect(page1.status()).toBe(200);
  const body = (await page1.json()) as { conversations?: readonly { name?: string }[]; total?: number };
  expect((body.conversations ?? []).map((c) => c.name), 'folder_id filter must return that folder\'s conversations').toContain(conversationName);
  expect(body.total).toBeGreaterThan(0);
});

test('J10: a conversation is addressable by uuid and keeps its owner across an update', async ({ page }) => {
  const request = page.request;
  const conversationName = uniq('ident');
  const conversationId = await createConversation(request, conversationName);

  // The numeric-id form is the one the app uses everywhere; read the uuid off it.
  const byId = await request.get(`${CONVERSATION_ROOT}/${conversationId}`);
  expect(byId.status()).toBe(200);
  const detail = (await byId.json()) as { uuid?: string; created_by?: string };
  expect(detail.uuid, 'conversation details must expose the uuid this test addresses it by').toBeTruthy();
  expect(detail.created_by, 'a conversation must report its owner').toBeTruthy();

  // Addressing the SAME conversation by uuid used to be a 500 — the id column
  // is a SERIAL, so Postgres raised a type error and the route reported the
  // server as broken. A uuid identifies the row, so it must resolve to it.
  const byUuid = await request.get(`${CONVERSATION_ROOT}/${detail.uuid}`);
  expect(byUuid.status(), `GET by uuid: ${(await byUuid.text()).slice(0, 200)}`).toBe(200);
  expect((await byUuid.json()).id, 'the uuid must resolve to the same conversation').toBe(conversationId);

  // An identifier that can never name a row is a 404, not a 500 — the
  // distinction between "no such conversation" and "the server broke".
  const garbage = await request.get(`${CONVERSATION_ROOT}/definitely-not-an-identifier`);
  expect(garbage.status(), 'an unsupported identifier must be 404, not 500').toBe(404);

  // The PUT response used to drop created_by (returning ""), so a client that
  // refreshed its cache from the mutation response lost the owner.
  const put = await request.put(`${CONVERSATION_ROOT}/${conversationId}`, { data: { name: `${conversationName}-renamed` } });
  expect(put.status()).toBe(200);
  expect((await put.json()).created_by, 'PUT must report the same created_by the details endpoint does').toBe(detail.created_by);
});

test('J10: folder reordering persists', async ({ page }) => {
  const request = page.request;
  const first = await createFolder(request, uniq('order-a'));
  const second = await createFolder(request, uniq('order-b'));

  const orderOf = (folders: readonly FolderRow[]): readonly string[] =>
    folders.map((f) => f.id).filter((id) => id === first.id || id === second.id);

  // Move `second` above `first` with the exact payload useReorderFolders.ts:79-97 sends.
  const reorder = await request.put(`${FOLDER_ROOT}/${second.id}`, {
    data: {
      name: second.name,
      position: 0,
      neighbor_above_id: null,
      neighbor_below_id: first.id,
    },
  });
  expect(reorder.status()).toBe(200);

  // "Reordering persists" means the listing order changed and stays changed.
  // Order comparison, not visibility: two fresh GETs.
  expect(orderOf(await listFolders(request)), 'reorder had no effect on list order').toEqual([second.id, first.id]);
  expect(orderOf(await listFolders(request)), 'reordered order did not survive a refetch').toEqual([second.id, first.id]);

  // The leg above is NOT on its own discriminating: a freshly created folder
  // already sorts above an older one, so `[second, first]` is also the order
  // a server that ignored the PUT entirely would report. Moving `first` back
  // to the top is the assertion that can only pass if the reorder is real —
  // it demands an order the creation sequence never produces.
  const reorderBack = await request.put(`${FOLDER_ROOT}/${first.id}`, {
    data: {
      name: first.name,
      position: 0,
      neighbor_above_id: null,
      neighbor_below_id: second.id,
    },
  });
  expect(reorderBack.status()).toBe(200);
  expect(reorderBack.status() === 200 && 'position' in (await reorderBack.json()), 'reorder response must report the resolved position').toBe(true);

  expect(orderOf(await listFolders(request)), 'reordering the older folder to the top had no effect').toEqual([first.id, second.id]);
  expect(orderOf(await listFolders(request)), 'the re-reordered order did not survive a refetch').toEqual([first.id, second.id]);

  // A rename must not disturb the order it just established — the two share
  // one PUT route, and a full-replacement PUT that defaulted the missing
  // `position` to 0 would silently send the folder to the bottom.
  const rename = await request.put(`${FOLDER_ROOT}/${first.id}`, { data: { name: `${first.name}-renamed` } });
  expect(rename.status()).toBe(200);
  expect(orderOf(await listFolders(request)), 'a rename moved the folder').toEqual([first.id, second.id]);
});
