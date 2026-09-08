/**
 * Starting a conversation from the UI, and what a brand-new one starts as.
 *
 * Ported by use case from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/chat/test_conversation_management.py`):
 *
 *  - `TestCreateConversation::test_create_conversation_via_ui_button` (TC-CONV-001 UI)
 *  - `TestCreateConversation::test_new_conversation_default_settings` (TC-CONV-002)
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT "CREATE A CONVERSATION" IS IN THIS APP
 * ─────────────────────────────────────────────────────────────────────────────
 * There is no route that creates an empty conversation from a button. The
 * shell's "+ Create" resolves `chat` to `/chat?create=1`
 * (`widgets/create-button`'s `COMMAND_TARGETS`), and `useCreateChatReset` reads
 * that flag, calls the composition root's reset and REMOUNTS the chat subtree
 * through a key. The row in the database is written later, by the first send
 * (`resolveConversationForSend`). Both halves are asserted below, because the
 * flag's own defect was that only the first half existed: the button navigated
 * to a pathname the user was already on, nothing read `create`, the route did
 * not remount, and the previous transcript, attachments and streaming state all
 * stayed on screen — only a manual reload started a second chat. This journey
 * therefore starts INSIDE an existing conversation with text in the composer,
 * so "it reset" is something that can be observed at all.
 */
import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  clickCreateButton,
  createConversation,
  deleteConversation,
} from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-new';

const CONVERSATIONS_PATH = `/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}`;
/** The grouped listing the rail itself reads (`entities/folder`'s `foldersList`). */
const FOLDERS_PATH = `/elitea_core/folder/prompt_lib/${DEFAULT_PROJECT_ID}`;
const CONVERSATION_PATH = `/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;
const MODEL_CATALOGUE = `${API_BASE}/configurations/models/${DEFAULT_PROJECT_ID}?include_shared=true`;

/** `ChatBox` derives the conversation name from the question, truncated to 50 chars. */
const MAX_NAME = 50;

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}${SUFFIX}`;
}

/** See `chat.management.spec.ts`'s M1: `DateGroup` starts collapsed, so its rows are mounted but not visible. */
async function openTodayGroup(page: Page): Promise<void> {
  const sidebar = page.getByTestId('chat-conversation-sidebar');
  await expect(sidebar).toBeVisible({ timeout: 20_000 });
  const today = sidebar.getByRole('button', { name: 'Today' });
  await expect(today).toBeVisible({ timeout: 20_000 });
  if ((await today.getAttribute('aria-expanded')) !== 'true') await today.click();
}

// ─────────────────────────────────────────────────────────────────────────────
// legacy: test_conversation_management.py::TestCreateConversation::test_create_conversation_via_ui_button
// ─────────────────────────────────────────────────────────────────────────────
test('the Create control leaves the open conversation for a blank one, and the next send stores it', async ({ page }) => {
  const existingName = uniqueName('existing');
  const existing = await createConversation(page.request, existingName);

  await page.goto(`${BASE_URL}/app/chat/${existing}`);
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });

  // A draft in the composer of the conversation being left. This is the state
  // the `?create=1` defect preserved, and the only thing on this screen that
  // proves a remount rather than a route change.
  await input.fill(`${AUTOTEST_PREFIX}draft-that-must-not-survive`);
  await expect(input).not.toHaveValue('');

  await clickCreateButton(page);

  // The route leaves the conversation. `toHaveURL`, not `waitForURL`: this is a
  // client-side history change, which fires no navigation lifecycle event.
  await expect(page).toHaveURL(/\/app\/chat(\?|$)/, { timeout: 15_000 });
  // …and the chat subtree really was rebuilt: the draft is gone. A route change
  // that reused the same `ChatBox` instance would still be holding it.
  await expect(page.getByTestId('chat-message-input')).toHaveValue('', { timeout: 15_000 });
  await expect(page.getByTestId('user-message')).toHaveCount(0);

  await checkA11y(page);

  // The second half: the first send is what writes the row.
  const text = uniqueName('created');
  expect(text.length, 'the message must fit the 50-char name truncation').toBeLessThanOrEqual(MAX_NAME);
  const created = page.waitForResponse(
    (response) => response.url().includes(CONVERSATIONS_PATH) && response.request().method() === 'POST',
    { timeout: 20_000 },
  );
  await page.getByTestId('chat-message-input').fill(text);
  await page.getByTestId('chat-send-button').click();
  const response = await created;
  expect(response.status()).toBe(201);
  const body = (await response.json()) as { id?: string; name?: string };
  expect(body.name).toBe(text);
  // A NEW conversation, not the one the user left — the whole point of Create.
  expect(body.id).not.toBe(existing);

  // The SERVER first, on the rail's own endpoint. Without this the UI assert
  // below reports "element not found" for two unrelated findings — the listing
  // does not have the conversation yet, or it does and the rail was never told
  // — and the report keeps neither. Polled, not read once: the create and the
  // listing are two requests.
  await expect
    .poll(
      async () => {
        const listing = await page.request.get(
          `${API_BASE}${FOLDERS_PATH}?grouped=true&sort_by=updated_at&sort_order=desc`,
        );
        if (!listing.ok()) return [];
        const grouped = (await listing.json()) as {
          date_groups?: readonly { name: string; conversations?: readonly { id: string | number }[] }[];
        };
        const today = grouped.date_groups?.find((group) => group.name === 'Today');
        return (today?.conversations ?? []).map((conversation) => String(conversation.id));
      },
      { timeout: 20_000, message: 'the created conversation never reached the rail’s own listing endpoint' },
    )
    .toContain(String(body.id));

  // It reaches the rail beside the one it was created from.
  await openTodayGroup(page);
  await expect(page.getByTestId(`conversation-item-${body.id}`)).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId(`conversation-item-${existing}`)).toBeVisible();

  await deleteConversation(page.request, body.id as string);
  await deleteConversation(page.request, existing);
});

// ─────────────────────────────────────────────────────────────────────────────
// legacy: test_conversation_management.py::TestCreateConversation::test_new_conversation_default_settings
//
// The legacy test asserted one thing — that the model selector showed
// something. "Default settings" is a bigger statement than that, and every part
// of it is cheap to read here: no transcript, no participants, no modules, and
// a model that is the project's default rather than the picker's `None`
// fallback. Each of those is a real default a regression could break
// independently (a conversation created with a stale participant list, or with
// internal tools inherited from the last one, are both shapes the product has
// produced).
// ─────────────────────────────────────────────────────────────────────────────
test('a conversation opens with an empty transcript, no participants, no modules and the default model', async ({ page }) => {
  const catalogue = await page.request.get(MODEL_CATALOGUE);
  expect(catalogue.status()).toBe(200);
  const items = ((await catalogue.json()) as { items?: readonly { name: string; display_name?: string; default?: boolean }[] }).items ?? [];
  expect(items.length, 'this stack seeds at least one llm-section model row').toBeGreaterThan(0);
  const expectedModel = items.find((model) => model.default === true) ?? items[0];
  const expectedLabel = expectedModel?.display_name ?? expectedModel?.name ?? '';

  const conversationId = await createConversation(page.request, uniqueName('defaults'));

  // The server's own answer for a conversation nobody has configured, read
  // before the page sees it: this is what "default" means, and the screen is
  // then checked against it rather than against a constant.
  const detail = await page.request.get(`${API_BASE}${CONVERSATION_PATH}/${conversationId}`);
  expect(detail.status()).toBe(200);
  const stored = (await detail.json()) as {
    participants?: readonly unknown[];
    meta?: { internal_tools?: readonly string[] };
  };
  expect(stored.participants ?? []).toEqual([]);
  expect(stored.meta?.internal_tools ?? []).toEqual([]);

  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });
  await expect(page.getByText('Something went wrong.')).toHaveCount(0);

  // Empty transcript and empty composer.
  await expect(page.getByTestId('chat-message-input')).toHaveValue('');
  await expect(page.getByTestId('user-message')).toHaveCount(0);

  // The project's default model, not the picker's empty-catalogue fallback.
  await expect(page.getByTestId('model-selector-name')).toHaveText(expectedLabel, { timeout: 15_000 });
  expect(expectedLabel).not.toBe('None');

  // No participants: the rail renders a section only for participants that
  // exist, so both sections must be absent rather than empty.
  const expand = page.getByRole('button', { name: 'Expand participants' });
  await expect(expand).toBeVisible({ timeout: 20_000 });
  await expand.click();
  await expect(page.getByTestId('participants-container')).toBeVisible({ timeout: 10_000 });
  await expect(page.getByTestId('participants-section-Agents')).toHaveCount(0);
  await expect(page.getByTestId('users-section')).toHaveCount(0);

  await checkA11y(page);
  await deleteConversation(page.request, conversationId);
});
