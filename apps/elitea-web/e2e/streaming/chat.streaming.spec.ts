/**
 * Chat definition-of-done journey (#284 tasks 1-2).
 *
 * The single behavioural gate for the product core loop: log in, send, watch an
 * answer stream in, reload, and find it still there. The 2026-08-12 audit found
 * Send silently no-op with every unit suite green — this is what makes that
 * unfalsifiable claim falsifiable.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS FILE IS NOT UNDER e2e/journeys/
 * ─────────────────────────────────────────────────────────────────────────────
 * The `chromium`/`webkit` projects match `journeys/**` and run against
 * `docker-compose.e2e-standalone.yml`, which has NO runtime plane, NO agent
 * worker and NO model backend — an agent turn cannot happen there at all. This
 * journey needs the full standalone stack, so it is its own Playwright project
 * (`chat-stream`) driven by `scripts/chat-stream-e2e.sh`. Putting it under
 * `journeys/` would make every existing E2E run fail on a stack that was never
 * meant to serve it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT MAKES THESE ASSERTIONS DISCRIMINATING
 * ─────────────────────────────────────────────────────────────────────────────
 * The standalone stack sets `VITE_SOCKET_SERVER: ""`, so the app builds
 * `createNoopSocketClient()` — `emit`/`on` are no-ops. There is therefore NO
 * socket path in this stack: neither `chat_predict` nor `chat_message_sync` can
 * deliver anything. An assistant answer appearing on screen before any reload
 * can only have arrived over the SSE replay stream and through the ported
 * reducer. That is the whole point of running the journey here.
 *
 * The wire contract asserted below is the one the backend actually emits, NOT
 * the `chat.stream.chunk`/`chat.stream.done` pair #284's body names: every frame
 * arrives as one SSE `execution.node_event` whose semantic type lives in the
 * payload. Grep for those two names in `services/**` returns nothing; asserting
 * them would fail against a perfectly healthy stream.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { expectStoredAssistantAnswer } from '../fixtures/api';

/**
 * Route shapes, matched WITHOUT pinning a project id.
 *
 * The chat driver acts inside its own personal project — the app selects the
 * signed-in user's personal project, and that is also the project whose
 * credential the /llm hop resolves (#290). Hardcoding `1` here would assert
 * against a project this persona never opens, so the id is read back from the
 * app's own requests instead.
 */
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const EVENTS_RE = /\/executions\/(\d+)\/[^/]+\/events/;

/**
 * The model `seed-llm` seeds into every personal project.
 *
 * Overridable because the same stack can be seeded against a real provider —
 * `LLM_PROVIDER=vllm deploy/scripts/standalone-stack.sh seed-llm` names its own
 * model, and the picker then offers that instead. Continuous integration seeds
 * the offline mock and never sets the variable, so the default is what runs
 * there; locally it is what lets this journey be driven against the model the
 * operator actually has.
 */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/** `ChatBox` names the conversation after the question, truncated to 50 chars. */
const MAX_NAME = 50;

function uniquePrompt(): string {
  // The mock echoes the prompt back, so a value unique to THIS run is what
  // proves the rendered answer was produced by this turn rather than replayed
  // from a previous one or served from a cache.
  const text = `autotest stream ${Date.now()}`;
  expect(text.length, 'the prompt must fit the 50-char conversation-name truncation').toBeLessThanOrEqual(MAX_NAME);
  return text;
}

test('the chat loop works end to end: send, stream, persist, reload', async ({ page }) => {
  // A whole agent turn — conversation create, admission, dispatch to the
  // worker, a model call and the stream back — does not fit Playwright's 30s
  // default. The individual waits below are each bounded well under this, so a
  // real hang still fails on the specific step rather than on the clock.
  test.setTimeout(180_000);

  const prompt = uniquePrompt();

  // ── The negative guard (#284): count every request the send path makes ──
  // The failure mode this journey exists to kill is a Send that renders
  // optimistically and never reaches the server. Counting real requests — not
  // spinners — is what makes that impossible to pass.
  const sendRequests: string[] = [];
  page.on('request', (request) => {
    const url = request.url();
    if (START_RE.test(url) || EVENTS_RE.test(url)) sendRequests.push(`${request.method()} ${url}`);
  });

  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  // Pick the seeded model, as #284 asks. It is not decoration: an ad-hoc turn
  // resolves against a `dummy` participant carrying the model, and the start
  // route reads `llm_settings.model_name`. With nothing selected the send is
  // rejected 400 before it reaches the worker.
  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
  await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await modelOption.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 15_000 });
  // The send control does not exist while the composer is empty; a stub that
  // always paints one fails here.
  await expect(page.getByTestId('chat-send-button')).toHaveCount(0);
  await input.fill(prompt);
  const sendButton = page.getByTestId('chat-send-button');
  await expect(sendButton).toBeEnabled({ timeout: 5_000 });

  // Both halves of the transport are awaited as RESPONSES, so a start that
  // 4xx's or a stream that never opens fails here rather than timing out later
  // against a blank message list.
  const created = page.waitForResponse(
    (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  const streamed = page.waitForResponse((r) => EVENTS_RE.test(r.url()), { timeout: 30_000 });

  // Sampling starts WITH the send, not after the response assertions below:
  // the reply streams in under a second, and a sampler that only begins once
  // those promises resolve routinely opens after the last chunk has landed —
  // reporting "nothing was painted incrementally" about a turn it never
  // watched. This runs concurrently and is awaited at the end.
  //
  // THE RECORDING LIVES IN THE PAGE, and that is the whole point. The first
  // version polled `locator.textContent()`, which AUTO-WAITS for the element:
  // its opening call blocked until the answer node attached, so the loop made
  // exactly ONE read for the whole turn and read the finished answer. Measured
  // in the trace of run 33810201650, where that first call started with the
  // click and returned 894 ms later — against a reply that spans 4 words x
  // MOCK_LLM_CHUNK_DELAY_MS=120, i.e. 480 ms. The assertion then reported
  // "the reply arrived in one frame" about a stream it never saw, and it did
  // so three times in a row while the app was streaming correctly. The comment
  // above was right about the hazard and the mechanism reintroduced it.
  //
  // A MutationObserver installed BEFORE the click cannot miss a paint,
  // whatever the round-trip latency: every distinct text the node holds is
  // recorded in page memory and drained below. `page.evaluate` does not
  // auto-wait for anything, so a drain that arrives late costs nothing.
  const answer = page.getByTestId('chat-message-list').getByTestId('application-answer').first();
  await page.evaluate(() => {
    const store = window as unknown as { __eliteaAnswerStates?: string[] };
    store.__eliteaAnswerStates = [];
    const read = (): string =>
      document
        .querySelector('[data-testid="chat-message-list"] [data-testid="application-answer"]')
        ?.textContent?.trim() ?? '';
    let last = '';
    const record = (): void => {
      const text = read();
      if (text !== last) {
        last = text;
        store.__eliteaAnswerStates?.push(text);
      }
    };
    new MutationObserver(record).observe(document.body, {
      subtree: true,
      childList: true,
      characterData: true,
    });
    record();
  });
  const partials: string[] = [];
  const sampler = (async () => {
    const deadline = Date.now() + 25_000;
    while (Date.now() < deadline) {
      const states = await page.evaluate(() => {
        const store = window as unknown as { __eliteaAnswerStates?: string[] };
        const seen = store.__eliteaAnswerStates ?? [];
        store.__eliteaAnswerStates = [];
        return seen;
      });
      for (const text of states) {
        if (text && !text.includes(prompt)) partials.push(text);
        if (text.includes(prompt)) return;
      }
      await page.waitForTimeout(50);
    }
  })();

  await sendButton.click();

  const createdResponse = await created;
  expect(createdResponse.status(), 'the send must create a real conversation').toBe(201);
  const projectId = CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
  expect(projectId, 'the conversation must belong to a project').not.toBe('');
  const conversation = (await createdResponse.json()) as { id?: string; name?: string };
  expect(conversation.id, 'the conversation must carry a server-assigned id').toMatch(/^\d+$/);
  expect(conversation.name, 'the conversation is named after the question that opened it').toBe(prompt);

  const startResponse = await started;
  expect(startResponse.status(), 'the agent-execution start must be admitted').toBe(200);
  const startBody = (await startResponse.json()) as { events_url?: string; task_id?: string };
  // `events_url` is the server's own absolute path. Its ABSENCE is the
  // documented signal that a backend serves no replay stream — on this stack it
  // must be present, or the UI silently fell back to a socket that cannot work.
  expect(startBody.events_url, 'the start response must carry the stream to read').toMatch(
    /^\/api\/v2\/executions\/\d+\/[0-9a-f]+\/events$/,
  );

  const streamResponse = await streamed;
  expect(streamResponse.status(), 'the browser must be able to READ the stream it is meant to render').toBe(200);
  expect(
    streamResponse.headers()['content-type'] ?? '',
    'the replay stream must be served as SSE',
  ).toContain('text/event-stream');

  // ── A token rendered BEFORE the turn finished ──
  // The stack runs the mock with MOCK_LLM_CHUNK_DELAY_MS set so the reply
  // spans several paints; the sampler above has been watching since the click.
  await expect(answer, 'an assistant turn must appear while the run is still open').toBeVisible({ timeout: 30_000 });
  await sampler;
  expect(
    partials.length,
    `no partial answer was ever painted — the reply arrived in one frame, not as a stream. Samples: ${JSON.stringify(partials.slice(0, 5))}`,
  ).toBeGreaterThan(0);
  const partial = partials[partials.length - 1] ?? '';

  // ── The completed answer ──
  // The mock echoes this run's unique prompt, so this cannot pass on a cached
  // or misrouted response. Asserted BEFORE any reload: with the socket disabled
  // there is no other producer, so the text on screen came off the SSE stream
  // through the reducer.
  // The mock ECHOES the prompt, which is what makes this assertion prove the
  // answer belongs to this run. A real model does not echo anything, so against
  // one the assertion has to fall back to "the turn produced text" — stated
  // here rather than silently weakened for everyone.
  if (!process.env['E2E_CHAT_MODEL']) {
    await expect(answer, 'the streamed answer must echo THIS run’s prompt').toContainText(prompt, { timeout: 60_000 });
  } else {
    await expect
      .poll(async () => ((await answer.textContent()) ?? '').trim().length, {
        timeout: 60_000,
        message: 'the turn produced no answer text',
      })
      .toBeGreaterThan(0);
  }
  expect(partial.length, 'the partial paint must be shorter than the finished answer').toBeLessThan(
    ((await answer.textContent()) ?? '').trim().length,
  );

  // Terminal state: the composer is usable again once the turn completes.
  await expect(page.getByTestId('chat-message-input'), 'the composer must be released when the turn ends').toBeEditable(
    { timeout: 60_000 },
  );

  // The negative guard, now that both halves have been observed.
  expect(sendRequests.length, `the send path made no request at all: ${JSON.stringify(sendRequests)}`).toBeGreaterThan(0);
  expect(sendRequests.some((entry) => entry.startsWith('POST'))).toBe(true);

  // ── Persistence ──
  // Waited for EXPLICITLY, because the stream is ahead of the store — the
  // measurement behind that, and the `IS_ERROR` discrimination, are in
  // `expectStoredAssistantAnswer`.
  //
  // `contains` is checked INSIDE the poll, and unconditionally — unlike the
  // on-screen assertion above, which relaxes for a real model. The mock echoes
  // this run's unique prompt, so the stored row must be THIS turn's answer and
  // not merely some non-empty text. The relaxation is not repeated here
  // because the reload assertion below filters by `prompt` too: an
  // `E2E_CHAT_MODEL` run fails on that either way, and pretending otherwise in
  // one of the three places would only make the file look real-model-ready
  // when it is not.
  //
  // Asserting the STORED text here also makes the reload assertion below a
  // statement about rendering rather than a second, weaker persistence check.
  await expectStoredAssistantAnswer(page, projectId, conversation.id ?? '', {
    timeout: 30_000,
    message: 'the assistant reply was streamed but never stored',
    contains: prompt,
  });

  // ── Persistence, through the UI ──
  // A FRESH load of the conversation's own URL re-reads everything from the
  // server, so this fails against a stack that streamed the answer and never
  // stored it. `/app/chat` itself is not reloaded, because the send path never
  // navigates there (`pages/chat/index.tsx` has no "conversation created"
  // callback) — reloading it would open an empty chat and assert nothing.
  await page.goto(`${BASE_URL}/app/chat/${conversation.id ?? ''}`, { waitUntil: 'domcontentloaded' });
  await page.waitForLoadState('networkidle');
  await expect(page.getByText('Something went wrong.')).toHaveCount(0);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  const list = page.getByTestId('chat-message-list');
  await expect(list.getByTestId('user-message').filter({ hasText: prompt }).first()).toBeVisible({ timeout: 30_000 });
  await expect(
    list.getByTestId('application-answer').filter({ hasText: prompt }).first(),
    'the assistant reply must survive a reload',
  ).toBeVisible({ timeout: 30_000 });
});
