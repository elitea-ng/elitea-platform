/**
 * The canvas: turning part of an answer into an editable document, opening it
 * FROM THE TRANSCRIPT, editing it, and finding the edit still there
 * afterwards.
 *
 * Ported by USE CASE from the legacy suite's canvas cases: the canvas opens
 * for a code block, a table and a diagram in an answer; the document is
 * edited, undone, redone and saved back into the message; the change survives
 * a reload; and several canvases coexist in one conversation. No legacy file,
 * test or identifier is named here, only the behaviour.
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
 * these journeys is one real turn against the mock model, which echoes the
 * prompt — a prompt carrying a fenced code block, a table or a diagram
 * therefore produces a stored answer carrying one.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE OPENER (issue 853), AND WHAT IS STILL DRIVEN THROUGH THE API
 * ─────────────────────────────────────────────────────────────────────────────
 * The transcript now MOUNTS the canvas: the answer renderer models
 * `canvas_message` items and every one of them renders a block with an open
 * control, so the journeys below reach the editor by clicking, exactly as a
 * reader would. Until that landed this file could only drive the routes; the
 * server-contract journey it drove then is kept as the first case, because it
 * states something no click can — that the routes, called in the order a
 * client calls them, leave the user's own words in the conversation:
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
 * CREATING a canvas is still an API call in every journey here, and that is a
 * statement about the product, not a shortcut: the gesture that carves one out
 * — selecting a range of an answer and choosing to edit it — has no control in
 * this app yet. Only the OPEN, EDIT and SAVE half exists in the UI, and that
 * half is what these journeys click.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT COULD NOT BE MADE DETERMINISTIC, STATED RATHER THAN ASSERTED AROUND
 * ─────────────────────────────────────────────────────────────────────────────
 * Three legacy canvas cases have no control to drive on this app, and are
 * disclosed here rather than skipped in a way that reads as coverage:
 *
 *  * FULL SCREEN. The editor is a right-hand drawer and its header carries
 *    close / undo / redo / copy / language / table controls and no expand
 *    control at all; the reference's resizable split pane was deliberately not
 *    ported (`CanvasEditor.tsx`'s own deviation 4).
 *  * TABLE EXPORT (xlsx / csv). The reference's download footer needs
 *    `useDownloadTable` and a split button, neither of which has a port in
 *    this app — `MarkdownTableEditor.tsx`'s deviation 1 records it, and the
 *    editor renders no export control.
 *  * KEYBOARD SHORTCUTS for undo and redo. The header buttons are wired; no
 *    key handler is bound at the editor level, so a key press would prove the
 *    browser's own behaviour rather than this app's.
 *
 * Every conversation is deleted on the way out, and each run's own token makes
 * the rows it creates attributable.
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
    // The transcript survives a conversation that holds a canvas item, and
    // shows it: the block the reader opens is on screen carrying the edit that
    // was just saved through the route.
    await expect(page.getByText('Something went wrong.')).toHaveCount(0);
    await expect(page.getByTestId('user-message')).toHaveCount(1);
    await expect(page.getByTestId('canvas-block-content')).toContainText(edited, { timeout: 30_000 });

    const afterReload = await readCanvasFromTranscript(page, projectId, conversationId);
    expect(afterReload?.content, 'the edit must outlive the session that made it').toBe(edited);
    expect(afterReload?.uuid, 'the canvas keeps its identity across the edit').toBe(canvasUuid);
  } finally {
    await page.request.delete(`${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`);
  }
});

/* ════════════════════════════════════════════════════════════════════════════
 *  The transcript's own opener, and the P2 canvas kinds behind it
 * ════════════════════════════════════════════════════════════════════════════
 */

/** One stored answer to carve canvases out of, seeded by a single mock turn. */
interface SeededAnswer {
  readonly projectId: string;
  readonly conversationId: string;
  readonly groupId: string;
  readonly token: string;
}

/**
 * Sends one prompt and waits for its answer to be STORED.
 *
 * Waiting on the store and on this run's own token, not on "an answer
 * appeared": a refused turn is stored as an assistant row too, so the two are
 * indistinguishable on screen.
 */
async function seedAnswer(page: Page, token: string, prompt: string): Promise<SeededAnswer> {
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
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', { timeout: 45_000 });

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
    message: 'the answer was never stored, so there is no message to carve a canvas out of',
    contains: token,
  });

  const groups = await readStoredMessageGroups(page, projectId, conversationId);
  const answer = groups.at(-1);
  expect(answer, 'the completed turn stores the question and its answer').toBeDefined();
  return { projectId, conversationId, groupId: answer?.id ?? '', token };
}

/**
 * Carves a canvas out of whichever stored text item holds `body`.
 *
 * The item is looked up at CALL time rather than captured once, because the
 * create REWRITES the item it splits: the second canvas of a conversation runs
 * over a text item the first create produced, and an id read before that split
 * names a row that no longer exists.
 */
async function carveCanvas(
  page: Page,
  seed: SeededAnswer,
  body: string,
  canvas: { readonly name: string; readonly canvasType: string; readonly codeLanguage: string },
): Promise<string> {
  const groups = await readStoredMessageGroups(page, seed.projectId, seed.conversationId);
  const answer = groups.find((group) => group.id === seed.groupId);
  expect(answer, 'the seeded answer must still be in the conversation').toBeDefined();
  const textItem = answer?.items.find(
    (item) => item.itemType === 'text_message' && String(item.details['content'] ?? '').includes(body),
  );
  expect(textItem, `no stored text item holds the block to carve: ${body.slice(0, 60)}`).toBeDefined();
  const storedText = String(textItem?.details['content'] ?? '');
  const startsAt = storedText.indexOf(body);
  expect(startsAt, 'the mock model echoes the prompt, so the block must be in the stored answer').toBeGreaterThan(-1);

  const response = await page.request.post(`${BASE_URL}/api/v2/elitea_core/canvases/prompt_lib/${seed.projectId}`, {
    data: {
      message_group_id: Number(seed.groupId),
      message_item_id: Number(textItem?.id),
      name: canvas.name,
      canvas_type: canvas.canvasType,
      code_language: canvas.codeLanguage,
      canvas_content_starts_at: startsAt,
      canvas_content_ends_at: startsAt + body.length,
    },
  });
  expect(
    response.status(),
    `the canvas was not created: ${await describeRefusal(response)} ${(await response.text()).slice(0, 300)}`,
  ).toBe(200);
  const uuid = ((await response.json()) as { uuid?: string }).uuid ?? '';
  expect(uuid, 'the create must answer the canvas uuid the client addresses it by').not.toBe('');
  return uuid;
}

/** One canvas's stored document, read back through the conversation projection. */
async function storedCanvasContent(page: Page, seed: SeededAnswer, canvasUuid: string): Promise<string> {
  const groups = await readStoredMessageGroups(page, seed.projectId, seed.conversationId);
  for (const group of groups) {
    for (const item of group.items) {
      if (item.itemType !== 'canvas_message' || item.details['uuid'] !== canvasUuid) continue;
      const version = item.details['latest_version'];
      const content = typeof version === 'object' && version !== null ? (version as { canvas_content?: unknown }).canvas_content : undefined;
      return typeof content === 'string' ? content : '';
    }
  }
  return '';
}

/** The canvas block whose title names it — the transcript can hold several at once. */
function canvasBlockNamed(page: Page, name: string) {
  return page.getByTestId('canvas-block').filter({ has: page.getByTestId('canvas-block-title').getByText(name, { exact: true }) });
}

/** Replaces the open code editor's whole document in one insertion — CM6 ignores `fill()`. */
async function replaceEditorDocument(page: Page, text: string): Promise<void> {
  const content = page.getByTestId('chat-canvas-editor').locator('.cm-content').first();
  await expect(content, 'the canvas editor must offer a code pane').toBeVisible({ timeout: 20_000 });
  await content.click();
  await page.keyboard.press(process.platform === 'darwin' ? 'Meta+A' : 'Control+A');
  await page.keyboard.press('Backspace');
  await page.keyboard.insertText(text);
}

/**
 * A canvas is OPENED from the message, edited there, and the edit is the one
 * the server keeps — the whole gesture, through the UI, with undo and redo on
 * the way (issue 853; legacy canvas cases for opening a code block, editing
 * and saving it, undo/redo, and the edit persisting across a reload).
 */
test('the transcript opens a canvas, and what is typed in it is what the server keeps', async ({ page }) => {
  test.setTimeout(300_000);

  const token = uniqueToken('CANVASUI');
  const seed = await seedAnswer(page, token, `autotest ${token}\n\`\`\`python\n${CODE_BODY}\n\`\`\``);

  try {
    const canvasUuid = await carveCanvas(page, seed, CODE_BODY, {
      name: 'Edit code',
      canvasType: 'code',
      codeLanguage: 'python',
    });

    // A fresh load, so the block is rendered from a server read rather than
    // from whatever the sending page had in memory.
    await page.goto(`${BASE_URL}/app/chat/${seed.conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 30_000 });

    const block = page.getByTestId('canvas-block');
    await expect(block, 'the stored canvas must appear in the transcript').toHaveCount(1, { timeout: 30_000 });
    await expect(block.getByTestId('canvas-block-content')).toContainText(CODE_BODY);
    await expect(block.getByTestId('canvas-block-title')).toHaveText('Edit code');

    // ── the open ────────────────────────────────────────────────────────
    await block.getByTestId('canvas-block-open').click();
    const editor = page.getByTestId('chat-canvas-editor');
    await expect(editor, 'clicking the block must open the canvas editor').toBeVisible({ timeout: 20_000 });
    await expect(editor.locator('.cm-content').first()).toContainText(CODE_BODY, { timeout: 20_000 });

    // The block is replaced by its editing placeholder, so the same document
    // is not also shown in a copy that cannot save it.
    await expect(page.getByTestId('editing-placeholder')).toBeVisible({ timeout: 10_000 });

    // ── the edit, undone, and redone ────────────────────────────────────
    const edited = `print('autotest canvas edited in the editor ${token}')`;
    await replaceEditorDocument(page, edited);
    await expect(editor.locator('.cm-content').first()).toContainText(edited, { timeout: 10_000 });

    const undo = page.getByTestId('canvas-edit-undo');
    await expect(undo, 'an edited canvas must offer undo').toBeEnabled({ timeout: 10_000 });
    await undo.click();
    // Undo is asserted on the DOCUMENT, not on the button's own state: a
    // header that enables redo while the text never changed is the same
    // "the gesture reached nothing" defect this whole area exists for.
    await expect(editor.locator('.cm-content').first()).not.toContainText(edited, { timeout: 10_000 });

    const redo = page.getByTestId('canvas-edit-redo');
    await expect(redo, 'an undone edit must be redoable').toBeEnabled({ timeout: 10_000 });
    await redo.click();
    await expect(editor.locator('.cm-content').first()).toContainText(edited, { timeout: 10_000 });

    // ── the close, which is this editor's save ──────────────────────────
    await page.getByTestId('canvas-edit-close').click();
    await expect(editor).toBeHidden({ timeout: 20_000 });

    // THE STORE, through a read the editor does not write: a save only its own
    // endpoint can see is a save nobody reads.
    await expect
      .poll(async () => storedCanvasContent(page, seed, canvasUuid), {
        timeout: 30_000,
        message: 'closing the editor accepted the edit and then discarded it',
      })
      .toBe(edited);

    // ── and the next reader sees it ─────────────────────────────────────
    await page.goto(`${BASE_URL}/app/chat/${seed.conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 30_000 });
    await expect(page.getByTestId('canvas-block-content')).toContainText(edited, { timeout: 30_000 });
  } finally {
    await page.request.delete(
      `${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${seed.projectId}/${seed.conversationId}`,
    );
  }
});

/** The table and the diagram this journey carves out of one answer. */
const TABLE_BODY = ['| Metric | Value |', '| --- | --- |', '| alpha | 1 |'].join('\n');
const DIAGRAM_BODY = 'graph TD\n  A[Start] --> B[End]';
const DIAGRAM_AFTER = 'sequenceDiagram\n  Alice->>Bob: autotest';

/**
 * Two canvases of different kinds in ONE conversation, each opening its own
 * editor: the table grid (edit a cell, add a row) and the mermaid pane (change
 * the source to a different diagram type).
 *
 * One turn seeds both, and the second carve runs over a text item the first
 * carve produced — which is the stronger statement, not a shortcut: the split
 * writes real text items and the route must carve those exactly as it carves
 * an answer the runtime wrote.
 */
test('a table canvas and a diagram canvas live side by side, and each opens its own editor', async ({ page }) => {
  test.setTimeout(300_000);

  const token = uniqueToken('CANVASP2');
  const seed = await seedAnswer(
    page,
    token,
    `autotest ${token}\n\n${TABLE_BODY}\n\nand a diagram\n\n\`\`\`mermaid\n${DIAGRAM_BODY}\n\`\`\``,
  );

  try {
    const tableUuid = await carveCanvas(page, seed, TABLE_BODY, {
      name: 'Edit table',
      canvasType: 'table',
      codeLanguage: 'markdownTable',
    });
    const diagramUuid = await carveCanvas(page, seed, DIAGRAM_BODY, {
      name: 'Edit diagram',
      canvasType: 'diagram',
      codeLanguage: 'mermaid',
    });

    await page.goto(`${BASE_URL}/app/chat/${seed.conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 30_000 });
    await expect(
      page.getByTestId('canvas-block'),
      'both canvases of one answer must be on screen at once',
    ).toHaveCount(2, { timeout: 30_000 });

    // ── the table ───────────────────────────────────────────────────────
    await canvasBlockNamed(page, 'Edit table').getByTestId('canvas-block-open').click();
    const editor = page.getByTestId('chat-canvas-editor');
    await expect(editor, 'a table canvas must open the table editor').toBeVisible({ timeout: 20_000 });
    const grid = page.getByTestId('chat-table-canvas-grid');
    await expect(grid, 'a `markdownTable` canvas mounts the grid, not the code pane').toBeVisible({ timeout: 20_000 });

    // A cell edit: double click to enter the cell editor, replace it, commit
    // with Enter — the editor's own keyboard contract.
    const cell = grid.getByRole('gridcell').filter({ hasText: 'alpha' }).first();
    await expect(cell).toBeVisible({ timeout: 20_000 });
    await cell.dblclick();
    const cellInput = grid.getByRole('textbox').first();
    await expect(cellInput, 'a double click must open the cell editor').toBeVisible({ timeout: 10_000 });
    await cellInput.press(process.platform === 'darwin' ? 'Meta+A' : 'Control+A');
    await cellInput.fill(`gamma${token}`);
    await cellInput.press('Enter');
    await expect(grid.getByRole('gridcell').filter({ hasText: `gamma${token}` }).first()).toBeVisible({ timeout: 10_000 });

    // A row added from the header control.
    const rowsBefore = await grid.getByRole('row').count();
    await page.getByRole('button', { name: 'Add row', exact: true }).click();
    await expect
      .poll(async () => grid.getByRole('row').count(), { timeout: 10_000, message: 'the add-row control added no row' })
      .toBeGreaterThan(rowsBefore);

    await page.getByTestId('canvas-edit-close').click();
    await expect(editor).toBeHidden({ timeout: 20_000 });

    // The grid serialises back to markdown, and THAT is what the server keeps.
    await expect
      .poll(async () => storedCanvasContent(page, seed, tableUuid), {
        timeout: 30_000,
        message: 'the table edits were accepted by the grid and discarded by the save',
      })
      .toContain(`gamma${token}`);

    // ── the diagram ─────────────────────────────────────────────────────
    await page.goto(`${BASE_URL}/app/chat/${seed.conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 30_000 });
    await canvasBlockNamed(page, 'Edit diagram').getByTestId('canvas-block-open').click();
    await expect(editor, 'a diagram canvas must open the canvas editor').toBeVisible({ timeout: 20_000 });
    await expect(
      page.getByTestId('canvas-mermaid-diagram'),
      'a `mermaid` canvas mounts the rendered diagram beside its source',
    ).toBeVisible({ timeout: 20_000 });
    await expect(editor.locator('.cm-content').first()).toContainText('A[Start]', { timeout: 20_000 });

    // The diagram TYPE is the first token of a mermaid document, so switching
    // it is an edit of the source — there is no separate type control.
    await replaceEditorDocument(page, DIAGRAM_AFTER);
    await expect(editor.locator('.cm-content').first()).toContainText('sequenceDiagram', { timeout: 10_000 });
    await page.getByTestId('canvas-edit-close').click();
    await expect(editor).toBeHidden({ timeout: 20_000 });

    await expect
      .poll(async () => storedCanvasContent(page, seed, diagramUuid), {
        timeout: 30_000,
        message: 'the diagram source was edited and the save kept the old one',
      })
      .toContain('sequenceDiagram');

    // The other canvas of the same answer is untouched by the diagram's save —
    // one PUT per canvas, addressed by its own id.
    expect(
      await storedCanvasContent(page, seed, tableUuid),
      'saving one canvas must not rewrite the other',
    ).toContain(`gamma${token}`);
  } finally {
    await page.request.delete(
      `${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${seed.projectId}/${seed.conversationId}`,
    );
  }
});
