/**
 * Onetest wave-1 tail (T1b, folder `agents`) — ELITEA-0105, the Run History
 * panel's own CONTENT (title, columns, close). `agents.run-history.spec.ts`
 * already proves the tab opens, lists the server's own conversations and
 * loads one on click — this file covers what that journey does not: the
 * panel's title text, its close control, and the onetest case's specific
 * "3 columns: Date, Version, Duration" claim.
 *
 * Measured directly against `RunHistoryPanel.tsx`/`RunHistoryList.tsx`: the
 * title ("Run history") and a close (X) button are both real. The COLUMN
 * claim is not — there is no table and no per-row VERSION at all;
 * `RunHistoryList`'s `secondaryOf` renders exactly `date · duration ·
 * N messages`, with no version name anywhere in that string or in a
 * dedicated column.
 */
import { expect, test } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createAgent, createConversation, deleteAgent, deleteConversation } from '../../fixtures/api';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}`;
}

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
  expect(response.status(), `attaching the agent participant must succeed: ${(await response.text()).slice(0, 300)}`).toBe(
    200,
  );
}

/* onetest: ELITEA-0105 (real half) — the Run History panel opens titled "Run history", and its close (X) control closes it back to the plain editor. */
test('the Run History panel is titled "Run history" and its close control closes it', async ({ page, request }) => {
  const agentName = uniqueName('runhist-title-agent');
  const agent = await createAgent(page.request, agentName);
  const conversationName = uniqueName('runhist-title-convo');
  const conversationId = await createConversation(page.request, conversationName);
  await attachAgentParticipant(page.request, conversationId, agent);

  try {
    await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
    await expect(page.getByTestId('edit-application-editor-tabs')).toBeVisible({ timeout: 20_000 });
    await page.getByTestId('edit-application-tab-history').click();

    const panel = page.getByTestId('run-history-panel');
    await expect(panel).toBeVisible({ timeout: 20_000 });
    await expect(panel.getByText('Run history', { exact: true })).toBeVisible();

    const row = panel.getByTestId('run-history-row');
    await expect(row).toHaveCount(1, { timeout: 20_000 });

    await panel.getByTestId('run-history-close').click();
    await expect(panel).toHaveCount(0);
    await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible();
  } finally {
    await deleteConversation(request, conversationId);
    await deleteAgent(request, agent.id);
  }
});

/* onetest: ELITEA-0105 — product gap: the panel is claimed to show a 3-column table (Date, Version, Duration) per run; it shows neither a table nor a version name at all — only `date · duration · N messages`. */
test('a Run History row names the version that produced it', async ({ page, request }) => {
  const agentName = uniqueName('runhist-version-agent');
  const agent = await createAgent(page.request, agentName);
  const conversationName = uniqueName('runhist-version-convo');
  const conversationId = await createConversation(page.request, conversationName);
  await attachAgentParticipant(page.request, conversationId, agent);

  try {
    await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
    await expect(page.getByTestId('edit-application-editor-tabs')).toBeVisible({ timeout: 20_000 });
    await page.getByTestId('edit-application-tab-history').click();

    const panel = page.getByTestId('run-history-panel');
    const row = panel.getByTestId('run-history-row');
    await expect(row).toHaveCount(1, { timeout: 20_000 });

    test.fail(
      true,
      'ELITEA-0105 (#955): product gap — RunHistoryList.secondaryOf (src/entities/run-history/ui/RunHistoryList.tsx) ' +
        'renders `date · duration · N messages` only; no version name is read or shown anywhere on the row',
    );

    // The version this agent's sole version is named — `base`, from
    // `createAgent`'s own fixture body.
    await expect(row).toContainText('base');
  } finally {
    await deleteConversation(request, conversationId);
    await deleteAgent(request, agent.id);
  }
});
