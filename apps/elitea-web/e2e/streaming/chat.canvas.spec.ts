/**
 * The canvas: turning part of an answer into an editable document, editing it,
 * and finding the edit still there afterwards.
 *
 * Ported by USE CASE from the legacy suite's canvas cases — the canvas opens
 * for a fenced code block in an answer, the code is edited and saved back into
 * the message, and the change survives a reload. No legacy file, test or
 * identifier is named here, only the behaviour.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS FILE IS ON `chat-stream`, AND NOT WITH THE OTHER CHAT JOURNEYS
 * ─────────────────────────────────────────────────────────────────────────────
 * A canvas is carved OUT of a stored message item: `POST /elitea_core/canvases`
 * reads the text item it is given, splits it around the selected range and
 * writes the canvas in the middle. There is no route anywhere in this service
 * that writes a `chat_message_group` without running a turn — measured, not
 * assumed: no message-create REST endpoint exists, conversation create/update
 * accept `name`/`meta`/`folder_id` and nothing else, and the standalone stack
 * every other chat journey runs on persists no message at all. So the seed for
 * this journey is one real turn against the mock model, which echoes the
 * prompt — a prompt carrying a fenced code block therefore produces a stored
 * answer carrying one.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS JOURNEY CAN AND CANNOT DRIVE, STATED RATHER THAN IMPLIED
 * ─────────────────────────────────────────────────────────────────────────────
 * The canvas EDITOR is composed and works — it opens, it edits, and closing it
 * now saves (`processes/chat/ui/useCanvasEditing.ts`, unit-tested there,
 * including the four closes that must write nothing). What is still missing is
 * the affordance that OPENS it from a message: the answer renderer does not yet
 * model canvas message items, so nothing in the transcript mounts the block's
 * edit control and no click anywhere on the chat surface can reach the canvas.
 * That is the remaining half of the port and it is disclosed here rather than
 * asserted around.
 *
 * So this journey drives the canvas's SERVER contract, which is where the data
 * loss was:
 *
 *  1. `PUT /elitea_core/canvas/prompt_lib/{p}/{canvasID}` ran
 *     `UPDATE chat_conversations SET name = …` against the CANVAS id. The
 *     edited text was written nowhere — `chat_canvas_versions` had exactly one
 *     writer, the create — and where the ids happened to collide it renamed an
 *     unrelated conversation. The route answered `{"ok": true}` either way.
 *  2. A `canvas_message` item came back from the conversation read with NO
 *     `item_details` at all, and the transcript route aggregates `text_message`
 *     items alone by design. So the moment a canvas was created, its text was
 *     invisible to every reader of that conversation.
 *
 * Both are fixed in the same change as this file and pinned by Go integration
 * tests at the repository. This journey is the statement neither of those can
 * make: that the routes a client calls, called in the order a client calls
 * them, leave the user's own words in the conversation.
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

/** The code the canvas is carved around. ASCII only: the server slices the stored text by BYTE offset, and this journey computes those offsets in JavaScript. */
const CODE_BODY = "print('autotest canvas')";

function uniqueToken(tag: string): string {
  return `AUTOTEST${tag}${Date.now().toString(36).toUpperCase()}`;
}

/** One canvas, as the create and read routes answer it. */
interface CanvasDocument {
  readonly uuid: string;
  readonly content: string;
}

/** The canvas item in a conversation, as the conversation read serves it. */
async function readCanvasFromTranscript(
  page: Page,
  projectId: string,
  conversationId: string,
): Promise<CanvasDocument | undefined> {
  const groups = await readStoredMessageGroups(page, projectId, conversationId);
  for (const group of groups) {
    for (const item of group.items) {
      if (item.itemType !== 'canvas_message') continue;
      const details = item.details;
      const version = details['latest_version'];
      const content =
        typeof version === 'object' && version !== null
          ? (version as { canvas_content?: unknown }).canvas_content
          : undefined;
      return {
        uuid: typeof details['uuid'] === 'string' ? details['uuid'] : '',
        content: typeof content === 'string' ? content : '',
      };
    }
  }
  return undefined;
}

test('a canvas carved out of an answer keeps the edit made to it', async ({ page }) => {
  // One whole agent turn — conversation create, admission, dispatch to the
  // worker, a model call and the stream back — plus the canvas round trip.
  test.setTimeout(300_000);

  const token = uniqueToken('CANVAS');
  const prompt = `autotest ${token}\n\`\`\`python\n${CODE_BODY}\n\`\`\``;

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  // The seeded model is not decoration: with nothing selected the start route
  // refuses the send with a 400 for a missing `llm_settings.model_name`.
  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
  await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await modelOption.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

  // ── the turn that stores the answer this canvas is carved out of ────────
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

    // ── the block the user would select ─────────────────────────────────
    const groups = await readStoredMessageGroups(page, projectId, conversationId);
    const answer = groups.at(-1);
    expect(answer, 'the completed turn stores the question and its answer').toBeDefined();
    const textItem = answer?.items.find((item) => item.itemType === 'text_message');
    expect(textItem, 'the stored answer must carry a text item').toBeDefined();
    const messageItemId = textItem?.id ?? '';
    expect(messageItemId, 'the create route splits ONE item, named by its id').toMatch(/^\d+$/);
    const storedText = typeof textItem?.details['content'] === 'string' ? String(textItem.details['content']) : '';
    const startsAt = storedText.indexOf(CODE_BODY);
    expect(startsAt, 'the mock model echoes the prompt, so the fenced code must be in the stored answer').toBeGreaterThan(-1);
    const endsAt = startsAt + CODE_BODY.length;

    // ── create the canvas, exactly as the editor's own client does ───────
    const createCanvas = await page.request.post(
      `${BASE_URL}/api/v2/elitea_core/canvases/prompt_lib/${projectId}`,
      {
        data: {
          message_group_id: Number(answer?.id),
          message_item_id: Number(messageItemId),
          name: 'Edit code',
          canvas_type: 'code',
          code_language: 'python',
          canvas_content_starts_at: startsAt,
          canvas_content_ends_at: endsAt,
        },
      },
    );
    expect(
      createCanvas.status(),
      `the canvas was not created: ${await describeRefusal(createCanvas)} ${(await createCanvas.text()).slice(0, 300)}`,
    ).toBe(200);
    const canvasUuid = ((await createCanvas.json()) as { uuid?: string }).uuid ?? '';
    expect(canvasUuid, 'the create must answer the canvas uuid the client addresses it by').not.toBe('');

    // The conversation READ must be able to see it. This is the projection a
    // transcript is rendered from, and a canvas invisible here is a canvas the
    // user's own conversation has lost.
    await expect
      .poll(async () => (await readCanvasFromTranscript(page, projectId, conversationId))?.content ?? '', {
        timeout: 30_000,
        message: 'the canvas item came back from the conversation read with no content',
      })
      .toBe(CODE_BODY);

    // ── the edit ────────────────────────────────────────────────────────
    const edited = `print('autotest canvas, edited ${token}')`;
    const save = await page.request.put(
      `${BASE_URL}/api/v2/elitea_core/canvas/prompt_lib/${projectId}/${canvasUuid}`,
      { data: { canvas_content: edited, code_language: 'python' } },
    );
    expect(
      save.status(),
      `the canvas save was refused: ${await describeRefusal(save)} ${(await save.text()).slice(0, 300)}`,
    ).toBe(200);

    // THE STORE, through a different route from the one that wrote it: a save
    // only its own endpoint can see is a save nobody reads.
    await expect
      .poll(async () => (await readCanvasFromTranscript(page, projectId, conversationId))?.content ?? '', {
        timeout: 30_000,
        message: 'the edit was accepted and then discarded — the failure this journey exists for',
      })
      .toBe(edited);

    // ── and it is still there for the next reader ───────────────────────
    // A fresh page load, so nothing is answered out of the client's cache.
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 30_000 });
    // The transcript survives a conversation that holds a canvas item. The
    // canvas itself is not RENDERED yet (see this file's header); what must not
    // happen is the surface breaking on an item shape it does not model.
    await expect(page.getByText('Something went wrong.')).toHaveCount(0);
    await expect(page.getByTestId('user-message')).toHaveCount(1);

    const afterReload = await readCanvasFromTranscript(page, projectId, conversationId);
    expect(afterReload?.content, 'the edit must outlive the session that made it').toBe(edited);
    expect(afterReload?.uuid, 'the canvas keeps its identity across the edit').toBe(canvasUuid);
  } finally {
    await page.request.delete(`${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`);
  }
});
