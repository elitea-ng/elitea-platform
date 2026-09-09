/**
 * The `document` canvas (issue #879): opening a WHOLE assistant reply as a
 * rich-text document (not carving code out of it), editing a heading in the
 * rendered editor, and saving the result out to an artifact bucket as
 * Markdown.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS FILE IS ON `chat-stream`
 * ─────────────────────────────────────────────────────────────────────────────
 * Same reason `chat.canvas.spec.ts`/`chat.canvasExtraction.spec.ts` are: the
 * "Open as document" control only appears on a message that is actually
 * stored, and the standalone journeys stack persists no message at all. The
 * seed is one real turn against the mock model, which echoes its prompt — a
 * prompt carrying a Markdown heading therefore produces a stored answer
 * carrying one.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PINS THAT `chat.canvas.spec.ts` DOES NOT
 * ─────────────────────────────────────────────────────────────────────────────
 * That file's canvases are all carved OUT of an answer (a fenced block, a
 * table, a selected range) into a `code`/`table`/`diagram` pane. This journey
 * is the fourth kind (#879): the "Open as document" action on the answer's
 * OWN action row, which takes the answer whole rather than a slice of it, and
 * opens it in the rich-text pane — a real ProseMirror-rendered `<h1>`, not a
 * `<pre>` of Markdown source. Editing that heading and reading the saved
 * artifact back as plain Markdown is the round-trip claim (source of truth is
 * Markdown; the editor is the view onto it) that a unit test on
 * `DocumentEditor.tsx` alone cannot make — that component's tests stand up
 * the pane directly and never touch `useCanvasCreation`, the transcript, or
 * the artifact store.
 */
import { expect, test } from '@playwright/test';
import type { Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { describeRefusal, expectStoredAssistantAnswer } from '../fixtures/api';

const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The model the standalone stack seeds; overridable for the real-model lane. */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

function uniqueToken(tag: string): string {
  return `AUTOTEST${tag}${Date.now().toString(36).toUpperCase()}`;
}

function uniqueBucket(): string {
  return `autotest-cvsdoc-${Date.now().toString(36)}${Math.floor(Math.random() * 1000)}`;
}

/** Sends one prompt, waits for its answer to be STORED, and returns the ids the rest of the journey needs. */
async function seedAnswer(
  page: Page,
  token: string,
  prompt: string,
): Promise<{ readonly projectId: string; readonly conversationId: string }> {
  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

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

  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 120_000,
    message: 'the answer was never stored, so there is nothing to open as a document',
    contains: token,
  });

  return { projectId, conversationId };
}

test('a whole reply opens as a document, a heading edited in it round-trips, and the save lands as Markdown', async ({
  page,
}) => {
  test.setTimeout(300_000);

  const token = uniqueToken('CANVASDOC');
  const headingText = `${token} Heading`;
  const bodyText = `Autotest prose body under the heading ${token}.`;
  const prompt = `autotest ${token}\n\n# ${headingText}\n\n${bodyText}`;

  const { projectId, conversationId } = await seedAnswer(page, token, prompt);
  const bucket = uniqueBucket();

  try {
    // A bucket must already EXIST before the save dialog opens — its picker
    // only lists buckets the project already has, it does not create one.
    const bucketCreated = await page.request.post(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}`, {
      data: { name: bucket },
    });
    expect([200, 201], `the bucket seed failed: ${await describeRefusal(bucketCreated)}`).toContain(
      bucketCreated.status(),
    );

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 30_000 });

    const answer = page.getByTestId('application-answer').last();
    await expect(answer).toContainText(headingText, { timeout: 30_000 });

    // ── the opener: "Open as document" on the WHOLE reply ──────────────────
    const openAsDocument = answer.getByTestId('answer-open-as-document');
    await expect(openAsDocument, 'a fresh, unsplit reply must offer to open as a document').toBeVisible({
      timeout: 20_000,
    });
    await openAsDocument.click();

    // The create rewrites the answer into one canvas_message item — the
    // transcript re-renders it as a block, without a reload.
    const block = page.getByTestId('canvas-block');
    await expect(block, 'the whole-reply document canvas must appear in the transcript').toHaveCount(1, {
      timeout: 30_000,
    });
    await expect(block.getByTestId('canvas-block-title')).toHaveText('Edit document');
    await expect(block.getByTestId('canvas-block-content')).toContainText(headingText);

    // ── the open: a real rich-text pane, not a plain-text preview ──────────
    await block.getByTestId('canvas-block-open').click();
    const editor = page.getByTestId('chat-canvas-editor');
    await expect(editor, 'clicking the block must open the canvas editor').toBeVisible({ timeout: 20_000 });
    const documentPane = editor.getByTestId('canvas-document-editor');
    await expect(documentPane).toBeVisible({ timeout: 20_000 });

    // No language select for a document canvas — it is a top-level kind, like a table.
    await expect(editor.getByLabel('Select language')).toHaveCount(0);

    const heading = documentPane.getByRole('heading', { level: 1 });
    await expect(heading, 'the Markdown "# " heading must render as a real <h1>, not literal syntax').toHaveText(
      headingText,
    );
    await expect(documentPane).toContainText(bodyText);

    // ── the edit: append a word to the rendered heading ────────────────────
    const editedSuffix = ' EDITED';
    await heading.click();
    await page.keyboard.press('End');
    await page.keyboard.type(editedSuffix);
    await expect(heading).toHaveText(headingText + editedSuffix, { timeout: 10_000 });

    // ── save to artifacts ───────────────────────────────────────────────────
    await editor.getByTestId('canvas-edit-save-to-artifacts').click();
    const dialog = page.getByTestId('canvas-save-to-artifacts-dialog');
    await expect(dialog).toBeVisible({ timeout: 10_000 });
    // The lone bucket this run just seeded auto-fills — proof the picker
    // reached the real bucket list rather than opening empty.
    await expect(page.getByTestId('canvas-save-bucket-select')).toHaveValue(bucket, { timeout: 10_000 });

    const fileName = `${token}.md`;
    const nameInput = page.getByTestId('canvas-save-filename-input');
    await nameInput.fill(fileName);

    const uploadResponse = page.waitForResponse(
      (r) => r.url().includes(`/api/v2/artifacts/objects/${projectId}/${bucket}`) && r.request().method() === 'POST',
      { timeout: 20_000 },
    );
    await page.getByTestId('canvas-save-submit').click();
    const uploaded = await uploadResponse;
    expect(uploaded.status(), await uploaded.text()).toBe(201);
    await expect(dialog).toBeHidden({ timeout: 10_000 });

    // ── THE STORE, not the screen: the saved object as plain Markdown ──────
    const downloaded = await page.request.get(`${BASE_URL}/api/v2/artifacts/objects/${projectId}/${bucket}/${fileName}`);
    expect(downloaded.status()).toBe(200);
    const storedBody = await downloaded.text();
    // The heading round-trips as a real `#` Markdown heading (source of
    // truth is Markdown — the rich-text pane is the VIEW onto it), carrying
    // the edit, and the prose body survives beside it.
    expect(storedBody).toMatch(new RegExp(`^#\\s+${headingText}${editedSuffix}`, 'm'));
    expect(storedBody).toContain(bodyText);
  } finally {
    await page.request.delete(`${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`);
    await page.request.delete(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}/${bucket}`);
  }
});
