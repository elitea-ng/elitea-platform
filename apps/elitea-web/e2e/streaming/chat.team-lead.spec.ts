/**
 * A TEAM CONVERSATION WITH A LEAD, FIRST MESSAGE, NOTHING PICKED (issue 3049).
 *
 * `journeys/chat/chat.multiAgent.spec.ts` proves the lead is written and that
 * "To:" names it, against a served start POST: that lane has no runtime plane,
 * so no agent can answer there. This is the same conversation shape on the full
 * standalone stack, with a real turn against the offline mock model.
 *
 * The defect: a fresh conversation whose floor is `lead` (a team started from
 * the Teams tab or a team import writes exactly that, `meta.floor {policy:
 * lead, lead_participant_id}`) showed "To: <lead> (lead)", but the composer had
 * no target of its own (no pick, no history), so it sent the message as a model
 * chat with no participant. Main refused it with `agent_target_required` and
 * the person saw "This conversation has several agents. Pick the agent this
 * message is for." under a chip that had already named the agent.
 *
 * What is asserted: the chip names the lead; the start the composer sends is
 * the lead's agent turn (`agent.execute.application.v1`, `participant_id` =
 * the lead, no `addressed_to`); and the stored answer is the lead's, carrying
 * the per-run marker the mock echoes back.
 *
 * Lives in `streaming/` because it runs a real turn (`chat-stream` project,
 * `scripts/chat-stream-e2e.sh`).
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  fillComposer,
  readCallerIdentity,
  readCallerPersonalProjectId,
} from '../fixtures/api';

const API = `${BASE_URL}/api/v2`;
const APPLICATION_CONTRACT = 'agent.execute.application.v1';
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

interface Agent {
  readonly id: string;
  readonly versionId: string;
  readonly name: string;
}

async function createAgent(page: Page, projectId: string, name: string): Promise<Agent> {
  const created = await createAgentWithVersion(
    page.request,
    name,
    {
      instructions: 'You are a team member fixture. Answer the message you are given.',
      model: { modelName: MOCK_MODEL },
      meta: { step_limit: 5, internal_tools: [] },
    },
    projectId,
  );
  return { id: created.id, versionId: created.versionId, name };
}

interface StoredMessage {
  readonly role?: string;
  readonly content?: string;
  readonly author_participant_id?: number;
  readonly is_streaming?: boolean;
  readonly metadata?: { readonly is_error?: boolean };
}

async function storedMessages(page: Page, projectId: string, conversationId: string): Promise<readonly StoredMessage[]> {
  const stored = await page.request.get(`${API}/elitea_core/messages/prompt_lib/${projectId}/${conversationId}`);
  if (!stored.ok()) return [];
  return ((await stored.json()) as { items?: readonly StoredMessage[] }).items ?? [];
}

test('a message under the default lead chip is answered by the lead', async ({ page }) => {
  // Two agents, a conversation, one admission, a model call and the stream back.
  test.setTimeout(240_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project, and the setup waits for one').not.toBe('');
  const caller = await readCallerIdentity(page.request);
  const stamp = Date.now() % 1_000_000;
  const lead = await createAgent(page, projectId, `${AUTOTEST_PREFIX}lead-${stamp}`);
  const member = await createAgent(page, projectId, `${AUTOTEST_PREFIX}member-${stamp}`);
  const created = await page.request.post(`${API}/elitea_core/conversations/prompt_lib/${projectId}`, {
    data: { name: `${AUTOTEST_PREFIX}team-${stamp}`, is_private: true },
  });
  expect(created.status(), `the conversation must be created: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const conversationId = String(((await created.json()) as { id?: unknown }).id ?? '');

  try {
    const added = await page.request.post(`${API}/elitea_core/participants/prompt_lib/${projectId}/${conversationId}`, {
      data: [
        { entity_name: 'user', entity_meta: { id: Number(caller.id) } },
        ...[lead, member].map((agent) => ({
          entity_name: 'application',
          entity_meta: { id: agent.id, project_id: Number(projectId), name: agent.name },
          entity_settings: { version_id: agent.versionId },
        })),
      ],
    });
    expect(added.status(), `the participants must be added: ${(await added.text()).slice(0, 300)}`).toBe(200);
    const rows = (await added.json()) as readonly { id?: unknown; entity_name?: unknown; entity_meta?: { id?: unknown } }[];
    const leadParticipantId = Number(
      rows.find((row) => row.entity_name === 'application' && String(row.entity_meta?.id ?? '') === lead.id)?.id,
    );
    expect(leadParticipantId, 'the lead must be a participant').toBeGreaterThan(0);

    // The floor a team start writes (agentteams Start -> conversations.Create).
    const floor = await page.request.put(`${API}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`, {
      data: { meta: { floor: { policy: 'lead', lead_participant_id: leadParticipantId } } },
    });
    expect(floor.ok(), `the lead must be stored: ${(await floor.text()).slice(0, 300)}`).toBe(true);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-floor-target')).toHaveText(`To: ${lead.name} (lead)`, { timeout: 30_000 });

    const prompt = `${AUTOTEST_PREFIX}hello team ${stamp}`;
    const sendButton = await fillComposer(page, prompt);
    const start = page.waitForRequest(
      (request) => request.method() === 'POST' && request.url().includes('execution_contract='),
      { timeout: 30_000 },
    );
    await sendButton.click();
    const sent = await start;
    expect(new URL(sent.url()).searchParams.get('execution_contract'), 'an agent turn, not a model chat').toBe(APPLICATION_CONTRACT);
    const body = sent.postDataJSON() as { participant_id?: unknown; addressed_to?: unknown };
    expect(body.participant_id, 'the start names the lead').toBe(leadParticipantId);
    expect(body.addressed_to, 'an unaddressed message carries no address').toBeUndefined();
    const admitted = await sent.response();
    expect(admitted?.status(), 'the lead turn is admitted, not agent_target_required').toBe(200);

    // The answer is the lead's, with the marker the mock echoes back.
    await expect
      .poll(
        async () => {
          const answer = (await storedMessages(page, projectId, conversationId)).find((item) => item.role === 'assistant');
          if (answer === undefined || answer.is_streaming === true || answer.metadata?.is_error === undefined) return 'pending';
          if (answer.metadata.is_error) return `error: ${answer.content ?? ''}`;
          return `${String(answer.author_participant_id)}:${(answer.content ?? '').includes(prompt) ? 'echo' : 'other'}`;
        },
        { timeout: 120_000, message: 'the lead must answer the message' },
      )
      .toBe(`${String(leadParticipantId)}:echo`);
  } finally {
    await page.request.delete(`${API}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`);
    for (const agent of [lead, member]) {
      await page.request.delete(`${API}/elitea_core/application/prompt_lib/${projectId}/${agent.id}`);
    }
  }
});
