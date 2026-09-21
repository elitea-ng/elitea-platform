/**
 * WHAT THE MODEL WAS GIVEN (#939 groups 8 and 10).
 *
 * Two cases that look unrelated and are the same measurement from opposite
 * sides:
 *
 *   * ELITEA-0383/0067 — a follow-up turn must carry the earlier exchange, or
 *     the agent cannot answer "what did you just tell me";
 *   * ELITEA-0384/0069 — a sub-participant call must NOT carry the parent's
 *     `chat_history`: the child is given the task and nothing else.
 *
 * One asks for those rows to be present, the other for them to be absent, and
 * NEITHER is observable from anything this lane could read before: the mock's
 * reply is an echo of the LAST user message, so an answer that "recalls" and
 * an answer that does not are the same bytes; and an absence cannot be read
 * off a reply at all.
 *
 * ── THE CAPABILITY THIS FILE ADDED ───────────────────────────────────────
 *
 * `deploy/mock-llm/server.py` now journals `history` beside `instructions`:
 * one `{role, text}` per non-system message of the request, each text
 * truncated (`_history_digest`). The journal already recorded what the model
 * was TOLD TO BE (the system prompt) and what it was OFFERED (the tool
 * names); this is what it was GIVEN. Both assertions below read it, and both
 * are exact rather than circumstantial.
 *
 * RUST leg, like the rest of this directory.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  agentAsToolName,
  API_BASE,
  AUTOTEST_PREFIX,
  callToolWithArgumentsPrompt,
  clearMockLlmJournal,
  createAgentWithVersion,
  deleteAgent,
  expectStoredAssistantAnswer,
  fillComposer,
  readCallerPersonalProjectId,
  readMockLlmJournal,
  readStoredTranscript,
  type MockLlmJournalEntry,
} from '../fixtures/api';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

function marker(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}${Date.now() % 1_000_000}${Math.floor(Math.random() * 46_656).toString(36)}`;
}

/** Every text the model was given in this request, whatever role carried it. */
function historyText(entry: MockLlmJournalEntry | undefined): string {
  return (entry?.history ?? []).map((row) => `${row.role}: ${row.text}`).join('\n');
}

interface CreatedAgent {
  readonly id: string;
  readonly versionId: string;
  readonly name: string;
}

async function createChatAgent(
  page: Page,
  projectId: string,
  name: string,
  instructions: string,
): Promise<CreatedAgent> {
  const created = await createAgentWithVersion(
    page.request,
    name,
    {
      instructions,
      welcomeMessage: 'Ask me something.',
      conversationStarters: ['Tell me something.'],
      model: { modelName: MOCK_MODEL },
      meta: { step_limit: 25, internal_tools: [] },
    },
    projectId,
    `${AUTOTEST_PREFIX}history fixture`,
  );
  return { id: created.id, versionId: created.versionId, name };
}

/** Open a chat with the agent and mint its conversation, the way the agent page's Chat button does. */
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

/** Send one message and require the START to be admitted. */
async function sendTurn(page: Page, prompt: string): Promise<void> {
  const sendButton = await fillComposer(page, prompt);
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  await sendButton.click();
  expect((await started).status(), `the turn was refused: ${(await (await started).text()).slice(0, 300)}`).toBe(200);
}

/*
 * onetest: ELITEA-0383 (chat page) and ELITEA-0067 (agent page) — the same
 * regression from two entry points: a follow-up turn must be given the
 * exchange that preceded it. Driven once, because the two entry points mint
 * the same conversation through the same route and the claim is about what the
 * RUNTIME assembles, not about which button opened the chat.
 */
test('a follow-up turn carries the earlier exchange to the model', async ({ page }) => {
  // Two model calls plus the agent fixture.
  test.setTimeout(420_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'the chat persona must own a personal project (#290)').not.toBe('');

  const first = marker('fact');
  let agentId = '';
  try {
    const agent = await createChatAgent(
      page,
      projectId,
      `${AUTOTEST_PREFIX}hist-${String(Date.now()).slice(-7)}`,
      'You are a memory fixture. Answer briefly and remember what you said.',
    );
    agentId = agent.id;
    const conversationId = await openAgentChat(page, agent.id);

    await sendTurn(page, `Remember this exactly: ${first}`);
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 180_000,
      contains: first,
      message: 'the first turn stored no answer, so there is no history for the second to carry',
    });

    // Cleared HERE, between the turns: the assertion below is about the SECOND
    // request, and the first request obviously carried the first question.
    await clearMockLlmJournal(page);

    const second = marker('recall');
    await sendTurn(page, `What did I just tell you? ${second}`);
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 180_000,
      contains: second,
      message: 'the follow-up turn stored no answer',
    });

    // THE ASSERTION. The model's own request for the second turn must contain
    // the first exchange — both halves: the question that was asked and the
    // answer that was given. An agent that was handed only the new question
    // cannot "recall", and the echoed reply would look identical either way.
    const requests = await readMockLlmJournal(page);
    expect(requests.length, 'the second turn made no model request the journal saw').toBeGreaterThan(0);
    const rows = requests.flatMap((entry) => entry.history);
    // BY ROLE, not by text, and that is the whole point: the mock echoes the
    // question, so the first answer's text IS the first question's text. Only
    // the role tells "the earlier question was carried" apart from "the
    // earlier answer was carried", and the case needs both — an agent given
    // the questions but not its own replies cannot restate what it said.
    expect(
      rows.some((row) => row.role === 'user' && row.text.includes(first)),
      'the follow-up must carry the earlier QUESTION',
    ).toBe(true);
    expect(
      rows.some((row) => row.role === 'assistant' && row.text.includes(first)),
      'the follow-up must carry the earlier ANSWER too, not just the question',
    ).toBe(true);
    expect(
      rows.some((row) => row.role === 'user' && row.text.includes(second)),
      'the follow-up must of course also carry the new question',
    ).toBe(true);

    // And the transcript grew rather than restarted — the store-side half of
    // the same regression.
    const transcript = await readStoredTranscript(page, projectId, conversationId);
    expect(
      transcript.map((row) => row.role),
      'two exchanges must accumulate in one conversation',
    ).toEqual(['user', 'assistant', 'user', 'assistant']);
  } finally {
    if (agentId !== '') await deleteAgent(page.request, agentId).catch(() => undefined);
  }
});

/*
 * onetest: ELITEA-0384 and ELITEA-0069 — a parent that delegates must send the
 * child the TASK and not the conversation: no `chat_history` in the
 * sub-participant's input. Asserted as the absence it is, against the child's
 * own model request.
 */
test('a delegated sub-agent is given the task and not the parent’s chat history', async ({ page }) => {
  test.setTimeout(420_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'the chat persona must own a personal project (#290)').not.toBe('');

  const stamp = String(Date.now()).slice(-7);
  const secret = marker('parentonly');
  let parentId = '';
  let childId = '';

  try {
    const child = await createChatAgent(
      page,
      projectId,
      `${AUTOTEST_PREFIX}child-${stamp}`,
      'You are a sub-agent. Carry out the task you are given and report the result.',
    );
    childId = child.id;
    const parent = await createChatAgent(
      page,
      projectId,
      `${AUTOTEST_PREFIX}parent-${stamp}`,
      'You coordinate work. Hand tasks to the saved agent you have been attached to.',
    );
    parentId = parent.id;

    // Attach: the URL names the CHILD and the body names the PARENT — the
    // direction the agent page's own picker sends.
    const attached = await page.request.patch(
      `${API_BASE}/elitea_core/application_relation/prompt_lib/${projectId}/${child.id}/${child.versionId}`,
      { data: { application_id: Number(parent.id), version_id: Number(parent.versionId), has_relation: true } },
    );
    expect(attached.status(), `the child must attach: ${(await attached.text()).slice(0, 300)}`).toBeLessThan(300);

    const conversationId = await openAgentChat(page, parent.id);

    // ── A first exchange, so the parent HAS a history to leak ─────────────
    // Without it the assertion below is vacuous: a child request carrying no
    // history would pass whether or not the runtime filters anything.
    await sendTurn(page, `Remember this exactly: ${secret}`);
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 180_000,
      contains: secret,
      message: 'the parent’s first turn stored no answer, so it has no history to withhold',
    });

    await clearMockLlmJournal(page);

    // ── Now delegate ──────────────────────────────────────────────────────
    const task = marker('task');
    const childTool = agentAsToolName({ agentId: child.id, versionId: child.versionId, name: child.name });
    await sendTurn(
      page,
      callToolWithArgumentsPrompt(childTool, { task: `Handle this: ${task}` }, 'delegate it and quote the answer'),
    );
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 240_000,
      message: 'the delegating turn produced no answer',
    });

    const requests = await readMockLlmJournal(page);
    expect(requests.length, 'the delegating turn made no model request the journal saw').toBeGreaterThan(1);

    // The CHILD's request is the one carrying the child's own instructions.
    const childRequests = requests.filter((entry) => entry.instructions.includes('You are a sub-agent'));
    expect(
      childRequests.length,
      'the sub-agent was never asked anything — the delegation did not reach a child model call',
    ).toBeGreaterThan(0);

    // THE ASSERTION: the child was given the task, and was NOT given the
    // parent's conversation.
    const childGiven = childRequests.map((entry) => historyText(entry)).join('\n---\n');
    expect(childGiven, 'the child must be given the task it was delegated').toContain(task);
    expect(
      childGiven,
      'the child must NOT be given the parent’s chat history — this marker was only ever said to the parent',
    ).not.toContain(secret);
  } finally {
    if (parentId !== '') await deleteAgent(page.request, parentId).catch(() => undefined);
    if (childId !== '') await deleteAgent(page.request, childId).catch(() => undefined);
  }
});
