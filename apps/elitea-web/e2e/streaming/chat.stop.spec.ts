/**
 * STOP, pressed while the answer is still arriving.
 *
 * Every other journey in this directory watches a turn RUN TO COMPLETION.
 * Nothing watched one being cut off, and cancellation is the interaction with
 * the most moving parts of any in the chat surface: a client stream to detach,
 * a server-side execution to mark `CANCELLED`, and a half-written assistant row
 * that must be KEPT rather than deleted or flagged as a failure.
 *
 * WHAT MAKES THIS DISCRIMINATING, and why each assertion is not the obvious one
 * ─────────────────────────────────────────────────────────────────────────────
 *  1. The DELETE is asserted as a RESPONSE, not as a click. `transport.stop`
 *     fires it with `void stopChatTask(...).catch(() => undefined)`
 *     (features/chat-messages/model/useChatStreamTransport.ts) — the failure is
 *     deliberately swallowed, so a route that 404s or 409s is invisible from
 *     the browser. Requiring 204 here is the only place that can see it. The
 *     path itself was dead for a whole release (`NOTE(#126)` in
 *     services/elitea-main/internal/api/router.go still records the group of
 *     routes that were never registered), and the UI would have looked exactly
 *     the same.
 *
 *  2. The stored row must STOP GROWING. "The spinner went away" is what the
 *     client does to its own state and would pass against a backend that kept
 *     the run alive, kept billing it and kept appending to the row. Two reads a
 *     gap apart, compared by length, is the only observation that can tell a
 *     cancelled execution from a detached browser.
 *
 *  3. The row must survive, non-empty and NOT `is_error`. Cancellation shares
 *     its projection with the delete path: `ProjectCurrentAgentStop`
 *     (services/elitea-main/internal/db/queries/agent_cancel.sql) DELETES the
 *     question/answer pair when the response has no items yet, and only
 *     RETAINS it when something has already landed. So the journey waits for
 *     stored content BEFORE it presses Stop — otherwise it would be asserting
 *     against whichever branch the timing happened to take.
 *
 *  4. The kept text must be SHORTER than what the model was scripted to say.
 *     Without that, a run that was never interrupted at all — mock too fast,
 *     click too late — passes every assertion above.
 *
 * THE MODEL IS PINNED TO THE MOCK, deliberately and not through
 * `E2E_CHAT_MODEL`. This journey does not merely need "a model"; it needs one
 * whose output length is known in advance and whose token rate is slow enough
 * to act during. Following a stack that has been pointed at a real provider
 * would silently turn assertion 4 into a comparison against a sentence nobody
 * wrote. See `deploy/mock-llm/server.py` for the marker contract.
 *
 * WHY IT LIVES HERE: `journeys/**` runs against
 * `docker-compose.e2e-standalone.yml`, which has no runtime plane, no worker
 * and no model backend. The `chat-stream` project matches `streaming/chat.*`
 * and runs against the full standalone stack (`scripts/chat-stream-e2e.sh`).
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { readStoredAssistantAnswer } from '../fixtures/api';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const EVENTS_RE = /\/executions\/(\d+)\/[^/]+\/events/;
/** `DELETE /elitea_core/task/prompt_lib/{projectID}/{responseMessageID}` — the cancel. */
const CANCEL_RE = /\/elitea_core\/task\/prompt_lib\/(\d+)\/([0-9a-f-]+)$/;

/**
 * The mock model, and NOT `E2E_CHAT_MODEL` — see the header.
 *
 * Matched as a SUBSTRING of the picker row, because the name the stack seeds
 * carries a provider prefix (`vllm/E2E-MOCK-MODEL`): bifrost resolves the
 * provider from the model string alone, so `seed-llm` titles the row with the
 * prefix. Matching the suffix keeps this working against a stack seeded either
 * way. `E2E_MOCK_MODEL` overrides it for a stack whose mock is titled
 * differently.
 */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'E2E-MOCK-MODEL';

/**
 * The mock's `[[mock:slow]]` contract, restated here.
 *
 * These three values are the wire contract with `deploy/mock-llm/server.py`
 * (`SLOW_MARKER`, `SLOW_CHUNKS`, `SLOW_SENTINEL`). They are restated rather
 * than imported because the mock is a Python file in another service's tree
 * and there is nothing to import — so the pairing is asserted instead: a stack
 * whose mock does not honour the marker streams the plain echo, which is short
 * and finishes instantly, and the "content was still arriving" step below
 * fails rather than the journey passing on a turn it never interrupted.
 */
const SLOW_MARKER = '[[mock:slow]]';
const SLOW_CHUNKS = 80;
const SLOW_SENTINEL = 'MOCKSTREAMEND';

/**
 * How far into the scripted tail the store must be before Stop is pressed.
 *
 * Not "any content at all", which is the boundary condition rather than the
 * case: the cancel projection keeps the row only when it already has items, so
 * stopping on the very first chunk sits one scheduling hiccup away from the
 * DELETE branch, and the journey would then be measuring which side of that
 * race it landed on. Five chunks in is unambiguously mid-stream, and still
 * seventy-five chunks short of the end.
 */
const SLOW_PARTIAL_MARK = 'slow-005';

/** What the mock would have said in full, had nothing stopped it. */
function fullScriptedAnswer(prompt: string): string {
  const tail = Array.from({ length: SLOW_CHUNKS }, (_, index) =>
    `slow-${String(index + 1).padStart(3, '0')}`,
  ).join(' ');
  return `MOCK: ${prompt} ${tail} ${SLOW_SENTINEL}`;
}

test('Stop mid-stream cancels the run and keeps the partial answer', async ({ page }) => {
  // A slow turn is the point of this journey: 80 chunks at the mock's 250ms
  // default is ~20s of open stream, and the steps around it each carry their
  // own, much tighter bound, so a real hang fails on its own step.
  test.setTimeout(240_000);

  const prompt = `autotest stop ${Date.now()} ${SLOW_MARKER}`;

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  // ── The model ───────────────────────────────────────────────────────────
  // An ad-hoc turn resolves against a `dummy` participant carrying the model,
  // and the start route reads `llm_settings.model_name`. With nothing selected
  // the send is rejected 400 before it reaches the worker.
  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('option').filter({ hasText: MOCK_MODEL }).first();
  await expect(
    modelOption,
    `the mock model ${MOCK_MODEL} must be offered — this journey cannot run against a real provider`,
  ).toBeVisible({ timeout: 20_000 });
  await modelOption.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MOCK_MODEL, { timeout: 10_000 });

  // ── Send ────────────────────────────────────────────────────────────────
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 15_000 });
  await input.fill(prompt);
  const sendButton = page.getByTestId('chat-send-button');
  await expect(sendButton).toBeEnabled({ timeout: 5_000 });

  const created = page.waitForResponse(
    (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  const streamed = page.waitForResponse((r) => EVENTS_RE.test(r.url()), { timeout: 30_000 });

  await sendButton.click();

  const createdResponse = await created;
  expect(createdResponse.status(), 'the send must create a real conversation').toBe(201);
  const projectId = CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
  expect(projectId, 'the conversation must belong to a project').not.toBe('');
  const conversation = (await createdResponse.json()) as { id?: string };
  const conversationId = conversation.id ?? '';
  expect(conversationId, 'the conversation must carry a server-assigned id').toMatch(/^\d+$/);

  const startResponse = await started;
  expect(
    startResponse.status(),
    `the turn was refused before it could be stopped: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  const startBody = (await startResponse.json()) as { response_message_id?: string };
  // The id the cancel route addresses. Without it `transport.stop` can only
  // detach the browser, and no DELETE is sent at all — which would make the
  // cancel assertion below time out with a message about the network rather
  // than about the missing field.
  expect(
    startBody.response_message_id,
    'the start response must name the response message, or Stop has nothing to cancel',
  ).toMatch(/^[0-9a-f-]{36}$/);

  const streamResponse = await streamed;
  expect(streamResponse.status(), 'the browser must be able to read the stream').toBe(200);

  // ── Wait until the answer is PARTLY STORED, then stop ───────────────────
  // Not "until something is on screen": the cancel projection deletes the
  // question/answer pair when the response row still has no items, so pressing
  // Stop before the store has caught up would exercise the other branch and
  // leave nothing to measure. See assertion 3 in the header.
  await expect
    .poll(
      async () => (await readStoredAssistantAnswer(page, projectId, conversationId)).content,
      {
        timeout: 60_000,
        message:
          `the answer never reached ${SLOW_PARTIAL_MARK} in the store — either nothing was ` +
          'persisted (so Stop could only have deleted the question/answer pair), or the mock ' +
          'did not honour the slow marker and the turn was over before this poll began',
      },
    )
    .toContain(SLOW_PARTIAL_MARK);

  // Still streaming: the composer's Stop control replaces Send for exactly as
  // long as the run is open (`UserInput.tsx`'s `showStop`). Its absence here
  // would mean the turn had already finished, and every assertion below would
  // then be about a completed answer rather than a cancelled one.
  const stopButton = page.getByRole('button', { name: 'Stop generating' });
  await expect(
    stopButton,
    'the composer must offer Stop while the turn is open — the mock’s slow mode did not take effect',
  ).toBeVisible({ timeout: 30_000 });

  // Armed BEFORE the click, and PINNED to the response message the start
  // named. Both halves matter:
  //  - before, because `transport.stop` fires the DELETE inside the click
  //    handler, so a listener attached afterwards can miss it entirely and
  //    report "no cancel was ever sent" about a request that was;
  //  - pinned, because `ChatBox.stopGeneration` runs TWO stop paths (the
  //    transport's cancel and the socket-era `stopStreaming`), so matching the
  //    route shape alone would resolve on whichever landed first and make the
  //    identity assertion below depend on ordering rather than on behaviour.
  const cancelled = page.waitForResponse(
    (r) =>
      r.request().method() === 'DELETE' &&
      CANCEL_RE.exec(new URL(r.url()).pathname)?.[2] === startBody.response_message_id,
    { timeout: 60_000 },
  );
  await stopButton.click();

  // ── The cancel actually reached the server ─────────────────────────────
  const cancelResponse = await cancelled;
  expect(
    cancelResponse.status(),
    `the cancel was rejected: ${(await cancelResponse.text()).slice(0, 300)}`,
  ).toBe(204);

  // ── The stored answer stops growing ────────────────────────────────────
  // Two reads, a gap apart. The gap is four of the mock's own chunk intervals
  // (250ms each), so a run that is still streaming grows measurably between
  // them; anything shorter could land twice inside one interval and report a
  // live stream as a stopped one.
  const first = await readStoredAssistantAnswer(page, projectId, conversationId);
  await page.waitForTimeout(4_000);
  const second = await readStoredAssistantAnswer(page, projectId, conversationId);

  expect(second.found, 'the cancelled turn must KEEP its half-written answer').toBe(true);
  expect(
    second.content.length,
    `the answer kept growing after the cancel: ${first.content.length} -> ${second.content.length} bytes`,
  ).toBe(first.content.length);
  expect(second.content.length, 'the kept answer must not be empty').toBeGreaterThan(0);

  // A cancelled turn is not a FAILED turn. The store flags a refusal with
  // `metadata.is_error`, and a stop that lands there would make the surviving
  // row render as an error card.
  expect(second.isError, 'a user-cancelled turn must not be stored as an error').toBe(false);

  // ── It really was interrupted ──────────────────────────────────────────
  const full = fullScriptedAnswer(prompt);
  expect(
    second.content,
    'the whole scripted answer was stored — the turn ran to completion and was never interrupted',
  ).not.toContain(SLOW_SENTINEL);
  expect(
    second.content.length,
    `the kept answer (${second.content.length}) is not shorter than the full script (${full.length})`,
  ).toBeLessThan(full.length);

  // Terminal state: Stop releases the composer, exactly as a completed turn
  // does. A stop that leaves it disabled strands the conversation.
  await expect(
    page.getByTestId('chat-message-input'),
    'the composer must be released when the turn is stopped',
  ).toBeEditable({ timeout: 60_000 });
});
