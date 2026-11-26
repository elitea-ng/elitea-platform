/**
 * Live agent presence in the web chat (ADR-0061 decision 4c, #3067), against a fixture transcript and a held-open
 * fixture stream (`fixtures/teamPresence.ts` says why): a cloud agent's `agent_state` frame "working" on a tool reads
 * "Running jira_get_issue" on its participants row, and an agent waiting on another reads "Waiting for Mira".
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createConversation, deleteConversation } from '../../fixtures/api';
import { openParticipants } from '../../fixtures/sidePanel';
import { serveTeamPresence } from '../../fixtures/teamPresence';

test.describe('agent presence in the chat (fixture stream)', () => {
  let conversationId = '';

  test.beforeEach(async ({ request }) => {
    conversationId = await createConversation(request, `${AUTOTEST_PREFIX}agent-presence-${Date.now()}`);
  });

  test.afterEach(async ({ request }) => {
    if (conversationId !== '') await deleteConversation(request, conversationId);
  });

  test('a cloud agent running a tool reads "Running <tool>"', async ({ page }) => {
    await serveTeamPresence(page, conversationId);
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    // The agent rows live in the side panel, which is closed at the default 1280 px window.
    await openParticipants(page);

    const mira = page.locator('[data-testid="participant-state"][data-participant-id="2"]');
    await expect(mira).toContainText('Running jira_get_issue', { timeout: 20_000 });
    await expect(page.locator('[data-testid="participant-state"][data-participant-id="3"]')).toContainText('Waiting for Mira');
  });
});
