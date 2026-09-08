/**
 * IMAGE CREATION against a real image-capable model — the live port of
 * `qa/elitea-testing-public/automation/tests/ui/chat/test_image_creation.py`:
 *
 *  - LIVE-IMG-1 ← `TestImageCreation::test_create_image[detailed_description]`
 *  - LIVE-IMG-2 ← `TestImageCreation::test_create_image[minimal_prompt]`
 *
 * ## Why this cannot live on any existing project
 *
 * Every other chat lane runs against `deploy/mock-llm/server.py`, which serves
 * chat completions and nothing else — no image endpoint at all. A journey that
 * asked it for an image would fail for a reason that says nothing about the
 * product, which is why the coverage map put these two out of scope. They are
 * in scope again with their own lane and their own prerequisite:
 * `E2E_LIVE_IMAGE_MODEL` names an image-capable model row the stack's own
 * catalogue offers, exactly as the picker spells it.
 *
 * ## No skip, and a loud failure when the lane is selected without the model
 *
 * `playwright.config.ts` lists this file only when `E2E_LIVE_IMAGE_MODEL` is
 * set, so `--project=image-live` with no model runs zero tests and Playwright
 * ends the run with its own "no tests found" error and a non-zero exit. That
 * is the loud failure, and it is louder than a skip: a skipped test is
 * reported as a pass-shaped row, and the whole reason these lanes exist is
 * that they never claim coverage they did not take.
 *
 * Once the lane IS selected with a model, nothing below is optional. If the
 * named model is not in the picker the journey fails on that line rather than
 * quietly falling back to whatever else the catalogue offers — a fallback is
 * how a run against a text-only model would report a green image lane.
 *
 * ## The order the legacy body insists on
 *
 * Model FIRST, module SECOND. The legacy comment says why in one line —
 * "switching models resets internal tool toggles" — and the same is true here:
 * the conversation's `meta.internal_tools` is rewritten when the model
 * selection changes, so a run that armed the module first would send a turn
 * with no image tool at all and fail on a missing image.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, fillComposer } from '../fixtures/api';

import { liveImageModel } from './liveEnv';
import { selectedProjectId } from './liveToolkits';

/** The `INTERNAL_TOOLS_LIST` entry the legacy "Image creation" switch drives. */
const IMAGE_TOOL = 'image_generation';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The conversation's `meta.internal_tools`, as the server holds it right now. */
async function readInternalTools(
  request: APIRequestContext,
  projectId: string,
  conversationId: string,
): Promise<readonly string[]> {
  const response = await request.get(
    `${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`,
  );
  expect(response.status(), await response.text()).toBe(200);
  const body = (await response.json()) as { meta?: { internal_tools?: readonly string[] } };
  return body.meta?.internal_tools ?? [];
}

/**
 * Select the image model, then arm the image module. Returns nothing; every
 * step asserts, because each one is a different way this lane goes quiet.
 */
async function armImageGeneration(page: Page, projectId: string, conversationId: string): Promise<void> {
  const model = liveImageModel();

  await page.getByTestId('model-selector-button').click();
  const option = page.getByRole('menuitem').filter({ hasText: model }).first();
  await expect(
    option,
    `E2E_LIVE_IMAGE_MODEL names ${model}, and this deployment's picker does not offer it — ` +
      'the lane was selected against a stack that has no image-capable model configured',
  ).toBeVisible({ timeout: 30_000 });
  await option.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(model, { timeout: 15_000 });

  // Modules SECOND — see this file's header.
  const plus = page.getByTestId('plus-menu-button');
  await expect(plus).toBeEnabled({ timeout: 20_000 });
  await plus.click();
  const modules = page.getByTestId('plus-menu-tools');
  await expect(modules).toBeVisible({ timeout: 15_000 });
  await modules.click();

  const row = page.locator(`[data-testid="plus-submenu-item"][data-item-key="${IMAGE_TOOL}"]`);
  await expect(
    row,
    `the chat Modules panel does not offer ${IMAGE_TOOL} — it is a GATED tool, so the ` +
      'deployment running this lane must have its feature gate open',
  ).toHaveCount(1, { timeout: 20_000 });
  const toggle = row.locator('input[type="checkbox"]');
  await expect(toggle).not.toBeChecked();
  await toggle.click();

  // The SERVER's copy, not the switch: a toggle that painted itself on and
  // persisted nothing would send a turn with no image tool.
  await expect
    .poll(async () => [...(await readInternalTools(page.request, projectId, conversationId))], {
      timeout: 20_000,
      message: 'arming the image module must persist onto the conversation',
    })
    .toContain(IMAGE_TOOL);

  await page.keyboard.press('Escape');
}

/**
 * One image turn: send the prompt, require admission, and require an image in
 * the answer.
 *
 * The image is read off the STORED conversation as well as off the screen. An
 * `<img>` in the message list can come from markdown the model wrote about an
 * image it never made; a stored `attachment_message` item cannot.
 */
async function runImageTurn(page: Page, projectId: string, conversationId: string, prompt: string): Promise<void> {
  const sendButton = await fillComposer(page, prompt);
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await sendButton.click();
  const startResponse = await started;
  expect(
    startResponse.status(),
    `the image turn was refused: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);

  // Generation is minutes, not seconds — the legacy body allows 180 s and this
  // waits as long, on a poll that reads the store rather than the screen.
  const images = page
    .getByTestId('chat-message-list')
    .locator('img:not([src=""])');
  await expect(images.first(), 'the answer must render at least one image').toBeVisible({
    timeout: 240_000,
  });
  const source = await images.first().getAttribute('src');
  expect(source ?? '', 'the generated image must carry a non-empty source').not.toBe('');

  const stored = await page.request.get(
    `${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}` +
      '?messages_limit=50&sort_order=asc',
  );
  expect(stored.ok(), `reading the stored conversation answered ${stored.status()}`).toBe(true);
  const body = (await stored.json()) as {
    message_groups?: readonly { message_items?: readonly { item_type?: string }[] }[];
  };
  const items = (body.message_groups ?? []).flatMap((group) => group.message_items ?? []);
  expect(
    items.some((item) => item.item_type === 'attachment_message'),
    'the turn must have STORED the generated image — a rendered <img> alone can be ' +
      'markdown the model wrote about an image it never produced',
  ).toBe(true);
}

const CASES = [
  {
    id: 'LIVE-IMG-1',
    legacy: 'detailed_description',
    prompt: 'Generate an image of a sunset over mountains',
  },
  {
    id: 'LIVE-IMG-2',
    legacy: 'minimal_prompt',
    prompt: 'Create an image of a red apple.',
  },
] as const;

for (const testCase of CASES) {
  test(`${testCase.id}: an image-generation prompt returns a real image in the chat (legacy test_create_image[${testCase.legacy}])`, async ({
    page,
  }) => {
    // Model selection, module arming, and a generation that the legacy body
    // budgets 180 s for on its own.
    test.setTimeout(600_000);

    const name = `${AUTOTEST_PREFIX}image_${testCase.legacy}_${Date.now() % 1_000_000}`;

    // The conversation is created in the persona's OWN project, not in the
    // seeded project 1 that `createConversation` addresses: this lane runs on
    // the chat persona, whose personal project is where `/llm` resolves the
    // provider credential from (#290). A conversation in the wrong project is
    // refused at the start route, with a message about the execution path.
    await page.goto(`${BASE_URL}/app/chat`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 60_000 });
    const projectId = await selectedProjectId(page);

    const created = await page.request.post(
      `${API_BASE}/elitea_core/conversations/prompt_lib/${projectId}`,
      { data: { name } },
    );
    expect(created.ok(), `creating the conversation answered ${created.status()}`).toBe(true);
    const conversationId = String(((await created.json()) as { id?: string }).id ?? '');
    expect(conversationId, 'the created conversation must carry an id').not.toBe('');

    try {
      await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
      await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 60_000 });

      await armImageGeneration(page, projectId, conversationId);
      await runImageTurn(page, projectId, conversationId, testCase.prompt);
    } finally {
      await page.request
        .delete(`${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`)
        .catch(() => undefined);
    }
  });
}
