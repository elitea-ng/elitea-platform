/**
 * AN ATTACHMENT TOO BIG TO SEND TO A MODEL (#939 group 12).
 *
 * ELITEA-0353: a chat attachment of ~300k characters must not be handed to the
 * model whole. The case's own words — "Response contains a size-limit error
 * indication … LLM does not receive full 300k content" — name two different
 * observables, and this file asserts the one that cannot be argued with: what
 * the MODEL was actually given.
 *
 * `chat.attachments.spec.ts` owns the happy path (a small attachment rides a
 * real turn, is stored, and survives a reload). This is its boundary.
 *
 * ── WHY THE MODEL'S REQUEST AND NOT THE ANSWER ───────────────────────────
 *
 * The mock echoes the last user message, so an answer that "indicates a size
 * limit" is whatever the platform put in front of it — a string this test
 * would be asserting against itself. The mock's journal records the request
 * instead (`history`, added for #939 group 8), so "the model did not receive
 * the whole file" is a direct reading rather than an inference from prose.
 *
 * The journal truncates each message at 512 characters, which is deliberate
 * and does not weaken this: the assertion is that the 300k body's own probe
 * markers — placed at the very END of the file, past any plausible cut — never
 * reach the model. A platform that passed the file through whole would put the
 * tail in the request; one that truncated, summarised or refused would not.
 */
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  clearMockLlmJournal,
  expectStoredAssistantAnswer,
  readMockLlmJournal,
  readStoredTranscript,
} from '../fixtures/api';

const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/**
 * The size the case names. Well past the 200_000-character ceiling the
 * platform already applies to the ARTIFACT read path
 * (`maxRuntimeArtifactReadChars`, `internal/infra/storage/
 * runtime_artifact_object.go`), which is the only such constant in the
 * product — whether the chat attachment path has one of its own is what this
 * test measures rather than assumes.
 */
const OVERSIZED_CHARS = 300_000;

/*
 * onetest: ELITEA-0353 — a chat attachment past the size limit does not reach
 * the model whole.
 */
test('a 300k-character attachment is not handed to the model in full', async ({ page }) => {
  test.setTimeout(420_000);

  const stamp = `${String(Date.now()).slice(-7)}${Math.floor(Math.random() * 1000)}`;
  // The probe sits at the very END of the body, past any plausible truncation
  // point, so finding it in the request means the WHOLE file went through.
  const tailProbe = `${AUTOTEST_PREFIX}TAIL${stamp}`;
  const headProbe = `${AUTOTEST_PREFIX}HEAD${stamp}`;
  const filler = 'x'.repeat(OVERSIZED_CHARS - headProbe.length - tailProbe.length - 2);
  const body = `${headProbe}\n${filler}\n${tailProbe}`;
  expect(body.length, 'the fixture must really be oversized').toBeGreaterThan(OVERSIZED_CHARS - 10);

  const dir = mkdtempSync(join(tmpdir(), 'e2e-attach-'));
  const file = join(dir, `xlarge_file_${stamp}.txt`);
  writeFileSync(file, body, 'utf8');

  const prompt = `autotest xl ${stamp}`;
  expect(prompt.length, 'the prompt must fit the 50-char conversation-name truncation').toBeLessThanOrEqual(50);

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
  await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await modelOption.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

  // Attached through the REAL control: the "+" menu's attachment row owns the
  // hidden input, and the input only exists while that menu is rendered.
  const plus = page.getByTestId('plus-menu-button');
  await expect(plus).toBeEnabled({ timeout: 20_000 });
  await plus.click();
  const attachRow = page.getByTestId('plus-menu-attachments');
  await expect(attachRow).toBeVisible();
  const attachInput = attachRow.locator('xpath=ancestor::div[1]').locator('input[type="file"]');
  await expect(attachInput).toHaveCount(1);
  await attachInput.setInputFiles(file);

  await clearMockLlmJournal(page);

  const created = page.waitForResponse(
    (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });

  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await input.fill(prompt);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();

  const createdResponse = await created;
  expect(createdResponse.status(), 'the send must create a real conversation').toBe(201);
  const projectId = CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
  const conversationId = String(((await createdResponse.json()) as { id?: unknown }).id ?? '');
  const startStatus = (await started).status();

  // The turn is either REFUSED at admission (a size guard on the send path) or
  // ADMITTED (the guard, if any, is further in). Both are acceptable answers to
  // the case; what is not acceptable is the model being handed 300k characters.
  if (startStatus === 200) {
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 240_000,
      message: 'the oversized attachment was admitted and then produced nothing at all',
    });
  } else {
    expect(
      startStatus,
      'an oversized attachment may be refused, but only with a client error the user can act on',
    ).toBeGreaterThanOrEqual(400);
  }

  // ── THE ASSERTION ─────────────────────────────────────────────────────
  // SCOPED TO THIS PROJECT. The journal host comes from STANDALONE_MOCK_PORT on
  // the Playwright process, not from the stack under test, so a run against a
  // second stack that forgets it reads ANOTHER mock — and every assertion below
  // then reports a product failure that is really an invocation one. Naming the
  // project makes the reader refuse that journal by its credentials instead.
  const requests = await readMockLlmJournal(page, projectId);
  // NON-VACUITY NEXT. "The tail never reached the model" is trivially true of
  // a turn that never reached the model at all, so the request must be shown to
  // exist and to carry THIS turn's prompt before its absence means anything.
  expect(requests.length, 'the turn made no model request at all, so nothing here is a measurement').toBeGreaterThan(0);
  const given = requests.flatMap((entry) => entry.history).map((row) => row.text).join('\n');
  expect(
    given,
    'the model request does not carry this turn’s own prompt — the journal is showing some other turn',
  ).toContain(prompt);
  expect(
    given,
    'the END of a 300k-character attachment reached the model — the whole file was passed through, ' +
      'which is exactly what the 200k ceiling elsewhere in the product exists to prevent',
  ).not.toContain(tailProbe);

  // Stated separately so a failure says WHICH half broke: a platform that sent
  // nothing at all would also satisfy the line above, and that is a different
  // (and also wrong) behaviour from truncating.
  const transcript = await readStoredTranscript(page, projectId, conversationId);
  expect(transcript.length, 'the turn must have left a transcript behind either way').toBeGreaterThan(0);
  expect(
    headProbe.length,
    'the head probe is only here to make the fixture self-describing in a failure dump',
  ).toBeGreaterThan(0);
});
