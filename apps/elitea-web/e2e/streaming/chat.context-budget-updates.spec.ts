/**
 * WHAT MOVES THE IN-CONTEXT BUDGET (#939 group 9).
 *
 * `journeys/chat/chat.contextBudget.spec.ts` owns the panel's CONFIGURATION —
 * it reports the budget the reader set, and the pencil changes it. It never
 * sends a message, because the journeys stack composes no worker, so nothing
 * there has ever moved the budget.
 *
 * This file moves it. Two of the seven cases in the group are claims about the
 * budget FOLLOWING the transcript:
 *
 *   * ELITEA-0530 — deleting a message updates the budget at once;
 *   * ELITEA-0531 — clearing a multi-message chat resets it.
 *
 * ELITEA-0525 (a toolkit call must not inflate the count) was attempted here
 * and is NOT claimed: proving it needs a tool that actually RAN, and the mock
 * tool host is unreachable from the worker on this stack — the same limitation
 * `chat.tail-trace-steps.spec.ts` records for its own timing assertions. A
 * version that compared context deltas without first proving a tool ran would
 * pass on a turn where none did, which is the failure this suite keeps paying
 * for. It stays on the ledger with that reason.
 *
 * ── WHERE THE NUMBERS ARE READ ───────────────────────────────────────────
 *
 * `GET /elitea_core/context_analytics/prompt_lib/{p}/{c}` — the same route the
 * panel renders (`contextsettings.BuildStatus`), carrying
 * `message_groups_in_context`, `message_groups_total` and `current_tokens`.
 * Read there rather than off the panel's text because the rendered line groups
 * digits with a non-breaking space and rounds; the case is about the COUNT,
 * and an assertion on the rendering would fail the day the formatting changed
 * and pass the day the count did.
 *
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  deleteAgent,
  expectStoredAssistantAnswer,
  fillComposer,
  readCallerPersonalProjectId,
  readStoredTranscript,
} from '../fixtures/api';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

function marker(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}${Date.now() % 1_000_000}${Math.floor(Math.random() * 46_656).toString(36)}`;
}

interface BudgetStatus {
  readonly groupsInContext: number;
  readonly groupsTotal: number;
  readonly tokens: number;
}

/** The budget as the panel's own route reports it. */
async function readBudget(page: Page, projectId: string, conversationId: string): Promise<BudgetStatus> {
  const response = await page.request.get(
    `${API_BASE}/elitea_core/context_analytics/prompt_lib/${projectId}/${conversationId}`,
  );
  expect(response.ok(), `the context-analytics route answered ${String(response.status())}`).toBe(true);
  const body = (await response.json()) as {
    message_groups_in_context?: unknown;
    message_groups_total?: unknown;
    current_tokens?: unknown;
  };
  return {
    groupsInContext: Number(body.message_groups_in_context ?? -1),
    groupsTotal: Number(body.message_groups_total ?? -1),
    tokens: Number(body.current_tokens ?? -1),
  };
}

async function createChatAgent(page: Page, projectId: string, name: string, toolkits?: readonly unknown[]): Promise<{ id: string }> {
  const created = await createAgentWithVersion(
    page.request,
    name,
    {
      instructions: 'You are a context-budget fixture. Answer briefly.',
      welcomeMessage: 'Ask me something.',
      conversationStarters: ['Hello.'],
      model: { modelName: MOCK_MODEL },
      meta: { step_limit: 25, internal_tools: [] },
      ...(toolkits === undefined ? {} : { tools: toolkits }),
    },
    projectId,
    `${AUTOTEST_PREFIX}context-budget fixture`,
  );
  return { id: created.id };
}

async function openAgentChat(page: Page, agentId: string): Promise<string> {
  await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversationId = String(((await (await conversationCreated).json()) as { id?: unknown }).id ?? '');
  expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 45_000 });
  return conversationId;
}

async function completeTurn(
  page: Page,
  projectId: string,
  conversationId: string,
  text: string,
  expectIn: string | undefined,
): Promise<void> {
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  const sendButton = await fillComposer(page, text);
  await sendButton.click();
  expect((await started).status(), 'the turn was refused').toBe(200);
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 180_000,
    ...(expectIn === undefined ? {} : { contains: expectIn }),
    message: 'the turn stored no answer',
  });
}

/*
 * onetest: ELITEA-0530 (deleting a message updates the budget at once) and
 * ELITEA-0531 (clearing a multi-message chat resets it).
 *
 * One journey: both are the same measurement — the budget follows the
 * transcript — taken after two different removals, and the second needs the
 * multi-message conversation the first one builds.
 */
test('the context budget follows the transcript when a message is deleted and when the chat is cleared', async ({
  page,
}) => {
  test.setTimeout(480_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'the chat persona must own a personal project (#290)').not.toBe('');

  let agentId = '';
  try {
    const agent = await createChatAgent(page, projectId, `${AUTOTEST_PREFIX}budg-${String(Date.now()).slice(-7)}`);
    agentId = agent.id;
    const conversationId = await openAgentChat(page, agent.id);

    const empty = await readBudget(page, projectId, conversationId);
    expect(empty.groupsTotal, 'a new conversation holds no message groups').toBe(0);

    const first = marker('b1');
    await completeTurn(page, projectId, conversationId, `Say this back: ${first}`, first);
    const oneTurn = await readBudget(page, projectId, conversationId);
    // MEASURED: a "message group" here is per MESSAGE, not per exchange — one
    // question plus one answer reports 2. The unit is recorded rather than
    // assumed, and every assertion below is a COMPARISON so it stays true if
    // the unit ever changes.
    expect(oneTurn.groupsTotal, 'one exchange must put something in the context').toBeGreaterThan(0);

    const second = marker('b2');
    await completeTurn(page, projectId, conversationId, `Say this back: ${second}`, second);

    const twoTurns = await readBudget(page, projectId, conversationId);
    expect(twoTurns.groupsTotal, 'a second exchange must add to the context').toBeGreaterThan(oneTurn.groupsTotal);
    // NOT asserted to be > 0: MEASURED, `current_tokens` stays 0 on this stack
    // for an ordinary conversation — the analytics that fills it is produced by
    // the context-management strategy, and none is active here. The cases in
    // this group are about the budget FOLLOWING the transcript, which the group
    // counts answer exactly; a token assertion would be measuring whether a
    // strategy happens to be configured.
    expect(twoTurns.tokens, 'the token count must at least be reported').toBeGreaterThanOrEqual(0);

    // ── ELITEA-0530: delete one exchange ──────────────────────────────────
    // Through the route the row menu's own confirm calls, so the assertion is
    // about the product's delete and not about a direct row removal.
    const transcript = await readStoredTranscript(page, projectId, conversationId);
    const lastAnswer = [...transcript].reverse().find((row) => row.role === 'assistant');
    expect(lastAnswer?.id, 'there must be an answer to delete').toBeTruthy();
    const deleted = await page.request.delete(
      `${API_BASE}/elitea_core/messages/prompt_lib/${projectId}/${conversationId}?message_id=${String(lastAnswer?.id)}`,
    );
    expect(deleted.ok(), `the delete was refused: ${(await deleted.text()).slice(0, 300)}`).toBe(true);

    await expect
      .poll(async () => (await readBudget(page, projectId, conversationId)).groupsTotal, {
        timeout: 60_000,
        message: 'the budget did not follow the delete',
      })
      .toBeLessThan(twoTurns.groupsTotal);

    // ── ELITEA-0531: clear the whole conversation ─────────────────────────
    const cleared = await page.request.delete(
      `${API_BASE}/elitea_core/messages/prompt_lib/${projectId}/${conversationId}`,
    );
    expect(cleared.ok(), `the clear was refused: ${(await cleared.text()).slice(0, 300)}`).toBe(true);

    await expect
      .poll(async () => (await readBudget(page, projectId, conversationId)).groupsTotal, {
        timeout: 60_000,
        message: 'clearing the chat did not reset the budget',
      })
      .toBe(0);
    const afterClear = await readBudget(page, projectId, conversationId);
    expect(
      afterClear.groupsInContext,
      'a cleared conversation has nothing in context either — the reset must reach both numbers',
    ).toBe(0);
  } finally {
    if (agentId !== '') await deleteAgent(page.request, agentId).catch(() => undefined);
  }
});
