/**
 * Thumbs up / thumbs down + optional comment on a real chat message (#880).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS ONE IS ON `chat-stream` AND NOT WITH THE OTHER CHAT JOURNEYS
 * ─────────────────────────────────────────────────────────────────────────────
 * Feedback is keyed by `chat_message_group.uuid` — a real row a real turn
 * writes. `docker-compose.e2e-standalone.yml`, where every other chat
 * journey runs, mounts no runtime plane and persists no message group at
 * all (see `chat.message-delete.spec.ts`'s own header for the full
 * account), so `POST /elitea_core/message_feedback/prompt_lib/{p}/{uuid}`
 * there could only ever 404. Here a real turn writes the row the vote is
 * about, matching the DoD's own wording for this issue: "rates a SEEDED
 * message and reads it back via API".
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT IS ASSERTED THROUGH THE UI, AND WHAT THROUGH THE API
 * ─────────────────────────────────────────────────────────────────────────────
 * The FIRST vote goes through the real control in the real transcript
 * (`ApplicationAnswer`'s `MessageFeedbackControl`) — proof the feature is
 * actually WIRED into the composed chat surface, not only reachable by a
 * hand-built request. Everything after that (switching the vote, adding a
 * comment, retracting it, and the aggregate a second "user" would see) goes
 * straight through `page.request`, because those are server facts a click
 * would only be indirecting through, and the API is where "reads it back"
 * points.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { API_BASE, deleteConversation, readStoredMessageGroups } from '../fixtures/api';

/** The model the standalone stack seeds; overridable for the real-model lane. */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/** `ChatBox` names the conversation after the FIRST question, truncated to 50 chars. */
const MAX_NAME = 50;

const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

function uniqueToken(tag: string): string {
  return `AUTOTEST${tag}${Date.now().toString(36).toUpperCase()}`;
}

interface FeedbackSummary {
  readonly likes: number;
  readonly dislikes: number;
  readonly mine?: { readonly rating: 1 | -1; readonly comment?: string };
}

test('rating a real assistant message persists, upserts, and reads back through the API', async ({ page }) => {
  // One whole agent turn (conversation create, admission, dispatch, a model
  // call, the stream back) plus five feedback round-trips. Every wait below
  // is bounded well under this, so a real hang fails on its own step.
  test.setTimeout(300_000);

  const token = uniqueToken('FB');
  const prompt = `autotest echo exactly: ${token}`;
  expect(prompt.length, 'the prompt must fit the 50-char conversation-name truncation').toBeLessThanOrEqual(MAX_NAME);

  let projectId = '';
  let conversationId = '';

  try {
    await page.goto(`${BASE_URL}/app/chat`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

    // The seeded model is not decoration: an ad-hoc turn resolves against a
    // `dummy` participant carrying the model, and the start route reads
    // `llm_settings.model_name`. With nothing selected the send is refused
    // 400 before it reaches the worker.
    await page.getByTestId('model-selector-button').click();
    const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
    await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
    await modelOption.click();
    await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

    // ── the turn whose answer this journey rates ────────────────────────
    const created = page.waitForResponse(
      (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
      { timeout: 45_000 },
    );
    const started = page.waitForResponse(
      (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
      { timeout: 45_000 },
    );

    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 20_000 });
    await input.fill(prompt);
    await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
    await page.getByTestId('chat-send-button').click();

    const createdResponse = await created;
    expect(createdResponse.status(), 'the send must create a real conversation').toBe(201);
    projectId = CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
    expect(projectId, 'the conversation must belong to a project').not.toBe('');
    conversationId = ((await createdResponse.json()) as { id?: string }).id ?? '';
    expect(conversationId).toMatch(/^\d+$/);

    const startResponse = await started;
    expect(startResponse.status(), `the turn was refused: ${(await startResponse.text()).slice(0, 300)}`).toBe(200);

    // Waited on the STORE, and on this run's own token: readStoredMessageGroups
    // below is what actually names the answer's uuid — this just proves the
    // turn settled before that read runs.
    await expect
      .poll(
        async () => {
          const groups = await readStoredMessageGroups(page, projectId, conversationId);
          return groups.length;
        },
        { timeout: 120_000, message: 'the turn never stored a question + answer pair' },
      )
      .toBe(2);

    await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 60_000 });
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

    const groups = await readStoredMessageGroups(page, projectId, conversationId);
    expect(groups.length, 'a completed turn stores the question and its answer').toBe(2);
    // Oldest first (readStoredMessageGroups' own sort_order=asc) — the
    // SECOND group is the assistant's reply to the first.
    const answerUuid = groups[1]?.uuid ?? '';
    expect(answerUuid, 'the answer must carry a real message-group uuid').toMatch(/^[0-9a-f-]{36}$/);

    const feedbackUrl = `${API_BASE}/elitea_core/message_feedback/prompt_lib/${projectId}/${answerUuid}`;

    // ── 1. the FIRST vote, through the real control on the real transcript ──
    const answer = page.getByTestId('application-answer').last();
    await expect(answer).toBeVisible({ timeout: 30_000 });
    // `exact: true` — Playwright's accessible-name match is a case-insensitive
    // SUBSTRING match by default, and "dislike this answer" contains "like
    // this answer" as a literal substring, so the un-exact query resolves to
    // BOTH buttons (a strict-mode violation) once the row actually renders.
    const likeButton = answer.getByRole('button', { name: 'Like this answer', exact: true });
    await expect(likeButton, 'the answer must offer a like control').toBeVisible({ timeout: 15_000 });

    const posted = page.waitForResponse(
      (r) => r.url() === feedbackUrl && r.request().method() === 'POST',
      { timeout: 20_000 },
    );
    await likeButton.click();
    expect((await posted).status(), 'the UI vote must reach the server').toBe(200);
    await expect(likeButton, 'the clicked thumb must show as pressed').toHaveAttribute('aria-pressed', 'true', {
      timeout: 15_000,
    });

    // ── 2. read it back — the DoD's own wording ─────────────────────────
    const afterUiVote = await page.request.get(feedbackUrl);
    expect(afterUiVote.ok()).toBe(true);
    const afterUiVoteBody = (await afterUiVote.json()) as FeedbackSummary;
    expect(afterUiVoteBody.likes).toBe(1);
    expect(afterUiVoteBody.dislikes).toBe(0);
    expect(afterUiVoteBody.mine).toEqual({ rating: 1 });

    // ── 3. UPSERT: a second vote from the SAME caller replaces it ───────
    const withComment = await page.request.post(feedbackUrl, {
      data: { rating: -1, comment: `${token} needs more detail` },
    });
    expect(withComment.ok(), `dislike+comment was refused: ${await withComment.text()}`).toBe(true);
    const withCommentBody = (await withComment.json()) as FeedbackSummary;
    expect(withCommentBody.likes, 'the earlier like must be REPLACED, not kept alongside the dislike').toBe(0);
    expect(withCommentBody.dislikes).toBe(1);
    expect(withCommentBody.mine).toEqual({ rating: -1, comment: `${token} needs more detail` });

    // Read back independently — a separate GET must agree with the POST's
    // own response, proving the row is really stored and not just echoed.
    // (Not asserted through the UI here: these two writes go straight
    // through `page.request`, bypassing the app's own react-query cache, so
    // the composer would only repaint on its own next refetch — a fact
    // about client caching, not about the server this step is testing.)
    const afterUpsert = await page.request.get(feedbackUrl);
    const afterUpsertBody = (await afterUpsert.json()) as FeedbackSummary;
    expect(afterUpsertBody).toEqual(withCommentBody);

    // ── 4. RETRACT: DELETE removes only this caller's vote ──────────────
    const retracted = await page.request.delete(feedbackUrl);
    expect(retracted.ok()).toBe(true);
    const retractedBody = (await retracted.json()) as FeedbackSummary;
    expect(retractedBody.likes).toBe(0);
    expect(retractedBody.dislikes).toBe(0);
    expect(retractedBody.mine, 'no vote from this caller remains').toBeUndefined();

    const afterRetract = await page.request.get(feedbackUrl);
    const afterRetractBody = (await afterRetract.json()) as FeedbackSummary;
    expect(afterRetractBody).toEqual(retractedBody);

    // ── 5. voting on a message this project never had is a 404 ─────────
    const unknownUuid = '00000000-0000-4000-8000-000000000000';
    const refused = await page.request.post(
      `${API_BASE}/elitea_core/message_feedback/prompt_lib/${projectId}/${unknownUuid}`,
      { data: { rating: 1 } },
    );
    expect(refused.status(), 'feedback on a nonexistent message must be refused, not silently accepted').toBe(404);
  } finally {
    if (conversationId && projectId) {
      await deleteConversation(page.request, conversationId, projectId).catch(() => {});
    }
  }
});
