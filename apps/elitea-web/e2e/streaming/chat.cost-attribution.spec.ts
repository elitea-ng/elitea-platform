/**
 * Cost attribution by agent and by tool (issue #875) — an API-lane proof over
 * a REAL toolkit turn, not a mocked response.
 *
 * ── WHY THIS IS ITS OWN SPEC, NOT AN ASSERTION BOLTED ONTO chat.toolkit.spec.ts ──
 *
 * That file's subject is materialization and dispatch: does the toolkit reach
 * the model, and does the runtime execute the call it asks for. This file's
 * subject is a DIFFERENT hop, downstream of a working dispatch: does the
 * gateway's request log carry the execution id the SDK client now sends
 * (`EliteaClientContext.execution_id`, `internal/llmproxy/identity.go`
 * `HeaderExecutionID`), does `elitea_runtime.tool_call_records` carry it too
 * (`agent_trace.go`'s `recordAgentToolCalls`, since #875), and does
 * `/analytics_costs`' `estimate` block correlate the two into `by_agent` and
 * `by_tool`. A turn that dispatches correctly can still leave every one of
 * those columns NULL — that was the actual production state before #875: the
 * agent dimension had a reader (migration 0100, shipped with #615) and no
 * producer, because no worker ever sent the header. So this spec drives one
 * real turn and reads the MONEY endpoint back, which is the only way to catch
 * that class of gap — a unit or Go integration test plants the row directly
 * and never exercises the worker's own header-sending code path at all.
 *
 * ── WHAT IS NOT RE-PROVEN HERE ──
 *
 * The toolkit form → picker → freeze → dispatch chain is chat.toolkit.spec.ts's
 * job and its comment header explains why each step is real rather than
 * mocked. This spec reuses that exact path (same fixtures, same mock
 * operation) purely to PRODUCE one attributable call; it does not re-assert
 * materialization or the mock's own journals.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  EMPTY_TOOLKIT_GUARDRAILS,
  MOCK_TOOL_READ_OPERATION,
  attachToolkitThroughPicker,
  callToolPrompt,
  clearMockLlmJournal,
  clearMockToolJournal,
  createAgentThroughForm,
  createOpenApiToolkitThroughForm,
  expectStoredAssistantAnswer,
  fetchMockToolSpec,
  setToolkitGuardrails,
} from '../fixtures/api';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** Mirrors chat.toolkit.spec.ts's own pin — see that file for why. */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

interface EstimateAgentRow {
  readonly application_id: string;
  readonly calls: number;
}

interface EstimateToolRow {
  readonly tool_name: string;
  readonly attributed_runs: number;
}

interface CostsEstimate {
  readonly agent_dimension_available: boolean;
  readonly tool_dimension_available: boolean;
  readonly by_agent?: readonly EstimateAgentRow[];
  readonly by_tool?: readonly EstimateToolRow[];
}

/**
 * `GET /analytics_costs`'s `estimate` block, over a window wide enough to
 * cover clock skew between this process and the stack (`[-1h, +1h]`) — the
 * same margin `agentAnalyticsWindow()` uses on the Go integration tests this
 * spec is the end-to-end counterpart of.
 */
async function readCostEstimate(
  page: import('@playwright/test').Page,
  projectId: string,
): Promise<CostsEstimate | undefined> {
  const now = Date.now();
  const dateFrom = new Date(now - 60 * 60 * 1000).toISOString();
  const dateTo = new Date(now + 60 * 60 * 1000).toISOString();
  const response = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/analytics_costs/prompt_lib/${projectId}`,
    { params: { date_from: dateFrom, date_to: dateTo } },
  );
  expect(response.ok(), `analytics_costs refused: ${(await response.text()).slice(0, 300)}`).toBe(true);
  const body = (await response.json()) as { estimate?: CostsEstimate };
  return body.estimate;
}

/**
 * Polls for the two rows the gateway's batched request-log writer and the
 * trace projector settle asynchronously — the FlushInterval (1s) and the
 * turn's own trace commit are both already past by the time the stored
 * answer resolves, but neither is a hard guarantee, so this bounds the wait
 * rather than asserting on the first read.
 */
async function waitForAttribution(
  page: import('@playwright/test').Page,
  projectId: string,
  agentId: string,
  toolName: string,
): Promise<CostsEstimate> {
  const deadline = Date.now() + 30_000;
  let last: CostsEstimate | undefined;
  for (;;) {
    last = await readCostEstimate(page, projectId);
    const agentRow = last?.by_agent?.find((row) => row.application_id === agentId);
    const toolRow = last?.by_tool?.find((row) => row.tool_name === toolName);
    if (agentRow !== undefined && toolRow !== undefined) return last;
    if (Date.now() > deadline) {
      throw new Error(
        `attribution did not settle within 30s: agent_dimension_available=${String(
          last?.agent_dimension_available,
        )} tool_dimension_available=${String(last?.tool_dimension_available)} ` +
          `by_agent=${JSON.stringify(last?.by_agent)} by_tool=${JSON.stringify(last?.by_tool)}`,
      );
    }
    await page.waitForTimeout(1_000);
  }
}

test('a real toolkit turn is attributed to its agent and its tool on the cost estimate', async ({ page }) => {
  test.setTimeout(300_000);

  const suffix = String(Date.now() % 1_000_000);
  const toolkitName = `${AUTOTEST_PREFIX}costtk-${suffix}`;
  const agentName = `${AUTOTEST_PREFIX}costagent-${suffix}`;

  await setToolkitGuardrails(EMPTY_TOOLKIT_GUARDRAILS);
  await clearMockToolJournal(page);
  await clearMockLlmJournal(page);
  const spec = await fetchMockToolSpec(page);

  // ── Author + attach, exactly as chat.toolkit.spec.ts does ───────────────
  const { projectId, toolkitId } = await createOpenApiToolkitThroughForm(page, toolkitName, spec.text);
  const agent = await createAgentThroughForm(page, agentName);
  expect(agent.projectId, 'the agent and the toolkit must land in the same project').toBe(projectId);

  const storedAgent = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agent.agentId}`,
  );
  const storedMeta =
    ((await storedAgent.json()) as { version_details?: { meta?: Record<string, unknown> } }).version_details
      ?.meta ?? {};
  const pinned = await page.request.put(
    `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${projectId}/${agent.agentId}/${agent.versionId}`,
    { data: { meta: storedMeta, llm_settings: { model_name: MOCK_MODEL } } },
  );
  expect(pinned.status(), `the mock model must be pinnable: ${(await pinned.text()).slice(0, 300)}`).toBeLessThan(
    300,
  );

  await expect(page.getByTestId('agent-toolkits-section')).toBeVisible({ timeout: 30_000 });
  await attachToolkitThroughPicker(page, toolkitName);

  const withTools = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agent.agentId}`,
  );
  const frozenTools =
    ((await withTools.json()) as { version_details?: { tools?: readonly { tool_id?: number }[] } })
      .version_details?.tools ?? [];
  expect(
    frozenTools.find((tool) => String(tool.tool_id ?? '') === toolkitId),
    'the toolkit must be mapped onto the version before the turn, or nothing is dispatched',
  ).toBeDefined();

  // ── One turn, calling the read operation ─────────────────────────────────
  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversation = (await (await conversationCreated).json()) as { id?: string | number };
  const conversationId = String(conversation.id ?? '');
  expect(conversationId).not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 45_000 });

  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 30_000 });
  await input.fill(callToolPrompt(MOCK_TOOL_READ_OPERATION, `attribute this call ${suffix}`));
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();
  expect((await started).status(), 'the toolkit turn was refused').toBe(200);

  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 180_000,
    message: 'no stored answer — the turn must settle before attribution can be read back',
    contains: MOCK_TOOL_READ_OPERATION,
  });

  // ── The proof: the SAME execution shows up as spend on BOTH dimensions ──
  //
  // agent.agentId is the application id GetAgentAnalytics and estimateByAgent
  // both resolve `execution_id` to, via `p_<project>.chat_participants`
  // (`internal/infra/db/repos/analytics.go`'s `agentUsage`). MOCK_TOOL_READ_OPERATION
  // is the tool name `agent_trace.go`'s trace projector recorded, which
  // `elitea_runtime.tool_call_records` groups `by_tool` on.
  const estimate = await waitForAttribution(page, projectId, agent.agentId, MOCK_TOOL_READ_OPERATION);

  expect(estimate.agent_dimension_available, 'agent_dimension_available must be true once a real execution ran').toBe(
    true,
  );
  expect(estimate.tool_dimension_available, 'tool_dimension_available must be true once a real tool ran').toBe(true);

  const agentRow = estimate.by_agent?.find((row) => row.application_id === agent.agentId);
  expect(agentRow, `this agent must appear in by_agent: ${JSON.stringify(estimate.by_agent)}`).toBeDefined();
  expect(agentRow?.calls ?? 0, 'the agent row must count at least the one priceable call this turn made').toBeGreaterThan(
    0,
  );

  const toolRow = estimate.by_tool?.find((row) => row.tool_name === MOCK_TOOL_READ_OPERATION);
  expect(toolRow, `the called tool must appear in by_tool: ${JSON.stringify(estimate.by_tool)}`).toBeDefined();
  expect(
    toolRow?.attributed_runs ?? 0,
    'the tool row must count at least the one execution that called it',
  ).toBeGreaterThan(0);
});
