/**
 * P13 rejudge — the generic per-toolkit/per-agent/per-MCP Run History
 * TRACE detail (ELITEA-2802 `indexing`, ELITEA-2803 `mcp-servers`, ELITEA-
 * 2805 `toolkits-indexes/indexing-summaries`). All three ask about the SAME
 * screen: `entities/run-history/ui/RunHistoryTrace.tsx`, reached from
 * `toolkits.run-history.spec.ts`'s already-proven `run-history-panel` →
 * click a row → trace pane. That file proves the row LIST matches the
 * server; this file is about what happens once a row is SELECTED.
 *
 * `RunHistoryTrace.tsx`'s `StepDetail` component renders exactly two fields
 * off a fetched `MessageTraceStepDetail`: `text` and `tool_output` (as
 * pre-formatted monospace). The type also carries `tool_inputs` and
 * `thinking` (`messageTraceStepDetail.zod.ts`) — fetched by the SAME
 * `useGetMessageTrace` call `StepDetail` is handed — but neither is ever
 * read by any component. So:
 *
 *  - ELITEA-2802's "Calling 'index_data' with parameters" header, its JSON
 *    parameter dump, and its "Thought for X secs" collapsible thinking
 *    section do not exist — `tool_inputs`/`thinking` are silently dropped.
 *  - ELITEA-2803's "thinking steps … tools invoked with toolkit/MCP name"
 *    (`"GitHub: get_issue"` format) does not exist either: the step LIST
 *    (`RunHistoryTrace.tsx`'s `stepLabel`) labels a step with the bare
 *    `tool_name` alone, and there is no separate "tools invoked" section.
 *  - ELITEA-2805's "formatted summary … not raw JSON, not a bare chunk
 *    count" for `index_data` runs cannot hold either, for the same reason:
 *    whatever the tool's real output happens to be (raw JSON or a chunk
 *    count both included) is what `tool_output` shows verbatim, because
 *    nothing here formats it.
 *
 * Since a REAL trace needs a real chat/tool-call turn (chat-stream lane,
 * not this one), the list/detail reads are intercepted with `page.route` —
 * the same technique `indexes.indexing-summaries.spec.ts` uses for the
 * sibling Index-History-tab screen — carrying rich `tool_inputs`/`thinking`
 * content, so the test proves the UI drops them even when the SERVER sends
 * them, not merely that this run happened to have none.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createConversation, deleteConversation } from '../../fixtures/api';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}`;
}

async function createArtifactToolkit(request: APIRequestContext, name: string): Promise<string> {
  const created = await request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: { name, type: 'artifact', settings: { selected_tools: ['index_data'] } },
  });
  expect(created.status(), await created.text()).toBe(201);
  const toolkitId = String(((await created.json()) as { id?: string | number }).id ?? '');
  expect(toolkitId).not.toBe('');
  return toolkitId;
}

async function attachToolkitParticipant(request: APIRequestContext, conversationId: string, toolkitId: string): Promise<void> {
  const response = await request.post(
    `${API_BASE}/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
    { data: [{ entity_name: 'toolkit', entity_meta: { id: toolkitId, project_id: DEFAULT_PROJECT_ID } }] },
  );
  expect(response.status(), await response.text()).toBe(200);
}

/** A rich trace payload — real `tool_inputs`/`thinking` content the UI is proven to drop. */
async function mockMessageTrace(page: Page, conversationId: string): Promise<void> {
  const STEP_ID = 990001;
  const MESSAGE_GROUP_ID = 5001;
  await page.route(new RegExp(`/elitea_core/message_traces/prompt_lib/\\d+/${conversationId}`), async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        rows: [
          {
            id: STEP_ID,
            message_group_id: MESSAGE_GROUP_ID,
            kind: 'tool_call',
            tool_name: 'index_data',
            parent_agent_name: null,
            parent_agent_call_id: null,
            started_at: new Date().toISOString(),
            is_error: false,
          },
        ],
        total: 1,
      }),
    });
  });
  await page.route(new RegExp(`/elitea_core/message_trace/prompt_lib/\\d+/${STEP_ID}`), async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        id: STEP_ID,
        message_group_id: MESSAGE_GROUP_ID,
        kind: 'tool_call',
        tool_name: 'index_data',
        parent_agent_name: null,
        parent_agent_call_id: null,
        started_at: new Date().toISOString(),
        is_error: false,
        tool_inputs: {
          max_tokens: 512,
          process_attachments: true,
          ignore_empty_body: true,
          max_attachment_depth: 2,
          max_attachment_size_mb: 25,
          index_name: 'p13-marker-index-name',
        },
        tool_output: '{"indexed": 5, "skipped": 2}',
        text: null,
        thinking: 'p13-marker-thinking-content: reasoning about which pages to index',
      }),
    });
  });
}

test.describe('generic Run History trace: request params + thinking steps (ELITEA-2802/2803/2805)', () => {
  test.afterEach(async ({ page }, testInfo) => {
    void testInfo;
    await page.unrouteAll({ behavior: 'ignoreErrors' }).catch(() => undefined);
  });

  /* onetest: ELITEA-2802, ELITEA-2803, ELITEA-2805 — `RunHistoryTrace`'s `StepDetail` now also renders
   * `tool_inputs` (JSON-formatted) and `thinking`, fetched by the same `useGetMessageTrace` call `text`/
   * `tool_output` already used. FIXED: the parameters and reasoning content. STILL A GAP, not attempted
   * here (a separate, cosmetic concern from the data being shown at all): the step list still labels a
   * step with the bare tool name, not the "[Toolkit Name]: [tool_action]" format 2803 also asks for, and
   * `tool_output` is still shown verbatim rather than specially formatted per tool (2805's "not raw
   * JSON" claim) — this test does not require either. */
  test('a tool_call trace step shows its request parameters and thinking content', async ({ page }) => {
    const toolkitId = await createArtifactToolkit(page.request, uniqueName('runhist-trace-toolkit'));
    const conversationId = await createConversation(page.request, uniqueName('runhist-trace-convo'));
    await attachToolkitParticipant(page.request, conversationId, toolkitId);
    await mockMessageTrace(page, conversationId);

    try {
      await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });
      const historyButton = page.getByTestId('pipeline-history-tab');
      await expect(historyButton).toBeVisible({ timeout: 20_000 });
      await historyButton.click();

      const panel = page.getByTestId('run-history-panel');
      await expect(panel).toBeVisible({ timeout: 20_000 });
      const row = panel.getByTestId('run-history-row');
      await expect(row).toHaveCount(1, { timeout: 20_000 });
      await row.click();

      const trace = page.getByTestId('run-history-trace');
      await expect(trace).toBeVisible({ timeout: 15_000 });
      const step = page.getByTestId('run-history-trace-step');
      await expect(step).toHaveCount(1);
      // The label the case wants ("Toolkit: action") is not what this renders
      // (the row's text also carries the step's timestamp, from `secondary`).
      await expect(step).toContainText('index_data');
      await step.click();

      const detail = page.getByTestId('run-history-trace-detail');
      await expect(detail).toBeVisible({ timeout: 10_000 });
      // What IS shown: tool_output, verbatim (raw JSON, undisguised — 2805's own complaint).
      await expect(detail).toContainText('"indexed": 5');

      // The assertion this case says SHOULD hold — request parameters and thinking rendered.
      await expect(detail).toContainText('p13-marker-index-name');
      await expect(detail).toContainText('p13-marker-thinking-content');
    } finally {
      await deleteConversation(page.request, conversationId);
      await page.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${toolkitId}`).catch(() => {});
    }
  });
});
