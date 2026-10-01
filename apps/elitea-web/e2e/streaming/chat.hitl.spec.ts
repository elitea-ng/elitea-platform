/**
 * The CLARIFYING QUESTION round trip: an agent that stops to ask, a user who
 * answers in the browser, and a run that finishes on that answer.
 *
 * The native Rust runtime's `ask_user` internal tool parks a turn on a durable
 * confirmation and stores an assistant row whose interrupt carries
 * `guardrail_type: 'clarifying_question'`, `available_actions: ['answer']` and
 * a `questions` array with options. Every OTHER pause shape this app renders
 * (approve / reject / edit / block-with-comment) was already wired. This one
 * was not, and the failure was silent in the way that matters most:
 *
 *  1. `ChatHitlActions` knew four actions and `'answer'` matched none of them,
 *     so the pause rendered its question TEXT and ZERO controls — a card that
 *     looks like an answer, on a run nothing can resume;
 *  2. `questions` was dropped by BOTH delivery paths (the stream reducer's
 *     single-pause assembly and the stored-message reader), so even a card
 *     with controls would have had nothing to render them from;
 *  3. `buildHitlContinueBody` refused an `'answer'` action, returning
 *     `undefined` — which the handler reads as "no REST body fits" and falls
 *     back to a socket that is a no-op stub in every shipped deployment. The
 *     resume reached no transport at all.
 *
 * None of the three fails a unit suite: the components are individually
 * correct and the wiring between them is what was missing (the recurring
 * "composition root" class). So this journey drives the WHOLE loop in a
 * browser and asserts the three things a broken loop cannot produce: answer
 * CONTROLS on the pause card, a stored FINAL answer that quotes what was
 * answered, and a conversation that can still be SENT TO afterwards.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE ASSERTIONS ARE SHAPED THIS WAY
 * ─────────────────────────────────────────────────────────────────────────────
 *  - the pause is asserted by its OPTION BUTTON and its SUBMIT, never by the
 *    question text alone: the broken card printed the question too;
 *  - the resume is asserted as a 200 from the continuation ROUTE, not as a
 *    card that disappeared — the handler patches the message optimistically
 *    before any transport is chosen, so the card vanishes even when nothing
 *    was sent;
 *  - the final answer is read from the STORE and must quote the answered
 *    option. The mock's continuation script echoes the substituted tool result
 *    (`MOCK: resumed User answered: …`), so "Staging" in the stored row proves
 *    the answer reached the MODEL — not merely that the route accepted a body.
 *    An answer that never reached it would still store a second reply;
 *  - the FOLLOW-UP turn is sent at the moment the composer is released, before
 *    anything is read back. Section 7 carries why that order is the assertion
 *    and not an accident: waiting for the stored answer first waits for the
 *    very state whose absence refused the send.
 *
 * WHY IT LIVES HERE: `journeys/**` runs against
 * `docker-compose.e2e-standalone.yml`, which has no runtime plane, no worker
 * and no model backend. The `chat-stream` project matches `streaming/chat.*`
 * and runs against the full standalone stack via `scripts/chat-stream-e2e.sh`,
 * serially (`--workers=1`).
 *
 * WORKER: BOTH — the native Rust runtime (`STANDALONE_WORKER=rust`) and the
 * SDK worker (`=python`). It is a two-runtime journey on purpose: the same UI
 * drives both, so a change that admits one and refuses the other passes every
 * unit suite, and the assertions below are the only thing that can tell them
 * apart. See "THE PYTHON LEG" on the test itself for what it took to get here.
 *
 * A RACE THIS SPEC USED TO LOSE: the stored assistant row is readable while
 * the continuation is still streaming, and the first draft polled only for
 * the resumed marker (first chunk) before reading the transcript once — so
 * it sampled a half-written row and reported the answer missing, locally
 * and in CI. Both stored reads now happen at the END, after the follow-up
 * turn has been answered, so nothing is sampled mid-write.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  createAgentThroughForm,
  expectStoredAssistantAnswer,
  readStoredHitlInterrupt,
  readStoredTranscript,
} from '../fixtures/api';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
/** The REST resume — `POST …/continue_predict/prompt_lib/{project}/{conversationUuid}`. */
const CONTINUE_RE = /\/elitea_core\/continue_predict\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/**
 * The `[[mock:ask_user]]` contract, restated.
 *
 * These four values are the wire contract with `deploy/mock-llm/server.py`
 * (`ASK_USER_MARKER`, `ASK_USER_QUESTIONS`, and the `ask_user_resumed` reply).
 * They are restated rather than imported because the mock is a Python file in
 * another service's tree and there is nothing to import — so the pairing is
 * ASSERTED instead: a stack whose mock does not honour the marker answers with
 * the plain echo, and the pause card below never appears.
 */
const ASK_USER_MARKER = '[[mock:ask_user]]';
const QUESTION_TEXT = 'Which environment should I target?';
const OPTION_LABEL = 'Staging';

/**
 * The model the pause depends on.
 *
 * Pinned onto the agent VERSION rather than picked in the composer: an
 * application turn runs on `version_details.llm_settings.model_name`
 * (`ResolveCurrentApplicationTurn` → the worker's `validate_model`), and an
 * empty `llm_settings` falls back to whatever the project catalogue names as
 * its default — which on a stack that also carries a real provider is not the
 * mock, and the scripted tool call would never be emitted. `E2E_MOCK_MODEL`
 * overrides it for a stack whose mock is titled differently.
 *
 * The provider prefix is part of the name: bifrost resolves the provider from
 * the model string alone, so `seed-llm` titles the catalogue row `vllm/…`.
 */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/** The runtime-owned internal tool that produces the pause. */
const ASK_USER_TOOL = 'ask_user';
const IS_NATIVE_RUNTIME = (process.env['E2E_WORKER'] ?? 'rust') === 'rust';

test('an ask_user pause renders answerable controls, and the answer finishes the run', async ({ page }) => {
  // ── THE PYTHON LEG, AND WHY IT IS NO LONGER SKIPPED ─────────────────────
  //
  // This ran on the native runtime alone for a long time, under a sentence
  // that was false on both halves: "the Python worker has no equivalent
  // internal tool … the agent never pauses". `ask_user` is not native-only —
  // it is `elitea_sdk/runtime/tools/ask_user.py`'s `AskUserTool`, registered
  // by `runtime/toolkits/tools.py` from the same `meta.internal_tools` name —
  // and the SDK worker DOES pause on it. Everything through step 5 already
  // passed there: the card rendered, `Staging` was offered, the submit armed.
  //
  // What failed was the ANSWER, in two places, both in
  // services/elitea-worker-python/src/elitea_worker/agents/sdk_adapter.py:
  //
  //  1. `_require_in_process_hitl_resume` admitted {approve, reject, edit,
  //     block_with_comment} for a decision carrying no `guardrail_type`, and
  //     elitea-main omits that field for every root pause (`continue.go`'s
  //     `omitempty`) — so `answer` matched nothing and the worker refused with
  //     `{"code":"UNSUPPORTED_CAPABILITY"}`. The SDK's own handler for it
  //     (`langraph_agent.py`, `guardrail_type == 'clarifying_question'`) was
  //     never reached, the run never finished, and the conversation was left
  //     holding a stale interrupt that refused every later turn. It admits
  //     `answer` with its value now, and hands the SDK the DECODED answer map
  //     so the model reads the question it asked rather than the wire format.
  //  2. `_normalize_questions` re-keyed the questions `q1`, `q2`… and dropped
  //     the `id` the model sent, so the same model output produced
  //     `{q1: 'Staging'}` here against `{environment: 'Staging'}` there. The
  //     adapter installs an `ask_user` argument schema that carries the id
  //     now, so both runtimes advertise the ids the MODEL chose — which is
  //     what lets step 6 below assert one payload for both legs instead of
  //     going leg-aware the way `chat.toolkit-hitl.spec.ts` does for
  //     `tool_call_id`.
  //
  // Both runtimes receive the same answer value. The native pause also
  // supplies a call identity, which selects a bound continuation decision.

  // Two model round trips with a human decision between them: create, save,
  // chat, admission, the pause, the resume and a second dispatch. Every wait
  // below is bounded well under this, so a real hang fails on its own step
  // rather than on the clock.
  test.setTimeout(360_000);

  const name = `${AUTOTEST_PREFIX}ask-${Date.now() % 1_000_000}`;

  // ── 1. Author the agent through the form ────────────────────────────────
  const { projectId, agentId, versionId } = await createAgentThroughForm(page, name);

  // ── 2. Give it the ask_user tool and the deterministic model ────────────
  //
  // `meta` is REPLACED wholesale by `UpdateVersion`, not merged, so what the
  // form stored is read back first and only `internal_tools` is changed —
  // writing a bare `{internal_tools: […]}` would silently drop `step_limit`,
  // which the runtime reads.
  const stored = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
  );
  expect(stored.ok(), 'the agent the form created must be readable').toBe(true);
  const storedMeta =
    ((await stored.json()) as { version_details?: { meta?: Record<string, unknown> } }).version_details?.meta ?? {};

  const saved = await page.request.put(
    `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${projectId}/${agentId}/${versionId}`,
    {
      data: {
        meta: { ...storedMeta, internal_tools: [ASK_USER_TOOL] },
        // `openai_compatible` and `model_project_id` are deliberately absent:
        // elitea-main is the owner of both and derives them from the project's
        // catalogue row (`resolveCurrentAgentModel`), overwriting whatever a
        // version carries.
        llm_settings: { model_name: MOCK_MODEL },
      },
    },
  );
  expect(
    saved.status(),
    `the version must accept the ask_user tool: ${(await saved.text()).slice(0, 300)}`,
  ).toBeLessThan(300);

  // What the SERVER stores, not what this test sent. An admission gate that
  // refuses `ask_user` would answer 200 here and then refuse every turn with a
  // 422 that names neither the tool nor the runtime.
  const readback = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
  );
  const readbackDetail = (await readback.json()) as {
    version_details?: { meta?: { internal_tools?: readonly string[] }; llm_settings?: { model_name?: string } };
  };
  expect(
    readbackDetail.version_details?.meta?.internal_tools ?? [],
    'the ask_user tool must survive the save, or the agent has nothing to pause on',
  ).toContain(ASK_USER_TOOL);
  expect(
    readbackDetail.version_details?.llm_settings?.model_name,
    'the mock model must be pinned on the version, or the scripted pause never happens',
  ).toBe(MOCK_MODEL);

  // ── 3. Chat with it ─────────────────────────────────────────────────────
  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversation = (await (await conversationCreated).json()) as { id?: string | number; uuid?: string };
  const conversationId = String(conversation.id ?? '');
  expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 45_000 });

  // ── 4. Ask something the agent must clarify ────────────────────────────
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 30_000 });
  await input.fill(`${ASK_USER_MARKER} pick the environment`);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();

  const startResponse = await started;
  expect(
    startResponse.status(),
    `the ask_user turn was refused: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);

  // ── 5. The pause, and the controls it must offer ───────────────────────
  const card = page.getByTestId('chat-hitl-actions').first();
  await expect(
    card,
    'the run paused for a clarification but no HITL card was rendered',
  ).toBeVisible({ timeout: 150_000 });
  await expect(card, 'the card must show the question the agent asked').toContainText(QUESTION_TEXT, {
    timeout: 30_000,
  });

  // THE discriminating assertion. The broken card rendered the question text
  // and nothing else, so only the CONTROLS tell a rendered pause from an
  // answerable one.
  const option = page.getByTestId('hitl-answer-option-0-0');
  await expect(
    option,
    'the clarification offered no option control — the pause cannot be answered',
  ).toBeVisible({ timeout: 30_000 });
  await expect(option, 'the first option must be the one the model offered').toHaveText(OPTION_LABEL);
  const submit = page.getByTestId('hitl-answer-submit');
  await expect(submit, 'the clarification offered no submit').toBeVisible();
  await expect(
    submit,
    'submitting before anything is chosen would resume the run with an empty answer',
  ).toBeDisabled();

  // ── 6. Answer it, through the UI ───────────────────────────────────────
  const interrupt = await readStoredHitlInterrupt(page, projectId, conversationId);
  const resumed = page.waitForResponse(
    (r) => CONTINUE_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await option.click();
  await expect(submit, 'the submit must arm once an option is chosen').toBeEnabled({ timeout: 10_000 });
  await submit.click();

  // The ROUTE, not the disappearing card: `applyHitlOptimisticUpdate` removes
  // the card before any transport is chosen, so a resume that reached nothing
  // looks exactly like one that succeeded.
  const resumeResponse = await resumed;
  expect(
    resumeResponse.status(),
    `the clarification answer was refused: ${(await resumeResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  const resumeRequest = resumeResponse.request();
  expect(
    new URL(resumeRequest.url()).searchParams.get('execution_contract'),
    'the resume must go out on the HITL continuation contract',
  ).toBe('agent.continue.hitl.v1');
  const resumeBody = JSON.parse(resumeRequest.postData() ?? '{}') as {
    hitl_action?: string;
    hitl_value?: unknown;
    hitl_resume?: boolean;
    hitl_decisions?: readonly {
      interrupt_id?: string;
      tool_call_id?: string;
      action?: string;
      value?: unknown;
    }[];
  };
  expect(resumeBody.hitl_resume, 'the body must declare itself a HITL resume').toBe(true);
  // STRUCTURED, keyed by the question id the model chose — the shape
  // `currentHITLValue` canonicalises and both runtimes read back
  // (`AskUserRequest::format_answer`; `_hitl_resume_value` into the SDK's
  // `AskUserTool._format_answer`). A body carrying the card's encoded JSON as
  // a plain string would also be accepted, and would reach the model as a
  // quoted blob. `environment` is the MODEL's own id, not a positional `q1`,
  // Both runtimes must receive the same selected answer value.
  if (IS_NATIVE_RUNTIME) {
    const interruptId = interrupt.interrupt_id ?? '';
    const toolCallId = interrupt.tool_call_id ?? '';
    expect(interruptId, 'the native pause must identify its interrupt').not.toBe('');
    expect(toolCallId, 'the native pause must identify the paused call').not.toBe('');
    expect(resumeBody.hitl_decisions, 'the answer must bind the exact paused call').toEqual([
      {
        interrupt_id: interruptId,
        tool_call_id: toolCallId,
        action: 'answer',
        value: { environment: OPTION_LABEL },
      },
    ]);
    expect(resumeBody.hitl_action, 'a bound decision must not also send a root action').toBeUndefined();
    expect(resumeBody.hitl_value, 'the structured answer belongs to the bound decision').toBeUndefined();
  } else {
    expect(resumeBody.hitl_action, 'the clarification is answered with the `answer` action').toBe('answer');
    expect(
      resumeBody.hitl_value,
      'the answer must reach the route as a structured value, not as an encoded string',
    ).toEqual({ environment: OPTION_LABEL });
  }

  // ── 7. The composer is released — and the NEXT turn must be admitted ───
  //
  // A released composer is not proof the conversation is alive. A conversation
  // that can never be sent to again satisfies every assertion above and every
  // assertion below: the card is gone, the answer is stored, the input is
  // editable. The refusal lands on the NEXT send, which nothing here used to
  // make — so the approve leg of `chat.toolkit-hitl.spec.ts` had to open a
  // second conversation to get past it.
  //
  // Measured before the fix (3/3 against this stack): the start POST answered
  // 422 `unsupported_agent_execution`, while a second turn after an ORDINARY
  // turn answered 200 (`chat.multiturn.spec.ts`). The overlap gate refuses
  // while the conversation holds a response marked as being written, and the
  // start path's settle wait (`resolveAfterCurrentResponseSettles`,
  // services/elitea-main/internal/infra/db/repos/agent_start.go) threw away the
  // settle report it was waiting for: when the resume's terminal write landed
  // between the resolve and the probe, it answered with the pre-settle
  // refusal. A resumed run's `pipeline_finish` and its terminal write are
  // milliseconds apart, so the composer is released essentially AT that write
  // and the next send lands on it.
  //
  // ORDER IS THE DISCRIMINATOR HERE, and it is why the stored-answer poll now
  // runs AFTER this send rather than before it. That poll waits for the
  // resumed answer to be durable — which is exactly the state whose absence
  // produced the refusal — so a spec that asserts the answer first can never
  // reach the window. Send at the moment the product invites it.
  const composer = page.getByTestId('chat-message-input');
  await expect(
    composer,
    'the composer must be released once the answered run ends',
  ).toBeEditable({ timeout: 90_000 });

  const followUpToken = `hitl${Date.now().toString(36)}${Math.floor(Math.random() * 46_656).toString(36)}`;
  const followUpStarted = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await composer.fill(`autotest echo exactly: ${followUpToken}`);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();

  // The STATUS, read from the route. A refused send leaves the typed text in
  // the composer and paints nothing, which on screen is indistinguishable from
  // one still in flight; and the 422 is stored nowhere, so a transcript
  // assertion alone would report "no third row yet" and time out on the wrong
  // thing.
  const followUpResponse = await followUpStarted;
  expect(
    followUpResponse.status(),
    'the turn AFTER an answered clarification was refused — the conversation is dead once it has been resumed: ' +
      `${(await followUpResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  expect(
    new URL(followUpResponse.url()).pathname.split('/').pop(),
    'the follow-up must be started against the conversation the clarification was answered in',
  ).toBe(conversation.uuid ?? '');

  // ── 8. Both answers, read from the store ───────────────────────────────
  //
  // Admitted is not answered. The newest assistant row must quote THIS
  // question's token, so a follow-up that started and then died fails here
  // rather than passing on the resumed answer already in the transcript.
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 150_000,
    message: 'the follow-up turn was admitted but never answered',
    contains: followUpToken,
  });

  // Polled for the COUNT as well: the pair is written by two statements and a
  // read that lands between them sees three rows, which would read as "a
  // message was lost".
  await expect
    .poll(async () => (await readStoredTranscript(page, projectId, conversationId)).length, {
      timeout: 30_000,
      message: 'the conversation must hold the answered clarification AND the follow-up pair',
    })
    .toBe(4);

  const transcript = await readStoredTranscript(page, projectId, conversationId);
  expect(
    transcript.map((row) => row.role),
    'the transcript must read question, answer, question, answer',
  ).toEqual(['user', 'assistant', 'user', 'assistant']);
  // The mock's continuation script quotes the substituted tool result
  // (`MOCK: resumed "User answered: …"`), so "Staging" in the stored row proves
  // the answer reached the MODEL — not merely that the route accepted a body.
  // An answer that never reached it would still store a second reply.
  expect(
    transcript[1]?.content ?? '',
    'the reply to the clarification must quote the answered option — the continuation reached the route and not the runtime, or the answer never reached the model',
  ).toContain(OPTION_LABEL);
  expect(
    transcript[3]?.content ?? '',
    'the follow-up answer must answer the FOLLOW-UP question, not repeat the resumed one',
  ).toContain(followUpToken);
  expect(
    transcript.filter((row) => row.isError).map((row) => row.content.slice(0, 200)),
    'no row may be flagged is_error — a refused turn is stored as an assistant row and renders like an answer',
  ).toEqual([]);

  // Cleanup is best-effort and deliberately last: a failure above should leave
  // the agent in place for inspection.
  await page.request.delete(`${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`);
});
