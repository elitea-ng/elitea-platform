/**
 * Journey: an agent's run-history tab (issue #868).
 *
 * `features/agents/ui/ConfigurationTab.tsx` declares a `renderRunHistory`
 * slot, but that component is not mounted anywhere in this app —
 * `pages/agents/EditApplication.tsx` composes `EditApplicationEditorTabs` +
 * `EditApplicationConfigurationPanel` instead. The real fix (see
 * `EditApplicationEditorTabs.tsx`'s own doc comment) is a third "History" tab
 * in that strip, backed by `@/entities/run-history`'s `RunHistoryPanel` —
 * this journey exercises THAT tab, on the real agent editor page.
 *
 * Seeds a conversation that carries the agent as its sole participant (the
 * `entity_name=application`/`entity_meta_id` shape `listConversations`
 * filters on — internal/api/v2/conversations/handler.go's `List`), and
 * asserts the History tab's row matches what the server's own
 * `GET /elitea_core/conversations/prompt_lib/{projectId}` answers for that
 * filter — the "server's trace list" the issue's own DoD names.
 */
import { expect, test } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createAgent,
  createConversation,
  deleteAgent,
  deleteConversation,
} from '../../fixtures/api';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}`;
}

interface ConversationSummary {
  readonly id: number | string;
  readonly name: string;
}

/** Attach the agent as the conversation's sole participant — the same shape chat.management.spec.ts's M2 establishes. */
async function attachAgentParticipant(
  request: APIRequestContext,
  conversationId: string,
  agent: { readonly id: string; readonly versionId: string },
): Promise<void> {
  const response = await request.post(
    `${API_BASE}/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
    {
      data: [
        {
          entity_name: 'application',
          entity_meta: { id: agent.id, project_id: DEFAULT_PROJECT_ID },
          entity_settings: { version_id: agent.versionId },
        },
      ],
    },
  );
  expect(
    response.status(),
    `attaching the agent participant must succeed: ${(await response.text()).slice(0, 300)}`,
  ).toBe(200);
}

/** The server's own answer for this agent's run history — what the UI is checked against. */
async function readAgentConversations(
  request: APIRequestContext,
  agentId: string,
): Promise<readonly ConversationSummary[]> {
  const response = await request.get(
    `${API_BASE}/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}` +
      `?entity_name=application&entity_meta_id=${agentId}&limit=50`,
  );
  expect(response.status()).toBe(200);
  const body = (await response.json()) as { rows?: readonly ConversationSummary[] };
  return body.rows ?? [];
}

test('agent History tab lists the server\'s own conversations for this agent', async ({ page }) => {
  const agentName = uniqueName('runhist-agent');
  const agent = await createAgent(page.request, agentName);
  const conversationName = uniqueName('runhist-convo');
  const conversationId = await createConversation(page.request, conversationName);

  await attachAgentParticipant(page.request, conversationId, agent);

  try {
    const serverRows = await readAgentConversations(page.request, agent.id);
    expect(serverRows.map((r) => String(r.id))).toContain(conversationId);

    await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
    const tabs = page.getByTestId('edit-application-editor-tabs');
    await expect(tabs).toBeVisible({ timeout: 20_000 });

    await page.getByTestId('edit-application-tab-history').click();

    const panel = page.getByTestId('run-history-panel');
    await expect(panel).toBeVisible({ timeout: 20_000 });

    const row = panel.getByTestId('run-history-row');
    await expect(row).toHaveCount(1, { timeout: 20_000 });
    await expect(row).toContainText(conversationName);

    // Opening it shows the (empty, since nothing executed in this stack) trace
    // view rather than nothing — the panel's "select a run" placeholder is gone.
    await row.click();
    await expect(panel.getByTestId('run-history-no-selection')).not.toBeVisible();
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteAgent(page.request, agent.id);
  }
});
