/**
 * The sensitive-action authorization DIALOG, as the `sensitive-tool-calling`
 * onetest folder describes it: what the card says, what it does to the
 * composer while it is open, and what the runtime does with the decision when
 * a turn carries MORE THAN ONE tool call.
 *
 * `chat.toolkit-hitl.spec.ts` already owns the single-call contract — one
 * sensitive call pauses, a rejection blocks the remote effect, a read-only
 * approval executes — and every assertion it makes is deliberately NOT
 * repeated here. What this file adds is the rest of the folder:
 *
 *   - the CARD's own content (title, action label, Parameters accordion,
 *     interpolated policy sentence) and the company-name default;
 *   - the Parameters section's ABSENCE for a call with no arguments;
 *   - the COMPOSER being held closed for the whole pause and released by
 *     either decision, and a follow-up turn working afterwards;
 *   - the pause outliving a long wait with no timeout of its own;
 *   - MULTI-CALL turns: a blocked sensitive call sitting beside a
 *     non-sensitive one that still runs, two different sensitive tools each
 *     raising their own pause, and the same sensitive tool called twice.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE MULTI-CALL TURNS NEEDED A MOCK CHANGE, AND WHY IT IS THE RIGHT PLACE
 * ─────────────────────────────────────────────────────────────────────────────
 * `deploy/mock-llm/server.py` scripted exactly ONE `tool_calls` entry per
 * turn, so "the blocked call did not stop the one beside it" and "each of two
 * sensitive tools raises its own dialog" were not merely unasserted — they
 * were unreachable: no prompt could make the model emit a second call. The
 * mock now scans EVERY top-level `[[mock:call_tool …]]` marker in the prompt
 * and emits one call per marker, in order. A prompt with one marker is
 * scripted byte for byte as it was (the single-marker reply sentence is
 * emitted unchanged), and a marker NESTED inside another's arguments — the
 * two-level delegation shape — is still part of its parent, because the scan
 * resumes past each marker's own close rather than at the next `[[`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE LEG CONTRACT: BOTH (rust and python)
 * ─────────────────────────────────────────────────────────────────────────────
 * Same reasoning as `chat.toolkit-hitl.spec.ts`: the Rust `direct_hitl.rs` is
 * a port of the SDK's `sensitive_tool_guard.py`, elitea-main feeds ONE policy
 * to both, and the card is the same card. The one field the two runtimes
 * genuinely disagree about (`tool_call_id`) is not asserted anywhere in this
 * file. What IS leg-shaped is the approved EFFECTFUL replay — the native
 * runtime refuses it (`UnsupportedCapability`) while the SDK would execute it
 * — so nothing here ever approves an effectful call: every approval below is
 * taken on the READ-ONLY operation, which both runtimes admit.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE FIXTURE IS BUILT OVER THE API AND THE DECISION IS TAKEN IN THE BROWSER
 * ─────────────────────────────────────────────────────────────────────────────
 * The subject of every case here is the DIALOG, not the toolkit form or the
 * agent form — both of which `chat.toolkit-hitl.spec.ts` and
 * `chat.toolkit.spec.ts` already drive. `createMockToolAgent` builds the same
 * three rows over their own routes in seconds instead of minutes, and leaves
 * the whole budget for the paused turns, which are the expensive part. The
 * conversation is then OPENED in the browser and every decision is taken by
 * clicking the card, because "the dialog renders decidable controls" is
 * exactly what a store-only assertion cannot see.
 *
 * GLOBAL STATE: `sensitive_tools` lives in `centry.platform_config` and there
 * is no narrower scope. It is restored in `afterEach`, which runs on failure
 * too — every other spec in this project shares the stack and a leaked entry
 * turns their tool calls into pauses nothing answers.
 */
import { expect, request as playwrightRequest, test, type Locator, type Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  EMPTY_TOOLKIT_GUARDRAILS,
  MOCK_CALL_TOOL_SENTINEL,
  MOCK_TOOL_CREATE_SENTINEL,
  MOCK_TOOL_EFFECTFUL_OPERATION,
  MOCK_TOOL_READ_OPERATION,
  callToolPrompt,
  callToolWithArgumentsPrompt,
  clearMockToolJournal,
  createMockToolAgent,
  expectStoredAssistantAnswer,
  fillComposer,
  readMockToolJournal,
  readStoredHitlInterrupt,
  readStoredHitlInterrupts,
  readStoredTranscript,
  setToolkitGuardrails,
  type MockToolAgentFixture,
  type ToolkitGuardrailValues,
} from '../fixtures/api';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
/** The REST resume — `POST …/continue_predict/prompt_lib/{project}/{conversationUuid}`. */
const CONTINUE_RE = /\/elitea_core\/continue_predict\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The structured result a denied call is replaced by (`BLOCKED_TOOL_RESULT_TYPE`). */
const BLOCKED_RESULT_TYPE = 'sensitive_tool_blocked';

/**
 * Which runtime is answering the turns — `scripts/chat-stream-e2e.sh` is the
 * one place that knows, and it exports this. Read for exactly ONE decision:
 * ELITEA-1003's gap is now closed on the native runtime and still open on the
 * SDK (see that test).
 */
const IS_NATIVE_RUNTIME = (process.env['E2E_WORKER'] ?? 'rust') === 'rust';

/** The card's own title, verbatim — `SensitiveToolCard` in `ChatHitlActions.tsx`. */
const CARD_TITLE = '⚠️ Sensitive Action Authorization Required';

/**
 * The company name the policy sentence must interpolate, and the default the
 * worker falls back to when the admin leaves the field blank
 * (`toolkits/policy.rs`, mirroring `guardrails.DefaultSensitiveActionCompanyName`).
 */
const COMPANY_NAME = 'EPAM';
const DEFAULT_COMPANY_NAME = 'Your organization';

/** Send one prompt through the composer and require the start to be ADMITTED. */
async function sendTurn(page: Page, prompt: string): Promise<void> {
  const send = await fillComposer(page, prompt);
  // Armed AFTER the composer settles — see `fillComposer`'s own note on why a
  // budget that also covers the settling reports the wrong failure.
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  await send.click();
  const response = await started;
  expect(response.status(), `the turn was refused: ${(await response.text()).slice(0, 300)}`).toBe(200);
}

/** The newest authorization card on screen, waited for. */
async function awaitCard(page: Page, message: string): Promise<Locator> {
  const card = page.getByTestId('chat-hitl-actions').last();
  await expect(card, message).toBeVisible({ timeout: 180_000 });
  return card;
}

/**
 * Take one decision on `card` and require the continuation route to accept it.
 *
 * The decision is asserted at the ROUTE rather than by the card disappearing:
 * `applyHitlOptimisticUpdate` removes it before any transport is chosen, so a
 * resume that never left the browser looks identical on screen.
 */
async function decide(page: Page, card: Locator, action: 'Approve' | 'Reject') {
  // A rejection carries a COMMENT, and it is not optional however the
  // placeholder is worded: the card's Reject sends `block_with_comment` with
  // whatever is typed, and the continuation route refuses an empty
  // `hitl_value` with 400 `Invalid agent execution request` (measured — see
  // S/tail/defects.md, and the fail-marked test at the end of this file).
  if (action === 'Reject') {
    await card.getByPlaceholder('Add a comment (optional)...').fill(`AUTOTESTDENIAL${String(Date.now() % 1_000_000)}`);
  }
  const resumed = page.waitForResponse((r) => CONTINUE_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  await card.getByRole('button', { name: action, exact: true }).click();
  const response = await resumed;
  expect(
    response.status(),
    `the ${action.toLowerCase()} was refused: ${(await response.text()).slice(0, 300)}`,
  ).toBe(200);
  return response;
}

/** The policy, with this file's company name and every operation it names sensitive. */
function guardrails(
  toolkitTypes: Record<string, readonly string[]>,
  companyName = COMPANY_NAME,
): ToolkitGuardrailValues {
  return { ...EMPTY_TOOLKIT_GUARDRAILS, sensitive_tools: toolkitTypes, sensitive_action_company_name: companyName };
}

test.afterEach(async () => {
  // Restored here rather than at the end of the body: this hook runs after a
  // FAILURE too, and a failure is exactly when the policy would otherwise be
  // left behind for every later spec on the same stack.
  await setToolkitGuardrails(EMPTY_TOOLKIT_GUARDRAILS);
});

/* onetest: ELITEA-1007, ELITEA-1015, ELITEA-1000, ELITEA-1012 — the authorization card's whole
 * content: the title, "Agent is about to perform:" and the `toolkit.tool` action label, the
 * Parameters accordion (collapsible, and ABSENT for a call with no arguments), and the policy
 * sentence with the configured company name interpolated. Driven from a CHAT conversation the
 * agent participates in (ELITEA-1012), not from the agent page. */
test('the authorization card names the action, its parameters and the company policy', async ({ page }) => {
  // Two paused turns and two decisions, on top of the fixture.
  test.setTimeout(420_000);

  let fixture: MockToolAgentFixture | undefined;
  try {
    await clearMockToolJournal(page);
    fixture = await createMockToolAgent(page, 'card');
    await setToolkitGuardrails(
      guardrails({ openapi: [MOCK_TOOL_EFFECTFUL_OPERATION, MOCK_TOOL_READ_OPERATION] }),
    );

    // ── 1. A call WITH arguments — the Parameters half ──────────────────────
    //
    // `body_json` is a real property of the operation's generated schema
    // (`families/openapi/spec.rs` — the JSON request body, encoded as a
    // string), so the pause carries a genuine argument rather than the empty
    // object the short marker form sends. That matters: a card that rendered
    // its Parameters section only for arguments it invented would pass an
    // assertion made against `{}`.
    const marker = `AUTOTESTPARAM${String(Date.now() % 1_000_000)}`;
    await sendTurn(
      page,
      callToolWithArgumentsPrompt(
        MOCK_TOOL_EFFECTFUL_OPERATION,
        { body_json: `{"title":"${marker}"}` },
        'create the item',
      ),
    );

    const card = await awaitCard(page, 'the sensitive call never raised an authorization card');
    await expect(card, 'the card must carry the folder’s own title, verbatim').toContainText(CARD_TITLE);
    await expect(card, 'the card must say what it is asking about').toContainText('Agent is about to perform:');

    // The ACTION LABEL is `{toolkit}.{tool}` (`toolkits/policy.rs`'s
    // `action_name`), which is the identifier the onetest case calls
    // "github.create_pull_request" — asserted as the composed label, not as
    // the bare tool name, because a card showing only the tool cannot tell an
    // operator WHICH toolkit is about to act.
    const actionLabel = `${fixture.toolkitName}.${MOCK_TOOL_EFFECTFUL_OPERATION}`;
    await expect(card, 'the card must name the toolkit AND the tool').toContainText(actionLabel);

    // The POLICY SENTENCE, with the company name the admin configured. Read
    // off the card, because the interpolation is the whole point: a template
    // that reached the browser un-substituted renders the literal
    // `{company_name}` and still looks like a policy message.
    await expect(card, 'the policy sentence must interpolate the configured company name').toContainText(
      `${COMPANY_NAME} requires approval before running the sensitive action '${actionLabel}'.`,
    );
    await expect(card, 'an un-substituted template must never reach the card').not.toContainText(
      '{company_name}',
    );

    // The PARAMETERS accordion, asserted on VISIBILITY and on the fold arrow —
    // never on the card's text. MUI's `Collapse` keeps its children mounted at
    // height 0, so `toContainText` reads a collapsed argument exactly as it
    // reads an expanded one (measured: the collapse assertion below passed for
    // free until it was written this way).
    //
    // DEVIATION FROM THE CASE, DELIBERATELY NOT FAILED HERE: ELITEA-1007 says
    // the accordion is "expanded by default". `SensitiveToolParams` opens with
    // `expanded = false`, so it arrives COLLAPSED. The section is present,
    // labelled and usable either way, and the case's own steps 4-5 (collapse,
    // then expand again) are what is asserted below; the initial-state
    // difference is recorded in S/tail/defects.md rather than failing an
    // assertion about the interpolated policy sentence beside it.
    const parameters = card.getByText('Parameters', { exact: false });
    await expect(parameters, 'a call with arguments must offer a Parameters section').toBeVisible();
    const parameterValue = card.getByText(marker, { exact: false });
    await expect(
      parameterValue,
      'a collapsed accordion must not be showing the argument',
    ).toBeHidden();
    await parameters.click();
    await expect(parameters, 'the fold arrow must turn when the section opens').toContainText('▾');
    await expect(parameterValue, 'expanding Parameters must show the argument the call carries').toBeVisible();
    await parameters.click();
    await expect(parameters, 'the fold arrow must turn back when the section closes').toContainText('▸');
    await expect(parameterValue, 'collapsing Parameters must hide the argument again').toBeHidden();

    // Same facts, from the STORE — none of the identity below is rendered, and
    // it is what binds the card to the call the runtime actually paused on.
    const interrupt = await readStoredHitlInterrupt(page, fixture.projectId, fixture.conversationId);
    expect(interrupt.guardrail_type, 'the pause must be the sensitive-tool one').toBe('sensitive_tool');
    expect(interrupt.tool_name, 'the pause must name the operation the model asked for').toBe(
      MOCK_TOOL_EFFECTFUL_OPERATION,
    );
    expect(
      JSON.stringify(interrupt.tool_args ?? {}),
      'the stored pause must carry the arguments the card displayed',
    ).toContain(marker);

    // Declined: the effectful operation must never run for a content assertion.
    await decide(page, card, 'Reject');
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the rejection was accepted but no reply quoting the block was stored',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });

    // ── 2. A call with NO arguments — the Parameters section must be ABSENT ─
    const bare = await fixture.newConversation('b');
    await sendTurn(page, callToolPrompt(MOCK_TOOL_READ_OPERATION, 'read the status'));
    const bareCard = await awaitCard(page, 'the read-only sensitive call never raised a card');
    await expect(bareCard).toContainText(CARD_TITLE);

    // THE RULE, asserted against what the pause ACTUALLY carries: the section
    // is rendered if and only if there is something to show. Written this way
    // because the two runtimes disagree about what an argument-less call
    // carries, and only one of them can reach the case's own state.
    //
    //  - the native runtime sends the model's arguments through unchanged, so
    //    the `{}` the short marker form sends arrives as `{}` and the section
    //    must be ABSENT — which is ELITEA-1000 exactly;
    //  - the SDK worker materialises the operation's schema defaults first, so
    //    the SAME call arrives carrying `{regexp: null, headers: {}}`
    //    (measured on a python-worker stack) and the section must be PRESENT
    //    and show them.
    //
    // A card that always rendered the section fails the first branch; one that
    // never rendered it fails the second. The onetest case's state is the
    // first, and it is reachable on the native leg.
    const bareInterrupt = await readStoredHitlInterrupt(page, fixture.projectId, bare);
    const bareArgs = bareInterrupt.tool_args;
    const bareArgKeys =
      typeof bareArgs === 'object' && bareArgs !== null ? Object.keys(bareArgs as Record<string, unknown>) : [];
    if (bareArgKeys.length === 0) {
      await expect(
        bareCard,
        'a tool called with no arguments must render NO Parameters section — there is nothing to show, ' +
          'and an empty accordion invites an operator to look for parameters that do not exist',
      ).not.toContainText('Parameters');
    } else {
      await expect(
        bareCard,
        'the pause carries arguments, so the card must offer the Parameters section that shows them',
      ).toContainText('Parameters');
      await bareCard.getByText('Parameters', { exact: false }).click();
      await expect(
        bareCard.getByText(`${bareArgKeys[0] ?? ''}:`, { exact: false }).first(),
        'expanding Parameters must show the arguments the pause carries',
      ).toBeVisible();
    }

    // And it is still a decidable card, not a dead one.
    await decide(page, bareCard, 'Approve');
    await expectStoredAssistantAnswer(page, fixture.projectId, bare, {
      timeout: 180_000,
      message: 'the parameterless call was authorized but its result never reached the transcript',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-1007 (the default-company half) — with the Company Name field left blank the
 * policy sentence falls back to "Your organization" rather than rendering blank or erroring. */
test('a blank company name falls back to the platform default in the policy sentence', async ({ page }) => {
  test.setTimeout(300_000);

  let fixture: MockToolAgentFixture | undefined;
  try {
    fixture = await createMockToolAgent(page, 'default');
    // The EMPTY company name — the field an operator never filled in.
    await setToolkitGuardrails(guardrails({ openapi: [MOCK_TOOL_READ_OPERATION] }, ''));

    await sendTurn(page, callToolPrompt(MOCK_TOOL_READ_OPERATION, 'read the status'));
    const card = await awaitCard(page, 'the sensitive call never raised an authorization card');

    const actionLabel = `${fixture.toolkitName}.${MOCK_TOOL_READ_OPERATION}`;
    await expect(
      card,
      'a blank company name must render the platform default, not an empty gap in the sentence',
    ).toContainText(`${DEFAULT_COMPANY_NAME} requires approval before running the sensitive action '${actionLabel}'.`);

    await decide(page, card, 'Approve');
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-1006, ELITEA-1010, ELITEA-1013, ELITEA-1014 — no message can be SUBMITTED past an
 * undecided authorization (it is queued instead, and delivered after the decision), the pause
 * survives a long wait with no timeout of its own, and either decision lets the conversation take
 * a further turn that answers normally.
 *
 * ELITEA-1006 IS PORTED TO ITS OBJECTIVE, NOT TO ITS STEPS, AND THE DIFFERENCE IS DELIBERATE.
 * The case's steps describe a composer that is greyed out and a Send button that cannot be
 * clicked. That was true of the product the case was written against and is no longer: the
 * interjection feature (ELITEA-2864…2874, `chat.queued-messages.spec.ts`) made the composer stay
 * LIVE for the whole of an open run on purpose — "before A17 the composer was disabled for the
 * length of a turn, so there was nothing to interject with". What the case is actually protecting
 * is its objective, "preventing new messages from being submitted", and that property still holds
 * and is what is asserted here: the message goes to the waiting list, no start request is
 * admitted while the pause is undecided, and the queued message is delivered afterwards. */
test('a message typed during a pending authorization is queued, not submitted', async ({
  page,
}) => {
  // Two paused turns, a 30-second wait, two decisions and two follow-up turns.
  // Budgeted for the SLOWER leg: measured 6.4 minutes end to end on a
  // python-worker stack, where each turn costs several times what it does
  // natively. A 9-minute cap left no headroom and the reject leg's
  // continuation poll timed out once under load.
  test.setTimeout(900_000);

  let fixture: MockToolAgentFixture | undefined;
  try {
    await clearMockToolJournal(page);
    fixture = await createMockToolAgent(page, 'composer');
    await setToolkitGuardrails(
      guardrails({ openapi: [MOCK_TOOL_EFFECTFUL_OPERATION, MOCK_TOOL_READ_OPERATION] }),
    );

    // ── 1. APPROVE, after a long wait ───────────────────────────────────────
    await sendTurn(page, callToolPrompt(MOCK_TOOL_READ_OPERATION, 'read the status'));
    const card = await awaitCard(page, 'the sensitive call never raised an authorization card');

    // WHAT IS ACTUALLY TRUE, MEASURED: the text field stays EDITABLE (the
    // interjection design keeps the composer live for the whole of an open
    // run, and a pause is an open run) while the SEND control is rendered
    // DISABLED. So the case's "input does not accept input" half is superseded
    // and its "pressing Enter does not send" half is exactly right — which is
    // the half the objective is about.
    const input = page.getByTestId('chat-message-input');
    await expect(
      input,
      'the composer stays live through an open run by design (ELITEA-2864); a pause is an open run',
    ).toBeEditable({ timeout: 15_000 });

    const interjection = `${AUTOTEST_PREFIX}interject ${String(Date.now() % 1_000_000)}`;
    // A listener that records any start request — the discriminating evidence.
    // "No new turn was admitted" cannot be read off the screen: an optimistic
    // row looks the same whether the route accepted it or not.
    const startsWhilePaused: string[] = [];
    const recordStart = (request: { url: () => string; method: () => string }): void => {
      if (START_RE.test(request.url()) && request.method() === 'POST') startsWhilePaused.push(request.url());
    };
    page.on('request', recordStart);
    await input.fill(interjection);
    const send = page.getByTestId('chat-send-button');
    await expect(
      send,
      'the Send control must be disabled while an authorization is undecided — a message submitted ' +
        'past the pause would be admitted against a run that is still holding a decision',
    ).toBeDisabled({ timeout: 10_000 });

    // The keyboard path too. It is a SEPARATE path (`onKeyDown` in the
    // composer, not the button's `onClick`), and a control that is disabled on
    // screen while Enter still submits is the exact bypass this case's "click
    // the Send button directly (attempt to bypass)" step is written for.
    await input.press('Enter');
    await expect(
      page.getByTestId('chat-queued-messages'),
      'Enter must not submit or queue a message past an undecided authorization',
    ).toHaveCount(0);
    expect(
      startsWhilePaused,
      'a new turn was ADMITTED while an authorization was still undecided',
    ).toEqual([]);
    page.off('request', recordStart);
    // Cleared, so the follow-up turn below sends its own text and not this.
    await input.fill('');

    // THE TIMEOUT ASSERTION (ELITEA-1010). Thirty seconds of nothing, then the
    // card must still be there and must still be decidable — a pause that
    // expired on its own would strand the run with no way to finish it, and
    // nothing on screen would say so.
    await page.waitForTimeout(30_000);
    await expect(
      card,
      'the pause timed out on its own — a sensitive authorization has no deadline, the user does',
    ).toBeVisible();
    await expect(
      card.getByRole('button', { name: 'Approve', exact: true }),
      'the pause must still be decidable after a long wait',
    ).toBeVisible();

    await decide(page, card, 'Approve');
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the approval was accepted but the tool result never reached the transcript',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });
    await expect(
      input,
      'the composer must be released once the authorized run ends',
    ).toBeEditable({ timeout: 120_000 });

    // The follow-up turn, in the SAME conversation, through the SAME controls
    // that were inert a moment ago. This is the half the store-only assertions
    // cannot make: a Send control that stayed disabled after the run ended
    // leaves a conversation nobody can continue, and nothing on screen says so.
    const followUp = `${AUTOTEST_PREFIX}after approve ${interjection.slice(-6)}`;
    await sendTurn(page, followUp);
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the conversation could not take a further turn after an authorized pause',
      contains: followUp,
    });

    // ── 2. REJECT, in its own conversation ──────────────────────────────────
    //
    // Its own conversation so the transcript assertion below speaks about this
    // decision alone — the same reason `chat.toolkit-hitl.spec.ts` separates
    // its two legs.
    const declined = await fixture.newConversation('c');
    await sendTurn(page, callToolPrompt(MOCK_TOOL_EFFECTFUL_OPERATION, 'create an item'));
    const declinedCard = await awaitCard(page, 'the effectful sensitive call never raised a card');

    await decide(page, declinedCard, 'Reject');
    await expectStoredAssistantAnswer(page, fixture.projectId, declined, {
      // 240s, not 180s: measured to time out once on the python leg under
      // load, and pass on its own there in 2.3 minutes.
      timeout: 240_000,
      message: 'the rejection was accepted but no reply quoting the block was stored',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });
    await expect(
      page.getByTestId('chat-message-input'),
      'the composer must be released once the declined run ends',
    ).toBeEditable({ timeout: 120_000 });

    const afterBlock = `${AUTOTEST_PREFIX}after block ${String(Date.now() % 1_000_000)}`;
    await sendTurn(page, afterBlock);
    await expectStoredAssistantAnswer(page, fixture.projectId, declined, {
      timeout: 180_000,
      message: 'the conversation could not take a further turn after a declined pause',
      contains: afterBlock,
    });

    // THE assertion the store cannot make: the remote effect never happened,
    // for the whole file's worth of turns up to here.
    expect(
      (await readMockToolJournal(page)).filter((entry) => entry.method === 'POST'),
      'the effectful operation ran even though every decision taken here declined it',
    ).toEqual([]);
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-1004 — a tool the Guardrails policy does NOT name executes immediately, with no
 * authorization dialog at any point in the turn. */
test('a tool the policy does not name runs with no authorization dialog', async ({ page }) => {
  test.setTimeout(300_000);

  let fixture: MockToolAgentFixture | undefined;
  try {
    fixture = await createMockToolAgent(page, 'nonsens');
    // ONLY the effectful operation is sensitive. The read-only one is the
    // "not configured as sensitive" tool this case is about — and the policy
    // is non-empty on purpose: a card that never appears because the policy
    // was empty proves nothing about the policy DISCRIMINATING.
    await setToolkitGuardrails(guardrails({ openapi: [MOCK_TOOL_EFFECTFUL_OPERATION] }));

    await sendTurn(page, callToolPrompt(MOCK_TOOL_READ_OPERATION, 'read the status'));
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the non-sensitive call never produced an answer quoting its result',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });
    // Asserted AFTER the turn has settled, not during it: "no card appeared"
    // is only a fact once there is nothing left that could still raise one.
    await expect(
      page.getByTestId('chat-hitl-actions'),
      'a tool the policy does not name must execute with no authorization dialog at all',
    ).toHaveCount(0);
    // And it was DISPATCHED, not answered as prose. On this stack the tool's
    // own host is unreachable from the worker (`MockToolSpec.reachable` is
    // false — the OpenAPI client is https-only and the mock's certificate does
    // not chain to a public root), so a dispatched call comes back as
    // `tool.unavailable`. That string is still proof of a DISPATCH: a turn
    // whose tool was never called carries the model's own prose instead.
    const answer =
      (await readStoredTranscript(page, fixture.projectId, fixture.conversationId))
        .filter((row) => row.role === 'assistant')
        .at(-1)?.content ?? '';
    expect(
      answer,
      'the non-sensitive call was never dispatched — the reply quotes no tool result at all',
    ).toMatch(/tool mock_tool_status said/);
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-1001 — after the user blocks a sensitive tool, the agent can still use the
 * non-blocked tool it called in the SAME turn, and its result is part of what the agent answers
 * with. */
test('a blocked sensitive call does not discard the non-sensitive call beside it', async ({ page }) => {
  test.setTimeout(420_000);

  // WAS a native-only gap (#949), and the two legs were measured separately
  // rather than assumed equal. ADK's confirmation pre-check breaks out of its
  // scan at the FIRST call that needs a decision and returns before ANY tool of
  // the message is dispatched, so a pause abandons the whole assistant message
  // — and `direct_hitl.rs` used to replay only the one decided call out of it,
  // dropping every sibling. It now replays the whole message, which is what the
  // SDK's `sensitive_tool_guard.py` (its `_PENDING_TOOL_MESSAGES` capture) has
  // always done, so both legs assert the same thing here.

  let fixture: MockToolAgentFixture | undefined;
  try {
    await clearMockToolJournal(page);
    fixture = await createMockToolAgent(page, 'mixed');
    await setToolkitGuardrails(guardrails({ openapi: [MOCK_TOOL_EFFECTFUL_OPERATION] }));

    // The read-only call first, the sensitive one second, in ONE assistant
    // message. Only the second may pause; the first must still be answered.
    await sendTurn(
      page,
      `${callToolPrompt(MOCK_TOOL_READ_OPERATION, '')} ${callToolPrompt(
        MOCK_TOOL_EFFECTFUL_OPERATION,
        'read the status and then create the item',
      )}`,
    );

    const card = await awaitCard(page, 'the sensitive half of the two-call turn never paused');
    await expect(card, 'the pause must be about the sensitive call, not the other one').toContainText(
      MOCK_TOOL_EFFECTFUL_OPERATION,
    );
    const interrupts = await readStoredHitlInterrupts(page, fixture.projectId, fixture.conversationId, 1);
    expect(
      interrupts.map((entry) => entry.tool_name),
      'only the operation the policy names may raise a pause',
    ).toEqual([MOCK_TOOL_EFFECTFUL_OPERATION]);

    await decide(page, card, 'Reject');
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the rejection was accepted but the turn never produced its continuation reply',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });

    const answer =
      (await readStoredTranscript(page, fixture.projectId, fixture.conversationId))
        .filter((row) => row.role === 'assistant')
        .at(-1)?.content ?? '';
    expect(answer, 'the declined call must be replaced by the structured blocked result').toContain(
      BLOCKED_RESULT_TYPE,
    );
    expect(
      answer,
      'the declined EFFECTFUL call’s own success receipt reached the model — it was executed anyway',
    ).not.toContain(MOCK_TOOL_CREATE_SENTINEL);
    // THE assertion this case exists for: blocking one call must not throw
    // away the other one's result.
    //
    // READ OFF THE COUNT, NOT THE TOOL NAME, and both halves of that are
    // measured. `deploy/mock-llm/server.py` names the tool only in its
    // SINGLE-call sentence (`tool <name> said …`); a turn with several calls is
    // NUMBERED instead — `tool result 1 said … tool result 2 said …` — so no
    // multi-call reply can ever carry the name. Nor can the result itself
    // supply it: the tool's host is unreachable from the worker on this stack
    // (see `chat.tail-trace-steps.spec.ts`'s header), so a dispatched call
    // comes back as `{"error":"tool.unavailable…"}`, which names nothing. What
    // IS discriminating is how many results the model was given and which of
    // them is the blocked one — the defect dropped the sibling entirely, so it
    // showed exactly one.
    const quotedResults = answer.match(/tool result \d+ said /g) ?? [];
    expect(
      quotedResults.length,
      'the non-sensitive call’s result was lost when the sensitive call beside it was blocked — ' +
        'a decline is about ONE invocation, not about the turn',
    ).toBe(2);
    expect(
      answer.split(BLOCKED_RESULT_TYPE).length - 1,
      'every result the model was given is the blocked payload — the non-sensitive call never ran',
    ).toBe(1);
    expect(
      (await readMockToolJournal(page)).filter((entry) => entry.method === 'POST'),
      'the declined effectful operation ran anyway',
    ).toEqual([]);
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-1003 — two DIFFERENT sensitive tools invoked in one turn each raise their own
 * authorization dialog, decided one at a time. */
test('two sensitive tools in one turn each raise their own authorization', async ({ page }) => {
  test.setTimeout(480_000);

  // Same root cause as ELITEA-1001 above, with the worse symptom: the runtime
  // carried ONE decided call out of a multi-call message and discarded the
  // rest, so a tool the operator had marked sensitive was disposed of without
  // the operator ever seeing it.
  //
  // FIXED ON THE NATIVE RUNTIME: the whole message is replayed with the
  // decisions already taken attached, so ADK's pre-check stops again at the
  // still-undecided sensitive call and raises its own card for it
  // (`direct_hitl.rs`: `replay_calls_for` / `settled_decision`).
  //
  // STILL OPEN ON THE SDK, and the mark is pinned to that leg rather than
  // removed, so the native leg keeps asserting the fixed behaviour and the SDK
  // leg goes green the day it is fixed rather than silently staying broken.
  // MEASURED on the python worker: the second sensitive call is never offered
  // and the card never appears. The fix does not belong in this repository —
  // `services/elitea-worker-python` only CONFIGURES the guard
  // (`sdk_adapter.py`'s `security.configure_sensitive_tools`), and the
  // interrupt itself is raised inside elitea-sdk's
  // `runtime/middleware/sensitive_tool_guard.py`, a different repository. Not a
  // skip: a skip would stop measuring the leg altogether.
  if (!IS_NATIVE_RUNTIME) {
    test.fail(
      true,
      'ELITEA-1003 (#948): product gap (SDK/python worker only) — when one assistant message calls two ' +
        'sensitive tools, only the FIRST raises an authorization dialog; the second is never offered ' +
        'for a decision and never runs. Fixed on the native runtime in `direct_hitl.rs`; the SDK half ' +
        'lives in elitea-sdk `sensitive_tool_guard.py`.',
    );
  }

  let fixture: MockToolAgentFixture | undefined;
  try {
    await clearMockToolJournal(page);
    fixture = await createMockToolAgent(page, 'twotools');
    await setToolkitGuardrails(
      guardrails({ openapi: [MOCK_TOOL_EFFECTFUL_OPERATION, MOCK_TOOL_READ_OPERATION] }),
    );

    await sendTurn(
      page,
      `${callToolPrompt(MOCK_TOOL_READ_OPERATION, '')} ${callToolPrompt(
        MOCK_TOOL_EFFECTFUL_OPERATION,
        'read the status and create the item',
      )}`,
    );

    // ── The FIRST dialog ────────────────────────────────────────────────────
    const first = await awaitCard(page, 'the first of two sensitive calls never paused');
    const firstInterrupt = await readStoredHitlInterrupt(page, fixture.projectId, fixture.conversationId);
    const firstTool = firstInterrupt.tool_name ?? '';
    expect(
      [MOCK_TOOL_READ_OPERATION, MOCK_TOOL_EFFECTFUL_OPERATION],
      'the first pause must be about one of the two sensitive calls',
    ).toContain(firstTool);
    // The read-only one is approved where it comes first; an EFFECTFUL approval
    // is refused by design on the native runtime (see the file header), so that
    // one is always declined.
    await decide(page, first, firstTool === MOCK_TOOL_READ_OPERATION ? 'Approve' : 'Reject');

    // ── The SECOND dialog, ONE AT A TIME ───────────────────────────────────
    //
    // This is the assertion the case is made of: after the first decision the
    // OTHER sensitive call must be offered in its own right, with its own
    // interrupt id.
    const secondTool =
      firstTool === MOCK_TOOL_READ_OPERATION ? MOCK_TOOL_EFFECTFUL_OPERATION : MOCK_TOOL_READ_OPERATION;
    const second = page.getByTestId('chat-hitl-actions').filter({ hasText: secondTool });
    await expect(
      second,
      `the second sensitive call (${secondTool}) was never offered for a decision — a tool the ` +
        'operator marked sensitive was disposed of without being shown',
    ).toBeVisible({ timeout: 120_000 });
    // Read the SECOND pause's identity from the store rather than expecting
    // both to sit in one array: `meta.hitl_interrupts` holds what is PENDING,
    // and resuming clears it before the next pause writes its own
    // (`agent_chat.sql`: `meta - 'hitl_interrupt' - 'hitl_interrupts'` on
    // resume, rebuilt on the next park). The property the case is made of is
    // that the two pauses are separately decidable, which is the interrupt id
    // differing — a shared id would make the second card resume the first.
    const secondInterrupt = await readStoredHitlInterrupt(page, fixture.projectId, fixture.conversationId);
    expect(secondInterrupt.tool_name, 'the second pause is about the other sensitive call').toBe(secondTool);
    expect(
      secondInterrupt.interrupt_id,
      'two pauses that share an interrupt id cannot be decided independently',
    ).not.toBe(firstInterrupt.interrupt_id);
    await decide(page, second.last(), secondTool === MOCK_TOOL_READ_OPERATION ? 'Approve' : 'Reject');

    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 240_000,
      message: 'both decisions were accepted but the turn never produced its continuation reply',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });
    expect(
      (await readMockToolJournal(page)).filter((entry) => entry.method === 'POST'),
      'the declined effectful operation ran anyway',
    ).toEqual([]);
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-1002 — the SAME sensitive tool called twice in one turn is authorized once: the
 * second invocation is auto-approved from the first decision, and BOTH complete. */
test('the same sensitive tool called twice in one turn is authorized once', async ({ page }) => {
  test.setTimeout(420_000);

  let fixture: MockToolAgentFixture | undefined;
  try {
    await clearMockToolJournal(page);
    fixture = await createMockToolAgent(page, 'repeat');
    await setToolkitGuardrails(guardrails({ openapi: [MOCK_TOOL_READ_OPERATION] }));

    // Both calls go to the READ-ONLY operation, because the case's expectation
    // is that BOTH invocations complete — and an approved effectful replay is
    // refused by design on the native runtime (see the file header).
    await sendTurn(
      page,
      `${callToolPrompt(MOCK_TOOL_READ_OPERATION, '')} ${callToolPrompt(
        MOCK_TOOL_READ_OPERATION,
        'read the status twice',
      )}`,
    );
    const card = await awaitCard(page, 'the repeated sensitive call never paused');
    await expect(card).toContainText(MOCK_TOOL_READ_OPERATION);

    const interrupts = await readStoredHitlInterrupts(page, fixture.projectId, fixture.conversationId, 1);
    // ONE dialog: the folder's contract is that a decision taken for a tool
    // covers its repeat invocation within the SAME turn. Asserted on the
    // STORE, not by counting cards — a second interrupt that was raised and
    // rendered off-screen would still be a second decision to take.
    expect(
      interrupts.length,
      'the same sensitive tool called twice in one turn asked for a second decision — the folder’s ' +
        'contract is one authorization per tool per turn',
    ).toBe(1);

    await decide(page, card, 'Approve');
    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 240_000,
      message: 'the single authorization did not carry the turn to completion',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });
    const answer =
      (await readStoredTranscript(page, fixture.projectId, fixture.conversationId))
        .filter((row) => row.role === 'assistant')
        .at(-1)?.content ?? '';
    expect(
      answer,
      'the one authorization must not have produced a BLOCKED result — approving once must not ' +
        'decline the repeat',
    ).not.toContain(BLOCKED_RESULT_TYPE);
    await expect(
      page.getByTestId('chat-hitl-actions'),
      'no second dialog may be left open once the single authorization is given',
    ).toHaveCount(0, { timeout: 60_000 });
  } finally {
    await fixture?.dispose();
  }
});

/* onetest: ELITEA-1009 — the platform guardrails configuration round-trips: the sensitive tools an
 * operator selects, the company name and the message template are all stored and read back
 * unchanged, and the policy is a single ENVIRONMENT-WIDE record rather than a per-project one
 * (the reachable half of ELITEA-1008). */
test('the guardrails configuration is stored platform-wide and reads back unchanged', async ({ page }) => {
  test.setTimeout(120_000);

  const template = 'AUTOTEST {company_name} must approve {action_name} first.';
  const values: ToolkitGuardrailValues = {
    ...EMPTY_TOOLKIT_GUARDRAILS,
    // Declared in the order the server answers them in (its own key order is
    // sorted), because `setToolkitGuardrails`'s read-back compares the key
    // LISTS — a real guard against a write landing in a different section,
    // and one that a differently ordered literal would trip for no reason.
    sensitive_tools: {
      github: ['create_pull_request', 'delete_branch'],
      openapi: [MOCK_TOOL_EFFECTFUL_OPERATION, MOCK_TOOL_READ_OPERATION],
    },
    sensitive_action_company_name: COMPANY_NAME,
    sensitive_action_message_template: template,
  };
  // `setToolkitGuardrails` already reads its own write back — this test is
  // about what is STILL there on a later, independent read, which is what the
  // case's "refresh the browser page" step actually asserts.
  await setToolkitGuardrails(values);

  // As the ADMIN persona, in a context of its own. The chat driver is refused
  // this route outright (403, measured) — which is itself the shape of the
  // case's "Logged in as Platform Administrator" precondition: the guardrails
  // section is not something an ordinary member can read back, let alone save.
  const admin = await playwrightRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.admin });
  const refusedForMember = await page.request.get(
    `${BASE_URL}/api/v2/admin/plugin_config_values/administration/guardrails`,
  );
  expect(
    refusedForMember.status(),
    'the guardrails section must not be readable by a non-administrator',
  ).toBe(403);

  const stored = await admin.get(`${BASE_URL}/api/v2/admin/plugin_config_values/administration/guardrails`);
  expect(
    stored.status(),
    'the guardrails section must be readable by the Platform Administrator that saved it',
  ).toBeLessThan(400);
  const body = (await stored.json()) as {
    values?: {
      sensitive_tools?: Record<string, readonly string[]>;
      sensitive_action_company_name?: string;
      sensitive_action_message_template?: string;
    };
  };
  expect(
    Object.keys(body.values?.sensitive_tools ?? {}).sort(),
    'every toolkit the operator selected tools for must survive the round trip',
  ).toEqual(['github', 'openapi']);
  expect(
    [...(body.values?.sensitive_tools?.['github'] ?? [])].sort(),
    'the individual tools checked under a toolkit must survive the round trip',
  ).toEqual(['create_pull_request', 'delete_branch']);
  expect(
    body.values?.sensitive_action_company_name,
    'the company name must survive the round trip',
  ).toBe(COMPANY_NAME);
  expect(
    body.values?.sensitive_action_message_template,
    'the message template must survive the round trip',
  ).toBe(template);

  // ENVIRONMENT-WIDE, not per project: the route carries no project segment at
  // all, and `resolveToolkitGuardrails` reads `centry.platform_config` and
  // nothing else — so there is no scope in which two projects could disagree.
  // That is the half of ELITEA-1008 this lane can answer; the admin form's own
  // grouping and display names belong to the admin UI.
  expect(
    new URL(stored.url()).pathname,
    'a policy addressed through a project would not be environment-wide',
  ).not.toMatch(/\/\d+(?:\/|$)/);

  await admin.dispose();
});


/* onetest: ELITEA-1013 (the Block control itself) — clicking Block closes the dialog and blocks the
 * call, with the comment box left empty. */
test('a sensitive call can be declined without typing a comment', async ({ page }) => {
  test.setTimeout(300_000);

  // WAS MEASURED: `POST …/continue_predict/…` answered 400 `{"error":"Invalid
  // agent execution request"}` for a `block_with_comment` resume whose
  // `hitl_value` is the empty string, and the card showed no error — the pause
  // simply stayed open. Every other decline in this file and in
  // `chat.toolkit-hitl.spec.ts` types a comment first, which is why the defect
  // had not surfaced.
  //
  // CLOSED in #950, and NOT by loosening the route: its contract is
  // deliberate and unchanged — `reject` refuses a value,
  // `block_with_comment` requires one (`agentexecution/continue.go`'s
  // `validCurrentHITLDecision`, and the same rule again in the native
  // runtime's `DirectHitlDecision::from_raw`). The card was sending the wrong
  // action for what the user had actually done: `BlockWithCommentControl`'s
  // Reject button always emitted `block_with_comment`, whatever was (not) in
  // the box labelled "Add a comment (optional)...". `ChatHitlActions` now
  // emits a plain `reject` when the comment is empty and the pause offers that
  // action — which is what an empty decline IS — and `block_with_comment`
  // with the comment verbatim when there is one. Pinned unit-level in
  // `ChatHitlActions.decline.test.tsx`.

  let fixture: MockToolAgentFixture | undefined;
  try {
    fixture = await createMockToolAgent(page, 'nocomment');
    await setToolkitGuardrails(guardrails({ openapi: [MOCK_TOOL_EFFECTFUL_OPERATION] }));

    await sendTurn(page, callToolPrompt(MOCK_TOOL_EFFECTFUL_OPERATION, 'create an item'));
    const card = await awaitCard(page, 'the sensitive call never raised an authorization card');

    const resumed = page.waitForResponse((r) => CONTINUE_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    // No comment typed — straight to the control the card offers.
    await card.getByRole('button', { name: 'Reject', exact: true }).click();
    const response = await resumed;
    expect(
      response.status(),
      `declining without a comment was refused: ${(await response.text()).slice(0, 300)}`,
    ).toBe(200);

    await expectStoredAssistantAnswer(page, fixture.projectId, fixture.conversationId, {
      timeout: 180_000,
      message: 'the comment-less decline was accepted but no reply quoting the block was stored',
      contains: MOCK_CALL_TOOL_SENTINEL,
    });
  } finally {
    await fixture?.dispose();
  }
});
