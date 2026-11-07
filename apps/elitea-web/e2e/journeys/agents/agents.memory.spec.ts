/**
 * The agent's memory in the client (ADR-0050 decision 1b, issue 1608): the
 * "Mira remembered: …" chip under an answer, and the agent editor's Memory tab.
 *
 * THE CHIP runs against a FIXTURE transcript. A real one needs a model run
 * that called `remember`, and the E2E stack has no model that will. So the
 * conversation, its messages and its trace listing are served, and the
 * `remember` call arrives the way a reloaded conversation brings it: a light
 * `tool_call` row in the listing whose body (`tool_inputs`, `tool_output`) the
 * client fetches from the step route. The memory routes are served too, with
 * state, so approve, edit and forget each reach them in the contract's shape
 * and the chip shows what the server answered.
 *
 * THE TAB runs against the real server: a real agent, its real (empty)
 * memory, and the real opt-out route, which a reload must read back.
 */
import { expect, test, type Page, type Route } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createAgent, deleteAgent } from '../../fixtures/api';

/** No tenant holds this id; the reads below are served for it. */
const CONVERSATION_ID = '990161';
const APPLICATION_ID = 41;
const ENTRY_ID = 7101;
const STEP_ID = 88001;

const routeFor = (resource: string): RegExp =>
  new RegExp(`/api/v2/elitea_core/${resource}/prompt_lib/[^/?#]+/${CONVERSATION_ID}(?:[/?#]|$)`);
const MEMORY_ENTRY = new RegExp(`/api/v2/elitea_core/agent_memory/prompt_lib/[^/?#]+/${APPLICATION_ID}/${ENTRY_ID}(?:[?#]|$)`);
const MEMORY_LIST = new RegExp(`/api/v2/elitea_core/agent_memory/prompt_lib/[^/?#]+/${APPLICATION_ID}(?:[?#]|$)`);

const PARTICIPANTS = [
  { id: '1', entity_name: 'user', kind: 'human', entity_meta: { id: 1 }, meta: { user_name: 'Memory Reader' } },
  {
    id: '2',
    entity_name: 'application',
    kind: 'agent',
    entity_meta: { id: APPLICATION_ID, name: 'release-bot' },
    persona: { display_name: 'Mira', role: 'Release manager' },
  },
];

function messageRows(): Record<string, unknown>[] {
  const created = Date.now() - 60 * 60_000;
  return [
    { id: 1, uid: 'memory-message-1', conversation_id: Number(CONVERSATION_ID), role: 'user', content: 'Keep answers short, please.', created_at: new Date(created).toISOString(), author_participant_id: 1, author_kind: 'human', addressed_to: [], visibility: 'all' },
    { id: 2, uid: 'memory-message-2', conversation_id: Number(CONVERSATION_ID), role: 'assistant', content: 'Noted. I will keep them short.', created_at: new Date(created + 60_000).toISOString(), author_participant_id: 2, author_kind: 'agent', addressed_to: [1], visibility: 'all', reply_to_id: 1, question_id: 1 },
  ];
}

function entry(content: string, status: string): Record<string, unknown> {
  return {
    id: ENTRY_ID, application_id: APPLICATION_ID, kind: 'preference', content, subject_user_id: 1, status, pinned: false, version: 1,
    conversation_uuid: null, message_id: null, execution_id: null, written_by_kind: 'agent', written_by_user_id: null,
    created_at: '2026-11-01T10:00:00Z', updated_at: '2026-11-01T10:00:00Z',
  };
}

const json = (route: Route, body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });

interface MemoryCalls {
  readonly patches: unknown[];
  deletes: number;
}

async function serveTranscript(page: Page): Promise<MemoryCalls> {
  const calls: MemoryCalls = { patches: [], deletes: 0 };
  const onlyGet = (body: () => unknown) => async (route: Route) => (route.request().method() === 'GET' ? json(route, body()) : route.fallback());
  await page.route(routeFor('conversation'), onlyGet(() => ({ id: Number(CONVERSATION_ID), name: `${AUTOTEST_PREFIX}memory chip`, is_private: true, participants: PARTICIPANTS, meta: {} })));
  await page.route(routeFor('messages'), onlyGet(() => ({ items: messageRows(), total: 2, page: 0, page_size: 50, total_pages: 1 })));
  await page.route(routeFor('message_traces'), onlyGet(() => ({
    rows: [{ id: STEP_ID, message_group_id: 2, kind: 'tool_call', tool_name: 'remember', started_at: '2026-11-01T10:00:00Z', finished_at: '2026-11-01T10:00:01Z', is_error: false, attrs: {} }],
    total: 1,
  })));
  // The step's heavy columns, fetched once by the chip.
  await page.route(new RegExp(`/api/v2/elitea_core/message_trace/prompt_lib/[^/?#]+/${STEP_ID}(?:[?#]|$)`), (route) => json(route, {
    tool_inputs: { kind: 'preference', content: 'Prefers short answers' },
    tool_output: JSON.stringify({ status: 'pending', entry_id: String(ENTRY_ID), note: 'Saved; it waits for approval.' }),
  }));

  let current = entry('Prefers short answers', 'pending');
  await page.route(MEMORY_LIST, (route) => json(route, { rows: [current], total: 1, people: [{ id: 1, name: 'Memory Reader' }], can_manage: true, can_delete: true, can_approve: true, opted_out: false }));
  await page.route(MEMORY_ENTRY, async (route) => {
    const method = route.request().method();
    if (method === 'PATCH') {
      const change = route.request().postDataJSON() as Record<string, unknown>;
      calls.patches.push(change);
      current = { ...current, ...change, version: Number(current['version']) + 1 };
      return json(route, current);
    }
    if (method === 'DELETE') {
      calls.deletes += 1;
      return route.fulfill({ status: 204 });
    }
    return json(route, current);
  });
  return calls;
}

test.describe('agent memory', () => {
  test('approve, edit and forget from the remembered chip', async ({ page }) => {
    const calls = await serveTranscript(page);
    await page.goto(`${BASE_URL}/app/chat/${CONVERSATION_ID}`);

    const chip = page.getByTestId(`agent-memory-chip-${ENTRY_ID}`);
    await expect(chip, 'the reloaded remember call shows as a chip').toBeVisible({ timeout: 20_000 });
    await expect(chip).toHaveAttribute('data-state', 'pending');
    await expect(page.getByTestId(`agent-memory-chip-${ENTRY_ID}-text`)).toHaveText('Mira remembered: Prefers short answers (waiting for approval)');

    await page.getByTestId(`agent-memory-chip-${ENTRY_ID}-approve`).click();
    await expect(chip).toHaveAttribute('data-state', 'active');
    expect(calls.patches).toContainEqual({ status: 'active' });

    await page.getByTestId(`agent-memory-chip-${ENTRY_ID}-edit`).click();
    const content = page.getByTestId('agent-memory-edit-content');
    await content.fill('Prefers bullet points');
    await page.getByTestId('agent-memory-edit-dialog').getByRole('button', { name: 'Save' }).click();
    await expect(page.getByTestId(`agent-memory-chip-${ENTRY_ID}-text`)).toHaveText('Mira remembered: Prefers bullet points');
    expect(calls.patches).toContainEqual({ content: 'Prefers bullet points' });

    await page.getByTestId(`agent-memory-chip-${ENTRY_ID}-forget`).click();
    await expect(chip).toHaveAttribute('data-state', 'forgotten');
    await expect(chip).toHaveText('Mira forgot this.');
    expect(calls.deletes).toBe(1);
  });

  test('the Memory tab reads the real store and keeps the opt-out', async ({ page }) => {
    test.setTimeout(60_000);
    const agent = await createAgent(page.request, `${AUTOTEST_PREFIX}memory-${Date.now()}`);
    try {
      await page.goto(`${BASE_URL}/app/agents/all/${agent.id}`);
      await expect(page.getByTestId('edit-application-editor-tabs')).toBeVisible({ timeout: 20_000 });
      await page.getByTestId('edit-application-tab-memory').click();

      await expect(page.getByTestId('agent-memory-panel')).toBeVisible();
      await expect(page.getByTestId('agent-memory-empty'), 'a new agent remembers nothing').toBeVisible({ timeout: 20_000 });
      await expect(page.getByTestId('agent-memory-error')).toHaveCount(0);

      const remember = page.getByTestId('agent-memory-opt-out').getByRole('switch');
      await expect(remember).toBeChecked();
      const saved = page.waitForResponse((response) => /agent_memory_opt_out/.test(response.url()) && response.request().method() === 'PUT');
      await remember.click();
      expect((await saved).ok(), 'the opt-out route accepts the change').toBe(true);
      await expect(page.getByTestId('agent-memory-opted-out')).toBeVisible();

      await page.reload();
      await page.getByTestId('edit-application-tab-memory').click();
      await expect(page.getByTestId('agent-memory-opt-out').getByRole('switch'), 'the server kept the opt-out').not.toBeChecked({ timeout: 20_000 });
    } finally {
      await deleteAgent(page.request, agent.id);
    }
  });
});
