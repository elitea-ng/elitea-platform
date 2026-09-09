/**
 * Persistent, cross-conversation personal memory (#870): a memory saved in
 * Settings > Memory actually reaches a LATER, unrelated conversation's
 * system prompt — proof the runtime recall path
 * (`internal/application/agentexecution`'s `CurrentMemoryRecallResolver`) is
 * really wired into a live ad-hoc turn, not only reachable by a hand-built
 * request against the CRUD routes.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS ONE IS ON `chat-stream` AND NOT WITH THE OTHER CHAT JOURNEYS
 * ─────────────────────────────────────────────────────────────────────────────
 * Recall runs inside a real turn's admission (`StartCurrentAdhoc`), which
 * needs the full standalone stack's runtime plane and a real worker — the
 * plain `journeys/` stack mounts none of that (see `chat.message-delete.
 * spec.ts`'s own header for the full account, and `chat.messageFeedback.
 * spec.ts`'s for the same reasoning applied to a different #-issue feature).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE ASSERTION READS THE MOCK'S JOURNAL, NOT THE ANSWER
 * ─────────────────────────────────────────────────────────────────────────────
 * The mock's reply is an echo of the last USER message
 * (`deploy/mock-llm/server.py`), so an assertion on the stored answer would
 * read back the question, identical whether the memory reached the prompt or
 * not. `MockLlmJournalEntry.instructions` is the SYSTEM prompt the request
 * actually carried — the only server-side, model-independent evidence of
 * what the runtime assembled — same technique `chat.variables.spec.ts` uses
 * to pin agent-variable substitution.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT ELSE THIS PINS
 * ─────────────────────────────────────────────────────────────────────────────
 * After the turn settles, a page reload forces the PERSISTED read path
 * (`entities/message/lib/normalise.ts`'s `normaliseAssistantMessage`, the
 * one path this feature's `memoriesUsed` field is guaranteed to flow
 * through) and the journey asserts the "Using N memories" indicator
 * (`MessageHeaderRow`'s `chat-message-memories-used` chip) renders on the
 * answer — proof `RecordCurrentMemoryUsage`'s best-effort stamp onto
 * `chat_message_group.meta` reached the ordinary transcript read, not just
 * the admission response.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  clearMockLlmJournal,
  deleteConversation,
  fillComposer,
  readCallerPersonalProjectId,
  readMockLlmJournal,
  readStoredMessageGroups,
} from '../fixtures/api';

/** The model the standalone stack seeds; overridable for the real-model lane. */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/\d+\/[0-9a-f-]+/;

function uniqueToken(tag: string): string {
  return `AUTOTEST${tag}${Date.now().toString(36).toUpperCase()}`;
}

test('a memory saved in Settings > Memory reaches a later chat turn’s system prompt', async ({ page }) => {
  // Memory create/delete, one whole agent turn (conversation create,
  // admission, dispatch, a model call, the stream back), and a reload.
  test.setTimeout(300_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project').not.toBe('');

  const token = uniqueToken('MEM');
  const memoryContent = `The user's favourite constant is ${token}. Always mention ${token} when asked about favourite constants.`;

  // ── seed the memory BEFORE any turn — recall reads it at admission time ──
  const memoriesUrl = `${API_BASE}/elitea_core/memories/prompt_lib/${projectId}`;
  const createMemoryResponse = await page.request.post(memoriesUrl, {
    data: { content: memoryContent, tags: [], enabled: true },
  });
  expect(
    createMemoryResponse.status(),
    `the memory must be created: ${(await createMemoryResponse.text()).slice(0, 300)}`,
  ).toBe(201);
  const memoryId = ((await createMemoryResponse.json()) as { id?: string }).id ?? '';
  expect(memoryId, 'the created memory must carry a real id').not.toBe('');

  let conversationId = '';
  try {
    // NOT `createConversation` + navigate-to-id: a conversation made that way
    // carries no participants at all, and the ad-hoc `dummy` (model) one is
    // provisioned ONLY by `useChatBoxSend`'s `createConversationForSend`, at
    // the moment the FIRST send creates the conversation — selecting a model
    // on an already-existing, participant-less conversation never attaches
    // one (`LLMModelSelector` is presentational; nothing else calls
    // `addParticipants` for it). Landing on such a conversation and sending
    // therefore 422s at `ResolveCurrentAdhocTurn`, which joins on
    // `entity_name='dummy'` — a state no real user reaches, since the product
    // never creates a conversation except through that same first-send path.
    // Same pattern `chat.canvasDocument.spec.ts`'s `seedAnswer` uses.
    await page.goto(`${BASE_URL}/app/chat`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

    await page.getByTestId('model-selector-button').click();
    const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
    await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
    await modelOption.click();
    await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

    // Bound the journal window to THIS turn — the mock serves every spec in
    // this project, and a prior test's entries must never be mistaken for
    // this one's.
    await clearMockLlmJournal(page);

    const created = page.waitForResponse(
      (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
      { timeout: 45_000 },
    );
    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 45_000,
    });

    // The prompt itself carries NO trace of the memory's content — proof
    // that anything containing `token` in the system prompt got there
    // through recall, not because this journey typed it into the question.
    const sendButton = await fillComposer(page, `${AUTOTEST_PREFIX}${token} What is my favourite constant?`);
    await sendButton.click();

    const createdResponse = await created;
    expect(createdResponse.status(), 'the send must create a real conversation').toBe(201);
    conversationId = ((await createdResponse.json()) as { id?: string }).id ?? '';
    expect(conversationId).toMatch(/^\d+$/);

    const startResponse = await started;
    expect(startResponse.status(), `the turn was refused: ${(await startResponse.text()).slice(0, 300)}`).toBe(200);

    await expect
      .poll(
        async () => (await readStoredMessageGroups(page, projectId, conversationId)).length,
        { timeout: 120_000, message: 'the turn never stored a question + answer pair' },
      )
      .toBe(2);

    // ── the system-prompt proof ─────────────────────────────────────────
    const journal = await readMockLlmJournal(page);
    const chatRequests = journal.filter((entry) => entry.path === '/v1/chat/completions');
    expect(chatRequests.length, 'the turn must have reached the mock model').toBeGreaterThan(0);
    const lastRequest = chatRequests[chatRequests.length - 1];
    expect(
      lastRequest?.instructions ?? '',
      'the recalled memory text must reach the system prompt the runtime actually sent',
    ).toContain(token);

    // ── the UI indicator, through the PERSISTED read path ───────────────
    await page.reload();
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
    const memoriesUsedChip = page.getByTestId('chat-message-memories-used').last();
    await expect(
      memoriesUsedChip,
      'a turn that recalled a memory must show the "Using N memories" indicator after reload',
    ).toBeVisible({ timeout: 30_000 });

    // ── and the same fact, straight from the API (RecordCurrentMemoryUsage) ──
    const groupsRaw = await page.request.get(
      `${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}?messages_limit=50&sort_order=asc`,
    );
    expect(groupsRaw.ok()).toBe(true);
    const groupsBody = (await groupsRaw.json()) as {
      message_groups?: readonly { author_participant_id?: unknown; meta?: { memories_used?: unknown } }[];
    };
    const answerGroup = (groupsBody.message_groups ?? []).find((g) => (g.meta?.memories_used ?? 0) as number > 0);
    expect(answerGroup, 'some message group in this turn must carry meta.memories_used > 0').toBeTruthy();
  } finally {
    if (conversationId && projectId) {
      await deleteConversation(page.request, conversationId, projectId).catch(() => {});
    }
    if (memoryId) {
      await page.request.delete(`${API_BASE}/elitea_core/memory/prompt_lib/${projectId}/${memoryId}`).catch(() => {});
    }
  }
});
