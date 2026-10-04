/**
 * The composer's "#" agent/pipeline picker (#6774).
 *
 * Typing "#" used to put the composer into a picker mode with no picker on
 * screen. Send stayed disabled until the "#" was deleted, so the chat looked
 * frozen. These journeys pin the fixed behaviour: the picker appears, it says
 * so when nothing matches, Send stays enabled, a space ends the query, and a
 * pick attaches the agent and removes the typed "#query".
 *
 * Chromium lane: nothing here sends a turn. The keystrokes go through
 * `pressSequentially`, because `.fill()` never runs the detection hooks (see
 * `chat.mentions.spec.ts`).
 */
import { test, expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  AUTOTEST_PREFIX,
  createAgent,
  createConversation,
  deleteAgent,
  deleteConversation,
} from '../../fixtures/api';

const SUFFIX = '-hash';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}${SUFFIX}`;
}

async function openComposer(page: Page, conversationId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await input.click();
}

/* #6774: "#" opens the picker with an empty state, Send stays enabled, and a space ends the query */
test('#6774: "#" shows the picker and its empty state, and never blocks Send', async ({ page }) => {
  const conversationId = await createConversation(page.request, uniqueName('empty'));
  try {
    await openComposer(page, conversationId);
    const input = page.getByTestId('chat-message-input');
    const picker = page.getByTestId('chat-hash-participant-popup');

    await input.pressSequentially('see #zzz-no-such-agent', { delay: 15 });
    await expect(picker, 'typing "#" must show the agent/pipeline picker').toBeVisible({ timeout: 10_000 });
    await expect(picker.getByText('No matching results')).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId('chat-send-button'), 'an open picker must not disable Send').toBeEnabled();

    await input.pressSequentially(' is fixed', { delay: 15 });
    await expect(picker, 'a space ends the "#" query').toHaveCount(0);
    await expect(input).toHaveValue('see #zzz-no-such-agent is fixed');
    await expect(page.getByTestId('chat-send-button')).toBeEnabled();
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});

/* #6774: picking an agent from the "#" picker attaches it and removes the typed query */
test('#6774: picking an agent attaches it and removes the typed "#query"', async ({ page }) => {
  const agentName = uniqueName('agent');
  const agent = await createAgent(page.request, agentName);
  const conversationId = await createConversation(page.request, uniqueName('pick'));
  try {
    await openComposer(page, conversationId);
    const input = page.getByTestId('chat-message-input');
    const picker = page.getByTestId('chat-hash-participant-popup');

    await input.pressSequentially(`hello #${agentName}`, { delay: 15 });
    await expect(picker).toBeVisible({ timeout: 10_000 });
    await picker.getByText(agentName, { exact: true }).click({ timeout: 15_000 });

    await expect(picker).toHaveCount(0);
    await expect(input, 'the pick removes the typed "#query"').not.toHaveValue(/#/);
    await expect(page.getByText(agentName).first(), 'the agent is attached to the chat').toBeVisible({ timeout: 15_000 });
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteAgent(page.request, agent.id);
  }
});
