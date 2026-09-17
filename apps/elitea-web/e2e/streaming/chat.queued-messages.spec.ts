/**
 * MESSAGE QUEUING (INTERJECTIONS) — what the composer does while a turn is
 * still running. Feature A17, folder `chat-interface/queued-text`.
 *
 * ── WHAT THIS PLATFORM ACTUALLY SUPPORTS, AND WHY THE QUEUE IS CLIENT-SIDE ──
 *
 * There is no mid-run interjection channel. `agent_start.go`'s
 * `resolveAfterCurrentResponseSettles` refuses a second start while the
 * conversation still holds a response row being written, waits out a three-
 * second settle budget and re-reads — so a second turn fired during the first
 * is a 422, not an interjection. The product behaviour the cases describe is
 * therefore built where it can be built: the composer stays live, what is typed
 * during a run goes into a visible "Waiting messages" queue, and the queue
 * drains FIFO through the ORDINARY send path once the run settles. Every
 * server-side rule (admission, budget, guardrails, participant resolution)
 * still applies to a queued message, because it is sent the same way a typed
 * one is.
 *
 * ── WHY THESE ASSERTIONS AND NOT THE OBVIOUS ONES ───────────────────────────
 *
 *  1. THE ORDER IS READ FROM THE STORE, not from the screen. The transcript on
 *     screen is rebuilt from optimistic rows plus stream frames, and an
 *     optimistic row appears the instant a send is attempted — so "three
 *     bubbles are visible" is true even for turns the server refused. The
 *     stored transcript (`sort_order=asc`) is the only place that says which
 *     turns the server actually admitted, in which order, and with which
 *     answers.
 *
 *  2. THE INTERJECTIONS MUST GET SEPARATE ANSWERS. The failure the cases name
 *     (ELITEA-2864/2866) is not "the messages were lost" but "they were merged
 *     into one response". A count of assistant rows cannot see that; each
 *     queued message's own text has to appear in its OWN answer, which the
 *     offline mock makes checkable because it echoes the prompt it was given.
 *
 *  3. THE RUN MUST STILL BE OPEN WHEN THE QUEUEING HAPPENS. Every assertion
 *     below is worthless if the turn finished first, so the queueing steps are
 *     gated on the STORE having reached `slow-005` — five chunks into an
 *     eighty-chunk scripted answer, unambiguously mid-stream — exactly as
 *     `chat.stop.spec.ts` gates its own click.
 *
 *  4. "RAPID CLICKS QUEUE ONE MESSAGE" (ELITEA-2871) is asserted through the
 *     mechanism rather than by clicking fast. `SendButton` renders no send
 *     control at all while the composer is empty, and the composer is cleared
 *     synchronously by the send itself — so after one click there is nothing
 *     left to click. Asserting a count of zero send controls states that,
 *     deterministically; three timed clicks would be measuring the scheduler.
 *
 * THE MODEL IS PINNED TO THE MOCK, deliberately, for the reason
 * `chat.stop.spec.ts` gives in full: this journey needs an answer whose length
 * and pace are known in advance, so that "the turn is still open" is a fact
 * rather than a hope.
 *
 * WHAT IS NOT COVERED HERE: an interjection ADDRESSED TO A NESTED AGENT
 * (ELITEA-2866). The runtime exposes no handle for a child run — the
 * delegation journeys (`chat.delegation.spec.ts`, `chat.nested-agent.spec.ts`)
 * show what exists, and it is parent-level admission only — so an interjection
 * typed while a nested agent is running is queued exactly like any other and
 * delivered as an ordinary follow-up turn once the PARENT settles. The half
 * that case can assert here (each interjection gets its own request/response
 * pair rather than being merged) is asserted by the first test.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { readStoredAssistantAnswer, readStoredTranscript } from '../fixtures/api';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const EVENTS_RE = /\/executions\/(\d+)\/[^/]+\/events/;

/** The mock model, and NOT `E2E_CHAT_MODEL` — see the header and `chat.stop.spec.ts`. */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'E2E-MOCK-MODEL';

/**
 * `deploy/mock-llm/server.py`'s `[[mock:slow]]` contract, restated (there is
 * nothing to import — it is a Python file in another service's tree). A stack
 * whose mock does not honour the marker streams the plain echo, which finishes
 * instantly, and the "the store reached slow-005" step below fails rather than
 * this journey passing on a turn it never interrupted.
 */
const SLOW_MARKER = '[[mock:slow]]';
const SLOW_PARTIAL_MARK = 'slow-005';

const input = (page: import('@playwright/test').Page) => page.getByTestId('chat-message-input');
const sendButton = (page: import('@playwright/test').Page) => page.getByTestId('chat-send-button');

/** Select the mock model. Without one the start route refuses 400 before the worker is reached. */
async function pickMockModel(page: import('@playwright/test').Page): Promise<void> {
  await page.getByTestId('model-selector-button').click();
  const option = page.getByRole('menuitem').filter({ hasText: MOCK_MODEL }).first();
  await expect(
    option,
    `the mock model ${MOCK_MODEL} must be offered — this journey cannot run against a real provider`,
  ).toBeVisible({ timeout: 20_000 });
  await option.click();
  await expect(page.getByTestId('model-selector-name')).toContainText(MOCK_MODEL, { timeout: 10_000 });
}

interface OpenedRun {
  readonly projectId: string;
  readonly conversationId: string;
}

/**
 * Open a conversation with a SLOW turn and return once the store proves the
 * answer is still arriving.
 *
 * Both halves matter. The navigation to `/chat/{id}` is waited for explicitly:
 * typing into the composer mid-navigation types into a component that is about
 * to be replaced, and the resulting "the send did nothing" failure would be a
 * statement about this test. And the store — not the screen — is what says the
 * turn is genuinely mid-stream.
 */
async function startSlowTurn(page: import('@playwright/test').Page, prompt: string): Promise<OpenedRun> {
  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
  await pickMockModel(page);

  await expect(input(page)).toBeEditable({ timeout: 15_000 });
  await input(page).fill(prompt);
  await expect(sendButton(page)).toBeEnabled({ timeout: 5_000 });

  const created = page.waitForResponse(
    (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  const streamed = page.waitForResponse((r) => EVENTS_RE.test(r.url()), { timeout: 30_000 });
  await sendButton(page).click();

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
    `the first turn was refused, so there is no run to interject into: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  expect((await streamed).status(), 'the browser must be able to read the stream').toBe(200);

  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 60_000 });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

  await expect
    .poll(async () => (await readStoredAssistantAnswer(page, projectId, conversationId)).content, {
      timeout: 60_000,
      message:
        `the answer never reached ${SLOW_PARTIAL_MARK} in the store — the mock did not honour the ` +
        'slow marker, so the turn was over before anything could be queued into it',
    })
    .toContain(SLOW_PARTIAL_MARK);

  return { projectId, conversationId };
}

/**
 * Which of `tokens` each stored USER row carries, oldest first.
 *
 * Tokens rather than whole prompts: the assertion being made is about ORDER
 * and COUNT, and comparing full strings would additionally pin whatever the
 * store does to the text on the way in — a detail this feature says nothing
 * about, and one that would turn an unrelated normalisation into a failure of
 * the queue. A row matching NO token is reported as `unmatched:…` rather than
 * dropped, so an extra turn nobody asked for is visible instead of invisible.
 */
async function questionOrder(
  page: import('@playwright/test').Page,
  projectId: string,
  conversationId: string,
  tokens: readonly string[],
): Promise<readonly string[]> {
  const rows = await readStoredTranscript(page, projectId, conversationId);
  return rows
    .filter((row) => row.role === 'user')
    .map((row) => tokens.find((token) => row.content.includes(token)) ?? `unmatched:${row.content.slice(0, 60)}`);
}

/* onetest: ELITEA-2864 — basic interjection queuing and sequential processing */
/* onetest: ELITEA-2865 — queued messages are processed in FIFO order */
/* onetest: ELITEA-2866 — PARTIAL: interjections raised during a nested-agent hop get their own request/response pairs; there is no runtime handle for addressing the CHILD run, so they are delivered once the parent settles */
/* onetest: ELITEA-2869 — the queue area shows the right count, order and status, and hides when empty */
/* onetest: ELITEA-2870 — a delivered interjection is labelled "Sent while running", and the label survives a reload */
/* onetest: ELITEA-2871 — the send control's states during an active run */
/* onetest: ELITEA-2872 — Enter queues, Shift+Enter opens a new line */
/* onetest: ELITEA-2873 — an empty or whitespace-only message cannot be queued */
/* onetest: ELITEA-2874 — rapid sequential interjections are all kept, in order, exactly once */
test('messages typed during a run queue, and are delivered in order once it settles', async ({ page }) => {
  // The slow turn alone is ~20s of open stream, and three follow-up turns run
  // after it; every step below carries its own tighter bound, so a real hang
  // fails on its own step rather than here.
  test.setTimeout(360_000);

  const stamp = Date.now();
  /** The token that identifies the OPENING question, distinct from every interjection's. */
  const opener = `queue-opener-${stamp}`;
  const prompt = `autotest ${opener} ${SLOW_MARKER}`;
  const queued = [
    `interject-one-${stamp}`,
    `interject-two-${stamp}`,
    `interject-three-${stamp}`,
  ];
  const order = [opener, ...queued];

  const { projectId, conversationId } = await startSlowTurn(page, prompt);

  // ── The composer is LIVE while the run is open ──────────────────────────
  // This is the whole enabling condition. Before A17 the composer was disabled
  // for the length of a turn, so there was nothing to interject with.
  await expect(
    input(page),
    'the composer must stay editable while a turn runs — that is what makes an interjection possible',
  ).toBeEditable({ timeout: 10_000 });
  await expect(
    page.getByRole('button', { name: 'Stop generating' }),
    'Stop must still be offered: queueing does not replace cancelling',
  ).toBeVisible({ timeout: 10_000 });
  // ELITEA-2871, first row: an empty composer offers no send control at all.
  await expect(sendButton(page)).toHaveCount(0);
  await expect(page.getByTestId('chat-queued-messages')).toHaveCount(0);

  // ── ELITEA-2873: whitespace is not a message ────────────────────────────
  // Pressed rather than clicked, for the reason `chat.composer.spec.ts` uses
  // the same key for the same case: a whitespace composer DOES paint a send
  // control (`isEmpty` is `!question`, and spaces are truthy), so the refusal
  // being asserted is the send path's own `question.trim()` guard, not a
  // missing button.
  await input(page).fill('   \t  ');
  await input(page).press('Enter');
  await expect(
    page.getByTestId('chat-queued-messages'),
    'a whitespace-only message must not reach the waiting list',
  ).toHaveCount(0);
  await input(page).fill('');

  // ── ELITEA-2872: Shift+Enter opens a line, Enter queues ─────────────────
  await input(page).fill('a draft line');
  await input(page).press('Shift+Enter');
  await expect(
    page.getByTestId('chat-queued-messages'),
    'Shift+Enter must not send',
  ).toHaveCount(0);
  await input(page).fill('');

  await input(page).fill(queued[0] as string);
  await input(page).press('Enter');
  await expect(page.getByTestId('chat-queued-count')).toHaveText(/Waiting messages · 1/, { timeout: 10_000 });
  // ELITEA-2871: the composer clears, and with it the send control disappears —
  // which is also why a second click cannot queue a duplicate (see header, 4).
  await expect(input(page)).toHaveValue('');
  await expect(sendButton(page)).toHaveCount(0);

  // The second goes by CLICK rather than Enter, so both entry points are
  // exercised against a live run.
  await input(page).fill(queued[1] as string);
  await expect(sendButton(page)).toBeEnabled({ timeout: 5_000 });
  await sendButton(page).click();
  await expect(page.getByTestId('chat-queued-count')).toHaveText(/Waiting messages · 2/, { timeout: 10_000 });

  await input(page).fill(queued[2] as string);
  await input(page).press('Enter');
  await expect(page.getByTestId('chat-queued-count')).toHaveText(/Waiting messages · 3/, { timeout: 10_000 });

  // ── ELITEA-2869: the list itself ────────────────────────────────────────
  const rows = page.getByTestId('chat-queued-item');
  await expect(rows).toHaveCount(3);
  // Chronological, first at top: the order they will be delivered in.
  await expect(rows.nth(0)).toContainText(queued[0] as string);
  await expect(rows.nth(1)).toContainText(queued[1] as string);
  await expect(rows.nth(2)).toContainText(queued[2] as string);
  await expect(page.getByTestId('chat-queued-status')).toHaveCount(3);
  // Still exactly three, and still nothing sent: a queue that leaked a message
  // onto the wire here would be refused by the settle gate, not interjected.
  expect(
    await questionOrder(page, projectId, conversationId, order),
    'nothing may reach the store while the queue is waiting',
  ).toEqual([opener]);

  // ── The queue drains ────────────────────────────────────────────────────
  // The strip disappearing is the client saying "all delivered"; the store
  // below is what says they were ADMITTED, in order, each with its own answer.
  await expect(page.getByTestId('chat-queued-messages'), 'the queue area must hide once it is empty').toHaveCount(0, {
    timeout: 180_000,
  });

  await expect
    .poll(async () => questionOrder(page, projectId, conversationId, order), {
      timeout: 180_000,
      message: 'the queued messages were never delivered, or not in the order they were typed',
    })
    .toEqual(order);

  // ── ELITEA-2864/2866: each interjection got its OWN answer ──────────────
  await expect
    .poll(
      async () => {
        const transcript = await readStoredTranscript(page, projectId, conversationId);
        return queued.filter((text) =>
          transcript.some((row) => row.role === 'assistant' && !row.isError && row.content.includes(text)),
        ).length;
      },
      {
        timeout: 180_000,
        message:
          'at least one interjection has no answer of its own — the failure this case names is three ' +
          'questions collapsing into a single merged response, which a row COUNT cannot see',
      },
    )
    .toBe(queued.length);

  // ── ELITEA-2870: the label, and its survival ────────────────────────────
  await expect(page.getByTestId('chat-message-interjected')).toHaveCount(3, { timeout: 30_000 });
  await page.reload();
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
  await expect(
    page.getByTestId('chat-message-interjected'),
    'the "Sent while running" label must survive a reload — it is keyed on the question id the server persisted',
  ).toHaveCount(3, { timeout: 60_000 });

  // Terminal state: the composer is free and the queue is gone.
  await expect(input(page)).toBeEditable({ timeout: 30_000 });
  await expect(page.getByTestId('chat-queued-messages')).toHaveCount(0);
});

/* onetest: ELITEA-2868 — queued messages are preserved when the run is stopped, and delivered afterwards */
test('Stop does not discard the waiting messages', async ({ page }) => {
  test.setTimeout(300_000);

  const stamp = Date.now();
  const opener = `stop-opener-${stamp}`;
  const prompt = `autotest ${opener} ${SLOW_MARKER}`;
  const queued = [`stop-first-${stamp}`, `stop-second-${stamp}`];
  const order = [opener, ...queued];

  const { projectId, conversationId } = await startSlowTurn(page, prompt);

  for (const [index, text] of queued.entries()) {
    await input(page).fill(text);
    await input(page).press('Enter');
    await expect(page.getByTestId('chat-queued-count')).toHaveText(
      new RegExp(`Waiting messages · ${String(index + 1)}`),
      { timeout: 10_000 },
    );
  }

  // Stop is not a special case for the queue, and that is the design: it
  // settles the run exactly as a completed turn does, so the waiting messages
  // become ordinary turns instead of being thrown away with the run.
  await page.getByRole('button', { name: 'Stop generating' }).click();

  await expect
    .poll(async () => questionOrder(page, projectId, conversationId, order), {
      timeout: 180_000,
      message: 'the queue was discarded when the run was stopped, or delivered out of order',
    })
    .toEqual(order);

  await expect(page.getByTestId('chat-queued-messages'), 'the queue area must hide once it is empty').toHaveCount(0, {
    timeout: 60_000,
  });
  // The chat continues normally afterwards — the case's last step.
  await expect(input(page)).toBeEditable({ timeout: 60_000 });
});

/* onetest: ELITEA-2867 — attachments are refused for the whole of an active run, and restored when it ends */
test('attachments are blocked while a turn is running', async ({ page }) => {
  test.setTimeout(300_000);

  const stamp = Date.now();
  const opener = `attach-opener-${stamp}`;
  const prompt = `autotest ${opener} ${SLOW_MARKER}`;
  const { projectId, conversationId } = await startSlowTurn(page, prompt);

  // ── The button ──────────────────────────────────────────────────────────
  const plus = page.getByTestId('plus-menu-button');
  await expect(plus).toBeEnabled({ timeout: 20_000 });
  await plus.click();
  const attachRow = page.getByTestId('plus-menu-attachments');
  await expect(attachRow).toBeVisible({ timeout: 10_000 });
  await expect(
    attachRow,
    'the attach row must be inert while a turn is running',
  ).toHaveAttribute('aria-disabled', 'true');
  // Closed through the control's OWN toggle, not with `Escape`.
  //
  // `PlusChatButton` renders the menu in a `Popper` closed by `toggleMenu` or
  // by its `ClickAwayListener`; nothing in it listens for `Escape`. The first
  // version of this step pressed `Escape`, which left the menu OPEN — so the
  // `plus.click()` at the END of this test CLOSED it instead of opening it,
  // and the restored attach row read as an absent one. Measured twice,
  // deterministically: the failure looked exactly like "attaching never came
  // back". (That `Escape` does not dismiss this menu is a real keyboard-
  // dismissal nit, but it is not ELITEA-2867's subject and is not asserted
  // here.)
  await plus.click();
  await expect(attachRow, 'the menu must close again before the drop test').toHaveCount(0, { timeout: 10_000 });

  // ── Drag-and-drop ───────────────────────────────────────────────────────
  // The interesting half: the drop/paste bridge does NOT go through the button
  // the reader can see — it calls `attachmentButtonRef.current.onDrop(...)`
  // directly. Gating only the visible control would leave this path open, and
  // the file would be attached to a turn already in flight with nothing on
  // screen saying so.
  const dataTransfer = await page.evaluateHandle(() => {
    const transfer = new DataTransfer();
    transfer.items.add(new File(['blocked'], 'blocked-mid-run.txt', { type: 'text/plain' }));
    return transfer;
  });
  await page.dispatchEvent('[data-testid="chat-input"]', 'drop', { dataTransfer });
  await expect(
    page.getByTestId('chat-attachment-chip-0'),
    'a file dropped during a run must not be staged',
  ).toHaveCount(0);

  // ── A plain-text interjection still queues ──────────────────────────────
  const text = `attach-blocked-${stamp}`;
  await input(page).fill(text);
  await input(page).press('Enter');
  await expect(page.getByTestId('chat-queued-count')).toHaveText(/Waiting messages · 1/, { timeout: 10_000 });

  // ── And the capability comes back when the run ends ─────────────────────
  await expect
    .poll(async () => questionOrder(page, projectId, conversationId, [opener, text]), {
      timeout: 180_000,
      message: 'the plain-text interjection was never delivered',
    })
    .toEqual([opener, text]);
  await expect(input(page)).toBeEditable({ timeout: 60_000 });

  await plus.click();
  await expect(
    page.getByTestId('plus-menu-attachments'),
    'attaching must work again once no run is active',
  ).toHaveAttribute('aria-disabled', 'false', { timeout: 20_000 });
});
