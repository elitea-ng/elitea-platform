/**
 * The TRACE-STEP migration, from the outside: tool-call detail moved off
 * `chat_message_group.meta` into its own `chat_message_trace_step` table, and
 * the `trace-steps-api` onetest folder is the set of properties that move has
 * to preserve.
 *
 * `/elitea_core/message_traces/…` (the listing) and `/elitea_core/message_trace/…`
 * (one step's heavy fields) are the routes it created;
 * `internal/api/v2/messagetraces/handler.go` is both of them.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THESE CASES NEED THE STREAM LANE AND REAL TOOL CALLS
 * ─────────────────────────────────────────────────────────────────────────────
 * Every one of them is a statement about rows that only a real turn writes.
 * There is no seeding route for `chat_message_trace_step` — it is written by
 * the execution-result projection as the worker reports steps — so a spec that
 * wanted N tool-call rows has to make an agent actually call a tool N times.
 * The mock's `[[mock:call_tool …]]` marker is how that is scripted, and since
 * the marker scan became plural (see `chat.tail-sensitive-tools.spec.ts`'s
 * header) one turn can carry as many calls as the case needs — which is what
 * makes the pagination case reachable at all.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT A "PIN" IS HERE, AND WHY THE COUNTS ARE COMPARED THE WAY THEY ARE
 * ─────────────────────────────────────────────────────────────────────────────
 * The screen renders one `chat-tool-action` row per tool step (`ActionView`),
 * INSIDE the turn's "Thought for …" panel — which is forced open while the
 * turn streams and closed, with its children unmounted, the moment it settles.
 * So a bare `count()` on a settled turn answers 0 whether the rows exist or
 * not, and every count below goes through `countToolPins`, which opens the
 * panels the way a reader does first. `chat.toolkit.spec.ts` drives the same
 * affordance for the same reason.
 *
 * The case asks for the UI count and the API count to agree. Comparing them
 * requires the SAME window on both sides: the API listing is conversation
 * scoped and the rows are rendered per message, so every comparison below is
 * made on a conversation holding exactly one tool-calling turn.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE TOOL HOST IS NOT REACHABLE FROM THE WORKER ON THIS STACK
 * ─────────────────────────────────────────────────────────────────────────────
 * `MockToolSpec.reachable` is false: the native worker's OpenAPI client is
 * https-only and the mock's certificate does not chain to a public root, so a
 * DISPATCHED call comes back as `tool.unavailable`. That changes nothing for
 * this folder — a step that ran and failed is still a step, still gets its own
 * row, and still renders its own pin — and the assertions below are about
 * counts, identity and paging rather than about a tool's output. Where a step's
 * OUTCOME matters it is read as `is_error`, which is populated either way.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE LEG CONTRACT: BOTH (rust and python)
 * ─────────────────────────────────────────────────────────────────────────────
 * The table, the projection and both routes are elitea-main's, upstream of
 * either runtime; what the worker contributes is the step reports, which both
 * emit. Nothing below reads a field only one of them fills.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  EMPTY_TOOLKIT_GUARDRAILS,
  MOCK_CALL_TOOL_SENTINEL,
  MOCK_TOOL_READ_OPERATION,
  callToolPrompt,
  callToolWithArgumentsPrompt,
  createMockToolAgent,
  expectStoredAssistantAnswer,
  fillComposer,
  readStoredHitlInterrupt,
  setToolkitGuardrails,
  type MockToolAgentFixture,
} from '../fixtures/api';

const API = `${BASE_URL}/api/v2`;

/**
 * Which runtime is answering the turns — `scripts/chat-stream-e2e.sh` is the
 * one place that knows, and it exports this. Read by the three MULTI-CALL
 * journeys below, which are native-only for a HARNESS reason rather than a
 * product one: see `multiCallPrompt`.
 */
const IS_NATIVE_RUNTIME = (process.env['E2E_WORKER'] ?? 'rust') === 'rust';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
/** The REST resume — `POST …/continue_predict/prompt_lib/{project}/{conversationUuid}`. */
const CONTINUE_RE = /\/elitea_core\/continue_predict\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

interface TraceStepRow {
  readonly id: string;
  readonly message_group_id: string;
  readonly kind: string;
  readonly tool_name: string | null;
  readonly is_error: boolean | null;
  readonly finished_at: string | null;
}

/** One page of the conversation's trace steps, as the listing route serves it. */
async function listTraceSteps(
  page: Page,
  projectId: string,
  conversationId: string,
  query = '',
): Promise<{ readonly rows: readonly TraceStepRow[]; readonly total: number | null }> {
  const url =
    `${API}/elitea_core/message_traces/prompt_lib/${projectId}/${conversationId}` +
    `?kind=tool_call${query === '' ? '' : `&${query}`}`;
  const response = await page.request.get(url);
  expect(
    response.status(),
    `the trace-step listing must answer: ${(await response.text()).slice(0, 300)}`,
  ).toBe(200);
  const body = (await response.json()) as { rows?: readonly TraceStepRow[]; total?: number | null };
  return { rows: body.rows ?? [], total: body.total ?? null };
}

/**
 * A prompt naming `count` scripted calls, each with DISTINCT arguments.
 *
 * Distinct, because identical calls collapse. The runtime fingerprints a tool
 * call by (name, arguments) — `tool_call_fingerprint`, `graph/llm.rs` — and
 * five copies of `mock_tool_status {}` in one message were measured to produce
 * TWO trace steps, not five: one per distinct fingerprint. That is a sensible
 * runtime property and a fatal one for a spec that wants N steps, so each
 * marker here carries its own `response_search` value. It is a real property
 * of the operation's generated schema (`families/openapi/spec.rs`), so the
 * call stays valid; nothing downstream depends on its value.
 *
 * ── WHY EVERY JOURNEY THAT USES THIS IS NATIVE-ONLY ──────────────────────
 *
 * A HARNESS limit, measured, not a product one. The tool's own host is
 * unreachable from the worker on this stack (see the file header). The native
 * worker's https-only client refuses it in milliseconds, so five calls cost
 * nothing; the SDK worker instead DIALS it and waits out a connect/TLS
 * timeout per call. Measured on a python-worker stack: the five-call turn in
 * `a streamed multi-tool turn…` took 17.1 minutes and still had not settled,
 * and the ten-call pagination turn timed out at 5.5 minutes with an empty
 * assistant row — and the test that ran AFTER them failed as collateral (it
 * passes on its own there, re-measured). Nothing about the trace-step
 * contract differs between the legs; what differs is how long an unreachable
 * tool takes to fail. So the multi-call journeys pin the native leg and say
 * why, and the two single-call journeys in this file run on BOTH.
 */
function multiCallPrompt(count: number, tail: string): string {
  const stamp = String(Date.now() % 1_000_000);
  const markers = Array.from({ length: count }, (_, index) =>
    callToolWithArgumentsPrompt(MOCK_TOOL_READ_OPERATION, { response_search: `autotest-${stamp}-${String(index)}` }, ''),
  );
  return `${markers.join(' ')} ${tail}`;
}

/**
 * Open every "Thought for …" panel in the transcript and hand back the tool
 * rows inside them.
 *
 * The rows are NOT top-level: the chat parity work put them inside the turn's
 * thinking accordion, which is forced open while the turn streams and closed
 * the moment it settles — and its children are unmounted while it is closed,
 * so a bare `count()` on a settled turn answers 0 whether the rows exist or
 * not. `chat.toolkit.spec.ts` drives the same affordance for the same reason;
 * this is that step, generalised to counting rather than to one named row.
 *
 * Clicking is polled rather than done once: `isStreaming` can still be true
 * for a frame after the stored answer lands, and a click on an already-open
 * panel would CLOSE it.
 */
async function countToolPins(page: Page, expected: number, where: string): Promise<number> {
  const summaries = page.getByTestId('chat-answer-thought-accordion').getByRole('button', { name: /Thought for/ });
  await expect(
    summaries.last(),
    `${where}: the turn ran tools but rendered no thinking panel to hold their rows`,
  ).toBeVisible({ timeout: 60_000 });
  let seen = 0;
  await expect
    .poll(
      async () => {
        for (const summary of await summaries.all()) {
          try {
            if ((await summary.getAttribute('aria-expanded')) !== 'true') {
              await summary.click({ timeout: 5_000 });
            }
          } catch {
            // A re-render between the read and the click detaches the node.
            // The next tick addresses its replacement — a retry, not a result.
          }
        }
        seen = await page.getByTestId('chat-tool-action').count();
        return seen;
      },
      {
        timeout: 60_000,
        message: `${where}: the transcript never showed ${String(expected)} tool-call row(s)`,
      },
    )
    .toBe(expected);
  return seen;
}

/** Send one prompt through the composer and require the start to be ADMITTED. */
async function sendTurn(page: Page, prompt: string): Promise<void> {
  const send = await fillComposer(page, prompt);
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  await send.click();
  const response = await started;
  expect(response.status(), `the turn was refused: ${(await response.text()).slice(0, 300)}`).toBe(200);
}

/** Wait until the conversation's tool-step listing holds at least `minimum` rows. */
async function awaitTraceSteps(
  page: Page,
  projectId: string,
  conversationId: string,
  minimum: number,
): Promise<readonly TraceStepRow[]> {
  let rows: readonly TraceStepRow[] = [];
  await expect
    .poll(
      async () => {
        rows = (await listTraceSteps(page, projectId, conversationId, 'limit=100')).rows;
        return rows.length;
      },
      {
        timeout: 180_000,
        message: `the turn never wrote ${String(minimum)} tool-call trace step(s)`,
      },
    )
    .toBeGreaterThanOrEqual(minimum);
  return rows;
}

test.afterEach(async () => {
  // Restored on failure too: the sensitive-tool journey below writes the
  // platform-wide policy, and a leaked entry turns every later spec's tool
  // calls into pauses nothing answers.
  await setToolkitGuardrails(EMPTY_TOOLKIT_GUARDRAILS);
});

/* onetest: ELITEA-2583 — GET conversation details no longer carries `tool_calls` / `thinking_steps`
 * in `message_group.meta` for a conversation created after the migration, while the tool-call pins
 * are still served (from the trace-steps endpoint) and still rendered. */
test('conversation details carry no tool_calls or thinking_steps, and the pins still render', async ({
  page,
}) => {
  test.setTimeout(300_000);

  let fixture: MockToolAgentFixture | undefined;
  try {
    fixture = await createMockToolAgent(page, 'meta');
    await sendTurn(page, callToolPrompt(MOCK_TOOL_READ_OPERATION, 'read the status'));
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the tool-calling turn never produced an answer quoting its result',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });

    // The exact read the case captures in DevTools: the conversation detail
    // route, with `messages_limit` — the handler embeds `message_groups` only
    // then, so omitting it would return a well-formed 200 carrying no groups
    // and every assertion below would pass vacuously.
    const detail = await page.request.get(
      `${API}/elitea_core/conversation/prompt_lib/${fixture.projectId}/${fixture.conversationId}` +
        `?messages_limit=50&sort_order=asc`,
    );
    expect(detail.status(), 'the conversation detail must be readable').toBe(200);
    const groups =
      ((await detail.json()) as { message_groups?: readonly { meta?: Record<string, unknown> }[] })
        .message_groups ?? [];
    expect(
      groups.length,
      'the detail read returned no message groups at all — the assertions below would be vacuous',
    ).toBeGreaterThan(0);
    for (const group of groups) {
      expect(
        Object.keys(group.meta ?? {}),
        'a conversation created after the migration must not carry tool_calls in message_group.meta — ' +
          'that is the heavy key the trace-step table exists to take out of this response',
      ).not.toContain('tool_calls');
      expect(
        Object.keys(group.meta ?? {}),
        'a conversation created after the migration must not carry thinking_steps in message_group.meta',
      ).not.toContain('thinking_steps');
    }

    // AND the detail is still served, from the route that now owns it. Without
    // this the assertions above are also satisfied by a turn that recorded no
    // tool call at all.
    const steps = await awaitTraceSteps(page, fixture.projectId, fixture.conversationId, 1);
    expect(
      steps.map((step) => step.tool_name),
      'the tool call must be readable from the trace-steps route',
    ).toContain(MOCK_TOOL_READ_OPERATION);

    // And it still RENDERS. Without this the assertions above are also
    // satisfied by a migration that took the keys out and forgot to put the
    // pins back. The RELOADED half of the same claim — the case's own "verify
    // tool calls are still visible in the UI (loaded from /message_traces/)"
    // read against a fresh page — is a product gap and lives in its own
    // fail-marked test at the end of this file.
    expect(
      await countToolPins(page, steps.length, 'the live turn'),
      'the tool-call pin must still render after the heavy keys left the conversation response',
    ).toBe(steps.length);
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-2584 — the trace-steps listing pages with `limit` and `offset`: successive pages
 * are disjoint, an offset past the end is an empty page rather than an error, and `limit=0` is
 * clamped to the default page rather than answering nothing. */
test('the trace-steps listing pages with limit and offset', async ({ page }) => {
  test.setTimeout(420_000);
  test.skip(
    !IS_NATIVE_RUNTIME,
    'multi-call turns are native-only HERE for a harness reason, not a product one: the SDK worker ' +
      'waits out a connect timeout per call against the unreachable tool host — see `multiCallPrompt`.',
  );

  let fixture: MockToolAgentFixture | undefined;
  try {
    fixture = await createMockToolAgent(page, 'paging');

    // TEN calls, in two turns of five. The case asks for "10+ tool calls";
    // two turns rather than one because paging must cross a message-group
    // boundary — a pager that silently scoped itself to one group would
    // otherwise page correctly over the only group there is.
    await sendTurn(page, multiCallPrompt(5, 'call five tools'));
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the first five-call turn never settled',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });
    await sendTurn(page, multiCallPrompt(5, 'call five more tools'));
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the second five-call turn never settled',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });

    const all = await awaitTraceSteps(page, fixture.projectId, fixture.conversationId, 10);
    expect(
      new Set(all.map((step) => step.message_group_id)).size,
      'the ten steps must span two message groups, or the paging never crosses a group boundary',
    ).toBeGreaterThan(1);

    const pages = await Promise.all(
      [0, 3, 6].map(async (offset) =>
        (await listTraceSteps(page, fixture?.projectId ?? '', fixture?.conversationId ?? '', `limit=3&offset=${String(offset)}`))
          .rows,
      ),
    );
    for (const [index, rows] of pages.entries()) {
      expect(rows.length, `page ${String(index)} must hold exactly the requested three rows`).toBe(3);
    }
    const paged = pages.flat().map((step) => step.id);
    expect(
      new Set(paged).size,
      'the three pages overlapped — a pager whose order is not total repeats rows and skips others',
    ).toBe(paged.length);
    // And they are a prefix of the unpaged listing, in the same order: paging
    // that returned nine DIFFERENT rows in a different order would satisfy the
    // uniqueness check above on its own.
    expect(paged, 'the pages must reassemble the listing’s own order').toEqual(
      all.slice(0, 9).map((step) => step.id),
    );

    const beyond = await listTraceSteps(
      page,
      fixture.projectId,
      fixture.conversationId,
      `limit=3&offset=${String(all.length + 50)}`,
    );
    expect(beyond.rows, 'an offset past the end must be an empty page, not an error').toEqual([]);

    // `limit=0`: documented behaviour rather than an assumption. The handler
    // clamps a non-positive limit to the DEFAULT page size (`parseListQuery` —
    // "a bad limit falls back to a NARROWER answer"), so it answers rows, not
    // an empty page and not a 400.
    const zero = await listTraceSteps(page, fixture.projectId, fixture.conversationId, 'limit=0');
    expect(
      zero.rows.length,
      'limit=0 is clamped to the default page rather than refused — a caller that sent it by ' +
        'accident gets a page, not an error and not silence',
    ).toBeGreaterThan(0);

    // `include_total` is what a client pages ON, and it must count the same
    // rows the listing pages over.
    const counted = await listTraceSteps(page, fixture.projectId, fixture.conversationId, 'limit=1&include_total=true');
    expect(counted.total, 'include_total must report the whole listing, not the page').toBe(all.length);
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-2591, ELITEA-2593 — a streamed turn with several tool calls renders exactly one
 * pin per execution, the screen's count matches the API's, and both survive a reload. PARTIAL: the
 * case's "pins appear as tools START, not after" step is not asserted — see the comment inside. */
test('a streamed multi-tool turn renders one pin per execution, and the count survives a reload', async ({
  page,
}) => {
  test.setTimeout(420_000);
  test.skip(
    !IS_NATIVE_RUNTIME,
    'multi-call turns are native-only HERE for a harness reason, not a product one: the SDK worker ' +
      'waits out a connect timeout per call against the unreachable tool host — see `multiCallPrompt`.',
  );

  let fixture: MockToolAgentFixture | undefined;
  try {
    fixture = await createMockToolAgent(page, 'pins');

    // FIVE calls, the case's own "5+ tool executions".
    await sendTurn(page, multiCallPrompt(5, 'call five tools'));
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the five-call turn never settled',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });

    // WHAT IS NOT ASSERTED, AND WHY. The case also asks that a pin appear as
    // each tool STARTS rather than after it finishes. On this stack the tool
    // host is unreachable from the worker, so every call fails in the
    // transport within milliseconds: there is no observable window between
    // "started" and "completed" to sample, and a poll that happened to catch
    // one would be a timing accident rather than a contract. Asserting it
    // needs a tool that can be made slow on purpose, which the mock's `/tool`
    // API cannot yet be (`[[mock:slow]]` slows the MODEL, not the tool).
    const steps = await awaitTraceSteps(page, fixture.projectId, fixture.conversationId, 5);
    expect(steps.length, 'five scripted calls must write five tool-call steps — no more, no fewer').toBe(5);
    expect(
      new Set(steps.map((step) => step.id)).size,
      'two steps shared an id — the listing is duplicating rows',
    ).toBe(steps.length);
    expect(
      steps.filter((step) => step.finished_at === null),
      'every step of a settled turn must have finished — a pin stuck at "running" is how a lost ' +
        'completion report surfaces',
    ).toEqual([]);

    // THE UI COUNT, against the API count, on a conversation holding exactly
    // one tool-calling turn.
    expect(
      await countToolPins(page, steps.length, 'the live turn'),
      'the screen never rendered a pin for every tool step the API reports',
    ).toBe(steps.length);

    // AND AFTER A RELOAD, on the API side — the steps themselves are durable
    // even though the pins that show them are not (the rendered half is the
    // fail-marked test at the end of this file).
    await page.reload();
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 45_000 });
    expect(
      (await listTraceSteps(page, fixture.projectId, fixture.conversationId, 'limit=100')).rows.length,
      'the reload changed how many steps the API reports',
    ).toBe(steps.length);
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-2592 — a HITL (sensitive-tool) pause and its resume work with trace steps: the
 * paused call is already a step, the decision completes it, and leaving the conversation and
 * coming back shows the same steps. */
test('a paused tool call is a trace step, and survives the resume and a return to the conversation', async ({
  page,
}) => {
  test.setTimeout(420_000);

  let fixture: MockToolAgentFixture | undefined;
  try {
    fixture = await createMockToolAgent(page, 'hitl');
    await setToolkitGuardrails({
      ...EMPTY_TOOLKIT_GUARDRAILS,
      sensitive_tools: { openapi: [MOCK_TOOL_READ_OPERATION] },
    });

    await sendTurn(page, callToolPrompt(MOCK_TOOL_READ_OPERATION, 'read the status'));
    const card = page.getByTestId('chat-hitl-actions').last();
    await expect(card, 'the sensitive call never raised an authorization card').toBeVisible({
      timeout: 180_000,
    });
    const interrupt = await readStoredHitlInterrupt(page, fixture.projectId, fixture.conversationId);
    expect(interrupt.tool_name, 'the pause must name the operation the model asked for').toBe(
      MOCK_TOOL_READ_OPERATION,
    );

    // The paused call is ALREADY a step. That is the regression the case
    // names: a pin that only appears once a tool has run leaves a paused turn
    // showing nothing at all, and the user is asked to authorize a call that
    // is invisible in the transcript beside the dialog.
    const paused = await awaitTraceSteps(page, fixture.projectId, fixture.conversationId, 1);
    expect(
      paused.map((step) => step.tool_name),
      'the paused call must already have its own trace step',
    ).toContain(MOCK_TOOL_READ_OPERATION);
    const pausedIds = paused.map((step) => step.id);

    const resumed = page.waitForResponse((r) => CONTINUE_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    await card.getByRole('button', { name: 'Approve', exact: true }).click();
    expect(
      (await resumed).status(),
      'the approval was refused',
    ).toBe(200);
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the approval was accepted but the turn never produced its continuation reply',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });

    const afterResume = await awaitTraceSteps(page, fixture.projectId, fixture.conversationId, 1);
    expect(
      afterResume.filter((step) => step.finished_at === null),
      'the authorized call must reach a finished state — the resume must complete its step, not ' +
        'leave it open and write a second one',
    ).toEqual([]);
    expect(
      pausedIds.every((id) => afterResume.some((step) => step.id === id)),
      'the step the pause was taken against disappeared on resume — the decision was recorded ' +
        'against a row that is no longer the one the transcript shows',
    ).toBe(true);

    // ── Leave, and come back ────────────────────────────────────────────────
    //
    // The case's "Continue": the conversation is navigated away from and
    // reopened, and the history must be whole.
    await page.goto(`${BASE_URL}/app/chat`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 45_000 });
    await page.goto(`${BASE_URL}/app/chat/${fixture.conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 45_000 });

    const afterReturn = (await listTraceSteps(page, fixture.projectId, fixture.conversationId, 'limit=100')).rows;
    expect(
      afterReturn.map((step) => step.id).sort(),
      'the trace steps changed across leaving and returning to the conversation',
    ).toEqual([...afterResume].map((step) => step.id).sort());
    // The RENDERED half of "all tool calls from before are still visible" is
    // the same product gap the fail-marked test at the end of this file owns:
    // a reopened conversation renders no thinking panel and therefore no pins.
    // Asserted there once rather than failing every test that navigates.

    // And the conversation still takes a turn afterwards — a "Continue" that
    // renders history but cannot be used is not a continuation.
    const followUp = `${AUTOTEST_PREFIX}after return ${String(Date.now() % 1_000_000)}`;
    await sendTurn(page, followUp);
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the reopened conversation could not take a further turn',
      contains: followUp,
    });
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-2593 (the refresh half), ELITEA-2583 (its "tool calls are still visible in the UI"
 * half), ELITEA-2592 (its "all tool calls from before are still visible" half) — the tool-call pins
 * a settled turn showed are still there after the page is reloaded. FAIL-MARKED: they are not. */
test('the tool-call pins a turn showed are still rendered after a reload', async ({ page }) => {
  test.setTimeout(300_000);
  test.skip(
    !IS_NATIVE_RUNTIME,
    'multi-call turns are native-only HERE for a harness reason, not a product one: the SDK worker ' +
      'waits out a connect timeout per call against the unreachable tool host — see `multiCallPrompt`.',
  );

  // MEASURED on `STANDALONE_WORKER=rust`: a settled tool-calling turn renders
  // its `Thought for …` panel and one `chat-tool-action` row per step on the
  // LIVE page (asserted, passing, in the two tests above). After `page.reload()`
  // — or after navigating away and reopening the conversation — the panel is
  // not rendered at all, so there is no affordance that could reveal the rows
  // and `chat-tool-action` has a count of zero. The steps themselves are
  // durable: `/elitea_core/message_traces/…` answers the same rows before and
  // after, which is asserted in the test above. So the data survived the
  // migration and the RENDER of it did not — a reader who reopens a
  // conversation cannot see which tools ran in it.
  test.fail(
    true,
    'ELITEA-2593 (#951): product gap — tool-call pins do not survive a reload: a reopened conversation ' +
      'renders no thinking panel and no `chat-tool-action` rows, though the trace-step API still ' +
      'answers the same steps. See S/tail/defects.md.',
  );

  let fixture: MockToolAgentFixture | undefined;
  try {
    fixture = await createMockToolAgent(page, 'reloadpins');
    await sendTurn(page, multiCallPrompt(3, 'call three tools'));
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the three-call turn never settled',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });

    const steps = await awaitTraceSteps(page, fixture.projectId, fixture.conversationId, 3);
    // The LIVE count first, so a failure below can only be about the reload —
    // not about the turn having rendered nothing in the first place.
    expect(await countToolPins(page, steps.length, 'the live turn'), 'the live turn must render its pins').toBe(
      steps.length,
    );

    await page.reload();
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 45_000 });
    expect(
      (await listTraceSteps(page, fixture.projectId, fixture.conversationId, 'limit=100')).rows.length,
      'the steps themselves must survive the reload — otherwise this is a storage defect, not a render one',
    ).toBe(steps.length);
    expect(
      await countToolPins(page, steps.length, 'after the reload'),
      'the reloaded conversation shows none of the tool-call pins it showed a moment ago',
    ).toBe(steps.length);
  } finally {
    await fixture?.dispose();
  }
});
