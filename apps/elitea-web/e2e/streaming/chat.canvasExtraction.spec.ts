/**
 * Carving a canvas OUT of an answer: the whole reply, and a slice from the
 * middle of one.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's canvas-extraction cases: turning a whole assistant
 * reply into a code canvas, and selecting a slice from the middle of one so
 * that the message becomes text / canvas / text. Its third case — an inverted
 * selection — needs no message at all and is asserted in
 * `e2e/journeys/api/api.chat-traces-canvas.spec.ts`, beside the route's other
 * refusals.
 *
 * `chat.canvas.spec.ts` next door owns the canvas's EDIT path: create one,
 * change it, and find the change again. This file owns the SPLIT — what the
 * create does to the message it carves, which is the half that rewrites the
 * user's stored transcript.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY IT NEEDS A REAL TURN
 * ─────────────────────────────────────────────────────────────────────────────
 * A canvas is carved out of a stored `text_message` item, and no route in this
 * service writes a message group without running a turn: conversation create
 * and update accept `name`, `meta`, `folder_id` and nothing else, and the plain
 * journeys stack persists no message at all. So the seed is one turn against
 * the mock model, which echoes its prompt — a prompt with an identifiable
 * middle therefore produces an answer with one.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY BOTH CASES SHARE ONE TURN
 * ─────────────────────────────────────────────────────────────────────────────
 * The second create runs over the text item the FIRST one produced. That is not
 * a shortcut around a second turn — it is the stronger statement: the split
 * writes real `text_message` items, and the route must be able to carve those
 * exactly as it carves an answer the runtime wrote. A whole-item selection then
 * leaves no surrounding text, which is the legacy "whole reply" case with its
 * boundary offsets (`0` and the length) genuinely exercised.
 *
 * The conversation is deleted on the way out, and the run's own token makes
 * every row it creates attributable.
 */
import { expect, test } from '@playwright/test';
import type { Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { describeRefusal, expectStoredAssistantAnswer, readStoredMessageGroups } from '../fixtures/api';

const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The model the standalone stack seeds; overridable for the real-model lane. */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/**
 * The slice the canvas is carved around, and the words that must survive on
 * either side of it.
 *
 * ASCII only: the server slices the stored text by BYTE offset and this journey
 * computes those offsets in JavaScript.
 */
const CODE_BODY = "print('autotest canvas split')";
const HEAD_MARKER = 'HEADTEXT';
const TAIL_MARKER = 'TAILTEXT';

/** One message group's items, as the per-message read serves them. */
interface MessageItem {
  readonly id: number;
  readonly itemType: string;
  readonly details: Record<string, unknown>;
}

/**
 * `GET /elitea_core/message/prompt_lib/{p}/{groupUuid}` — the read a client
 * makes for ONE message.
 *
 * This is the projection the split has to be visible in: it orders items by
 * `order_index` and names each one's `item_type`, which is the only way to
 * state that a message became text / canvas / text and in that order.
 */
async function readMessageItems(
  page: Page,
  projectId: string,
  groupUuid: string,
): Promise<readonly MessageItem[]> {
  const response = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/message/prompt_lib/${projectId}/${groupUuid}`,
  );
  expect(
    response.status(),
    `the per-message read failed: ${(await response.text()).slice(0, 300)}`,
  ).toBe(200);
  const body = (await response.json()) as {
    message_items?: readonly { id?: unknown; item_type?: unknown; item_details?: unknown }[];
  };
  return (body.message_items ?? []).map((item) => ({
    id: Number(item.id ?? 0),
    itemType: typeof item.item_type === 'string' ? item.item_type : '',
    details:
      typeof item.item_details === 'object' && item.item_details !== null
        ? (item.item_details as Record<string, unknown>)
        : {},
  }));
}

/** The canvas content of one item, as the conversation read serves it. */
async function canvasContents(
  page: Page,
  projectId: string,
  conversationId: string,
): Promise<readonly string[]> {
  const groups = await readStoredMessageGroups(page, projectId, conversationId);
  return groups
    .flatMap((group) => group.items)
    .filter((item) => item.itemType === 'canvas_message')
    .map((item) => {
      const version = item.details['latest_version'];
      const content =
        typeof version === 'object' && version !== null
          ? (version as { canvas_content?: unknown }).canvas_content
          : undefined;
      return typeof content === 'string' ? content : '';
    });
}

test('a canvas can be carved out of the middle of an answer and out of a whole message', async ({
  page,
}) => {
  // One whole agent turn — conversation create, admission, dispatch to the
  // worker, a model call and the stream back — plus two canvas round trips.
  test.setTimeout(300_000);

  const token = `AUTOTESTSPLIT${Date.now().toString(36).toUpperCase()}`;
  const prompt = `autotest ${token} ${HEAD_MARKER}\n${CODE_BODY}\n${TAIL_MARKER}`;

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  // The seeded model is not decoration: with nothing selected the start route
  // refuses the send with a 400 for a missing `llm_settings.model_name`.
  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
  await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await modelOption.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

  const created = page.waitForResponse(
    (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 45_000,
  });

  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await input.fill(prompt);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();

  const createdResponse = await created;
  expect(createdResponse.status(), 'the send must create a real conversation').toBe(201);
  const projectId = CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
  expect(projectId, 'the conversation must belong to a project').not.toBe('');
  const conversationId = ((await createdResponse.json()) as { id?: string }).id ?? '';
  expect(conversationId).toMatch(/^\d+$/);

  const startResponse = await started;
  expect(startResponse.status(), `the turn was refused: ${(await startResponse.text()).slice(0, 300)}`).toBe(200);

  try {
    // Waited on the STORE and on this run's own token: a refused turn is
    // stored as an assistant row too, so "an answer appeared" cannot tell the
    // two apart.
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 120_000,
      message: 'the answer was never stored, so there is no message to carve a canvas out of',
      contains: token,
    });

    const groups = await readStoredMessageGroups(page, projectId, conversationId);
    const answer = groups.at(-1);
    expect(answer, 'the completed turn stores the question and its answer').toBeDefined();
    const groupUuid = answer?.uuid ?? '';
    expect(groupUuid, 'the per-message read addresses a group by uuid').not.toBe('');

    const before = await readMessageItems(page, projectId, groupUuid);
    expect(before.map((item) => item.itemType), 'a fresh answer is one text item').toEqual([
      'text_message',
    ]);
    const storedText = String(before[0]?.details['content'] ?? '');
    const startsAt = storedText.indexOf(CODE_BODY);
    expect(
      startsAt,
      'the mock model echoes the prompt, so the code must be inside the stored answer',
    ).toBeGreaterThan(0);
    const endsAt = startsAt + CODE_BODY.length;
    expect(endsAt, 'the selection must leave text on BOTH sides, or nothing is split').toBeLessThan(
      storedText.length,
    );

    const canvasesURL = `${BASE_URL}/api/v2/elitea_core/canvases/prompt_lib/${projectId}`;

    // ── 1. the middle slice: one message becomes text / canvas / text ────
    const split = await page.request.post(canvasesURL, {
      data: {
        message_group_id: Number(answer?.id),
        message_item_id: before[0]?.id,
        name: 'Edit code',
        canvas_type: 'code',
        code_language: 'python',
        canvas_content_starts_at: startsAt,
        canvas_content_ends_at: endsAt,
      },
    });
    expect(
      split.status(),
      `the canvas was not created: ${await describeRefusal(split)} ${(await split.text()).slice(0, 300)}`,
    ).toBe(200);

    const afterSplit = await readMessageItems(page, projectId, groupUuid);
    expect(
      afterSplit.map((item) => item.itemType),
      'a slice out of the middle must leave the text on either side of it standing',
    ).toEqual(['text_message', 'canvas_message', 'text_message']);
    expect(
      String(afterSplit[0]?.details['content'] ?? ''),
      'the text before the selection is not the text that was there',
    ).toBe(storedText.slice(0, startsAt));
    expect(
      String(afterSplit[2]?.details['content'] ?? ''),
      'the text after the selection is not the text that was there',
    ).toBe(storedText.slice(endsAt));
    expect(
      await canvasContents(page, projectId, conversationId),
      'the canvas holds something other than the exact selection',
    ).toEqual([CODE_BODY]);

    // ── 2. a WHOLE message item becomes a canvas, with nothing left over ─
    const head = afterSplit[0];
    const headText = String(head?.details['content'] ?? '');
    expect(headText, 'the leading text item is empty, so there is nothing to carve').not.toBe('');

    const whole = await page.request.post(canvasesURL, {
      data: {
        message_group_id: Number(answer?.id),
        message_item_id: head?.id,
        name: 'Whole item',
        canvas_type: 'code',
        code_language: 'text',
        canvas_content_starts_at: 0,
        canvas_content_ends_at: headText.length,
      },
    });
    expect(
      whole.status(),
      // 200, not 201: the create answers the canvas the client then renders,
      // and the client normalises this body into its canvas cache.
      `carving a whole item failed: ${await describeRefusal(whole)} ${(await whole.text()).slice(0, 300)}`,
    ).toBe(200);
    const detail = (await whole.json()) as Record<string, unknown>;
    expect(detail['item_type']).toBe('canvas_message');
    expect(detail['canvas_type']).toBe('code');
    expect(detail['name']).toBe('Whole item');
    expect(detail['editors'], 'a new canvas has no editors yet').toEqual([]);
    const latest = (detail['latest_version'] ?? {}) as Record<string, unknown>;
    expect(
      latest['canvas_content'],
      'a selection spanning the whole item must carve the whole item',
    ).toBe(headText);
    expect(latest['code_language']).toBe('text');

    const afterWhole = await readMessageItems(page, projectId, groupUuid);
    expect(
      afterWhole.map((item) => item.itemType),
      'carving a whole item must leave NO empty text item behind it',
    ).toEqual(['canvas_message', 'canvas_message', 'text_message']);

    // Both canvases are readable through the projection a transcript renders
    // from, in the order the message holds them. A canvas the conversation
    // read cannot see is a canvas the user has lost.
    expect(await canvasContents(page, projectId, conversationId)).toEqual([headText, CODE_BODY]);

    // The surface survives a message made of two canvases and a tail. The
    // canvas item is not RENDERED yet — `chat.canvas.spec.ts`'s header records
    // that gap — so what must not happen is the transcript breaking on it.
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 30_000 });
    await expect(page.getByText('Something went wrong.')).toHaveCount(0);
    await expect(page.getByTestId('user-message')).toHaveCount(1);
  } finally {
    await page.request.delete(`${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`);
  }
});
