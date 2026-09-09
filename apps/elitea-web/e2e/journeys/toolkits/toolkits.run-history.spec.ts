/**
 * Journey: a toolkit's run-history tab (issue #868).
 *
 * `features/toolkits/ui/ConfigurationTab.tsx`'s `renderRunHistory` slot is a
 * real caller here (unlike agents' — see `agents.run-history.spec.ts`'s own
 * header): `pages/toolkits/EditToolkit.tsx` renders this `ConfigurationTab`
 * directly, and `ViewRunHistoryButton` (default testid `pipeline-history-tab`,
 * unchanged by this fix) already toggled `showHistory` before this fix —
 * clicking it just rendered nothing. This journey proves it now renders the
 * real `@/entities/run-history` panel, and that its row list matches the
 * server's own `GET /elitea_core/conversations/prompt_lib/{projectId}`
 * answer for this toolkit's `entity_name=toolkit` filter.
 */
import { expect, test } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createConversation, deleteConversation } from '../../fixtures/api';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}`;
}

interface ConversationSummary {
  readonly id: number | string;
  readonly name: string;
}

async function createArtifactToolkit(request: APIRequestContext, name: string): Promise<string> {
  const created = await request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: { name, type: 'artifact', settings: { selected_tools: ['list_files'] } },
  });
  expect(created.status(), `creating the toolkit answered ${created.status()}: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const toolkitId = String(((await created.json()) as { id?: string | number }).id ?? '');
  expect(toolkitId, 'the created toolkit must carry an id').not.toBe('');
  return toolkitId;
}

async function attachToolkitParticipant(request: APIRequestContext, conversationId: string, toolkitId: string): Promise<void> {
  const response = await request.post(
    `${API_BASE}/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
    { data: [{ entity_name: 'toolkit', entity_meta: { id: toolkitId, project_id: DEFAULT_PROJECT_ID } }] },
  );
  expect(
    response.status(),
    `attaching the toolkit participant must succeed: ${(await response.text()).slice(0, 300)}`,
  ).toBe(200);
}

async function readToolkitConversations(request: APIRequestContext, toolkitId: string): Promise<readonly ConversationSummary[]> {
  const response = await request.get(
    `${API_BASE}/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}` +
      `?entity_name=toolkit&entity_meta_id=${toolkitId}&limit=50`,
  );
  expect(response.status()).toBe(200);
  const body = (await response.json()) as { rows?: readonly ConversationSummary[] };
  return body.rows ?? [];
}

test('toolkit History tab lists the server\'s own conversations for this toolkit', async ({ page }) => {
  const toolkitName = uniqueName('runhist-toolkit');
  const toolkitId = await createArtifactToolkit(page.request, toolkitName);
  const conversationName = uniqueName('runhist-convo');
  const conversationId = await createConversation(page.request, conversationName);

  await attachToolkitParticipant(page.request, conversationId, toolkitId);

  try {
    const serverRows = await readToolkitConversations(page.request, toolkitId);
    expect(serverRows.map((r) => String(r.id))).toContain(conversationId);

    await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });

    const historyButton = page.getByTestId('pipeline-history-tab');
    await expect(historyButton).toBeVisible({ timeout: 20_000 });
    await historyButton.click();

    const panel = page.getByTestId('run-history-panel');
    await expect(panel).toBeVisible({ timeout: 20_000 });

    const row = panel.getByTestId('run-history-row');
    await expect(row).toHaveCount(1, { timeout: 20_000 });
    await expect(row).toContainText(conversationName);

    // Toolkits carry no `onRestoreConversation` (no live chat pane to restore
    // into) — the row therefore offers no Restore action.
    await expect(row.getByTestId('run-history-restore-button')).toHaveCount(0);

    await panel.getByTestId('run-history-close').click();
    await expect(panel).not.toBeVisible();
  } finally {
    await deleteConversation(page.request, conversationId);
    await page.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${toolkitId}`).catch(() => {});
  }
});
