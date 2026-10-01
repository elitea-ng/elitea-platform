/**
 * elitea_issues #6283 — "Team Project Conversations: Allow the Creator to
 * Revert a Public Conversation to Restricted Access". Reproduced: before this
 * package's fix, `ConversationItem.menu.tsx` rendered "Make public" only
 * while `conversation.isPrivate` was true and rendered NO replacement once it
 * went public — the creator had no menu path back. The server permits
 * restriction only for the creator.
 * The client affordance is a "Restrict access" entry that PUTs `is_private: true`.
 *
 * SCOPE CUT (also recorded in `S/issues/gaps.md`): the issue's full request
 * additionally wants a participant-selection step ("choose who keeps
 * access") before restricting. That step is NOT built — restricting reverts
 * to the conversation's participant list exactly as it already stands. This
 * journey pins the part that is built: the server write, and the menu
 * flipping back to "Make public" afterwards (`ConversationItem.menu.test.tsx`
 * covers the component-level branching this depends on).
 *
 * Reuses `chat.sharing.spec.ts`'s own fixtures/helpers rather than
 * reimporting duplicates of them, since this is the direct continuation of
 * that file's S1 case.
 */
import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  createConversation,
  deleteConversation,
  describeRefusal,
  setConversationPrivacy,
  PUBLISH_AUTHOR_PROJECT_NAME,
  readCallerIdentity,
  resolvePublishAuthorProjectId,
} from '../../fixtures/api';
import { ensureProjectSelected } from '../../fixtures/project';

const SUFFIX = '-restrict';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}${SUFFIX}`;
}

/** `is_private` as the server holds it — same shape as `chat.sharing.spec.ts`'s own reader. */
async function readIsPrivate(page: Page, conversationId: string, projectId: string): Promise<boolean> {
  const url = `${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`;
  const response = await page.request.get(url);
  if (!response.ok()) {
    throw new Error(`readIsPrivate: GET ${url} -> ${response.status()} ${response.statusText()}${await describeRefusal(response)}`);
  }
  const body = (await response.json()) as { is_private?: unknown };
  return body.is_private !== false;
}

async function openTodayGroup(page: Page): Promise<void> {
  const sidebar = page.getByTestId('chat-conversation-sidebar');
  await expect(sidebar).toBeVisible({ timeout: 20_000 });
  const today = sidebar.getByRole('button', { name: 'Today' });
  await expect(today).toBeVisible({ timeout: 20_000 });
  if ((await today.getAttribute('aria-expanded')) !== 'true') await today.click();
}

async function openChat(page: Page): Promise<void> {
  await page.goto(BASE_URL + '/app/chat');
  await ensureProjectSelected(page, PUBLISH_AUTHOR_PROJECT_NAME);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  await openTodayGroup(page);
}

/** Same retried hover+click gesture `chat.sharing.spec.ts` documents at length — the row menu trigger only mounts on hover, and the rail can reorder under a resting pointer. */
async function openRowMenu(page: Page, conversationId: string): Promise<void> {
  const row = page.getByTestId(`conversation-item-${conversationId}`);
  await expect(row).toBeVisible({ timeout: 20_000 });
  const menu = page.getByRole('menu');
  await expect(async () => {
    if ((await menu.count()) === 0) {
      await row.hover();
      await row.getByRole('button', { name: 'More actions' }).click({ timeout: 5_000 });
    }
    await expect(menu).toBeVisible({ timeout: 5_000 });
  }).toPass({ timeout: 40_000 });
}

async function closeRowMenu(page: Page): Promise<void> {
  await page.keyboard.press('Escape');
  await expect(page.getByRole('menu')).toHaveCount(0, { timeout: 10_000 });
}

test('R1: "Restrict access" reverts a public conversation to private on the server, and the menu offers "Make public" again', async ({ page, browser }) => {
  test.setTimeout(120_000);
  const projectId = await resolvePublishAuthorProjectId(page.request);
  const conversationId = await createConversation(page.request, uniqueName('restrict'), projectId);
  const otherMember = await browser.newContext({ storageState: STORAGE_STATE.admin });
  try {
    await setConversationPrivacy(page.request, conversationId, false, projectId);
    expect(await readIsPrivate(page, conversationId, projectId), 'setup: the conversation must be public before this case starts').toBe(false);

    expect((await readCallerIdentity(otherMember.request)).id).not.toBe((await readCallerIdentity(page.request)).id);
    const url = `${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`;
    expect((await otherMember.request.get(url)).status(), 'a public chat must be visible to another team member').toBe(200);
    expect((await otherMember.request.put(url, { data: { is_private: true } })).status(), 'a noncreator cannot restrict access').toBe(400);
    expect(await readIsPrivate(page, conversationId, projectId)).toBe(false);

    await openChat(page);
    await openRowMenu(page, conversationId);

    // "Restrict access" replaces "Make public" once public — never both.
    await expect(page.getByRole('menuitem', { name: 'Make public' })).toHaveCount(0);
    await page.getByRole('menuitem', { name: 'Restrict access' }).click();
    await expect(page.getByText('Project members who are not participants will no longer be able to open this conversation. Continue?')).toBeVisible({ timeout: 10_000 });
    await page.getByRole('menuitem', { name: 'Restrict access' }).click();

    // THE SERVER, not the optimistic patch — same discipline S1 uses for the opposite direction.
    await expect
      .poll(async () => readIsPrivate(page, conversationId, projectId), {
        timeout: 20_000,
        message: 'the conversation must be reverted to private on the server, not only in the list the client is holding',
      })
      .toBe(true);

    expect((await otherMember.request.get(url)).status(), 'restriction must remove access for a nonparticipant').toBe(404);

    // And the round trip: a reload's sidebar listing is what tells the menu
    // this conversation is private again, so "Make public" is offered once more.
    await openChat(page);
    await openRowMenu(page, conversationId);
    await expect(page.getByRole('menuitem', { name: 'Restrict access' }), 'a restricted conversation must not still offer "Restrict access"').toHaveCount(0);
    await expect(page.getByRole('menuitem', { name: 'Make public' })).toBeVisible({ timeout: 10_000 });
    await closeRowMenu(page);
  } finally {
    await otherMember.close();
    await deleteConversation(page.request, conversationId, projectId);
  }
});
