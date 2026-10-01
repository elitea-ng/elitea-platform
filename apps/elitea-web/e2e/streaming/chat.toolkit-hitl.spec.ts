/**
 * The SENSITIVE-ACTION contract: an operator marks a toolkit operation as
 * requiring authorization, the agent asks for it before the call, and the
 * decision the user takes in the browser decides whether the remote effect
 * happens at all.
 *
 * This is the one HITL shape whose failure mode is a real-world effect rather
 * than a stuck conversation. `chat.hitl.spec.ts` covers the clarification
 * pause, whose worst case is an unanswered question; here a control that does
 * not bind a decision to the exact call it was taken for lets a declined
 * action run. The native runtime is explicit about that boundary
 * (`services/elitea-worker-rust/src/agents/direct_hitl.rs`):
 *
 *   - a DENIED call is never dispatched. The real tool is replaced, before the
 *     runner is built, by a local adapter that emits a structured
 *     `sensitive_tool_blocked` result under the ORIGINAL call id;
 *   - an APPROVED call is replayed only when the tool is READ-ONLY
 *     (`is_read_only` — GET/HEAD/OPTIONS for an OpenAPI operation). An
 *     approved EFFECTFUL call is refused with `UnsupportedCapability`:
 *     "approved direct HITL replay remains closed for an effectful tool",
 *     because there is no durable effect receipt yet. That is a stated design
 *     boundary, not a gap in this journey — so the effectful leg here asserts
 *     the DENY path, and the approve leg uses the read-only operation, which
 *     is the one the runtime does admit.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE ASSERTIONS ARE SHAPED THIS WAY
 * ─────────────────────────────────────────────────────────────────────────────
 *  - the pause is asserted by its DECISION CONTROLS, never by its text. The
 *    sensitive card and the generic approve/reject card share one testid
 *    (`chat-hitl-actions`), and a card that renders its warning and no buttons
 *    is exactly the failure `chat.hitl.spec.ts` was written for;
 *  - the decision is asserted at the ROUTE, not by the card disappearing:
 *    `applyHitlOptimisticUpdate` removes it before any transport is chosen;
 *  - the OUTCOME is read from the STORE and from the TOOL journal together.
 *    The store says what the runtime told the model; the journal says whether
 *    the remote effect happened. Only the second can fail a runtime that
 *    reports a block and calls the tool anyway, which is the defect that
 *    matters;
 *  - the denial COMMENT is unique per run and is required to appear inside the
 *    stored blocked result. That is what proves the decision travelled the
 *    whole way — a resume that reached the route but not the runtime would
 *    still leave a second reply.
 *
 * GLOBAL STATE: `sensitive_tools` lives in `centry.platform_config` and there
 * is no narrower scope — `resolveToolkitGuardrails` reads it and nothing else.
 * It is written by the ADMIN persona and restored in `afterEach`, which runs on
 * failure too; every other spec in this project shares the stack and a leaked
 * entry would turn their tool calls into pauses nothing answers.
 *
 * ── WHY THIS RUNS ON BOTH WORKERS, AND THE ONE FIELD THAT DOES NOT ─────────
 *
 * This file used to skip the python leg with `native-runtime sensitive-tool
 * contract; the SDK worker guards differently`. That was asserted from the
 * header's own last line ("was not measured here"), not measured — and when it
 * finally was, it turned out to be almost entirely wrong. The Rust
 * `direct_hitl.rs` is a PORT of `elitea_sdk/runtime/middleware/
 * sensitive_tool_guard.py`, down to the constant: both spell
 * `BLOCKED_TOOL_RESULT_TYPE = 'sensitive_tool_blocked'`, both emit the same
 * six-key blocked payload, both carry the denial comment into `denial_reason`,
 * and both offer `['approve', 'reject', 'block_with_comment']`. elitea-main
 * feeds the SAME policy to both — `Policy.Runtime()`'s five fields reach the
 * SDK through `sdk_adapter._apply_toolkit_guardrails` → the SDK's own
 * `configure_sensitive_tools` — and its interrupt projection is opaque, so an
 * SDK-shaped pause populates `metadata.hitl_interrupt` like any other.
 *
 * What actually kept the python leg red was the freeze, the same defect
 * `chat.toolkit.spec.ts` names: a model pinned with no `temperature` key was a
 * bare `builtins.KeyError` inside the SDK. With that fixed, every assertion
 * below passes on a python-worker stack except ONE — `tool_call_id` — which is
 * genuinely absent there and is handled where it is asserted rather than by
 * giving up the whole leg. See step 4.
 *
 * ONE DIFFERENCE THIS JOURNEY DELIBERATELY DOES NOT EXERCISE: the approved
 * EFFECTFUL replay. The native runtime refuses it (`UnsupportedCapability`,
 * above); the SDK has no read-only predicate at all and would execute it for
 * real (`sensitive_tool_guard.py` branches on `action == 'reject'` and nothing
 * else). Both legs here approve only the READ-ONLY operation, so the two
 * runtimes agree on every path this file drives — but a spec that ever adds an
 * approved-effectful leg has to make it native-only.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  EMPTY_TOOLKIT_GUARDRAILS,
  MOCK_CALL_TOOL_SENTINEL,
  MOCK_TOOL_CREATE_SENTINEL,
  MOCK_TOOL_EFFECTFUL_OPERATION,
  MOCK_TOOL_READ_OPERATION,
  attachToolkitThroughPicker,
  callToolPrompt,
  clearMockToolJournal,
  createAgentThroughForm,
  createOpenApiToolkitThroughForm,
  expectStoredAssistantAnswer,
  fetchMockToolSpec,
  readMockToolJournal,
  readStoredHitlInterrupt,
  readStoredTranscript,
  setToolkitGuardrails,
} from '../fixtures/api';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
/** The REST resume — `POST …/continue_predict/prompt_lib/{project}/{conversationUuid}`. */
const CONTINUE_RE = /\/elitea_core\/continue_predict\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The model the pause depends on — pinned onto the version, see `chat.hitl.spec.ts`. */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/** The structured result a denied call is replaced by (`BLOCKED_TOOL_RESULT_TYPE`). */
const BLOCKED_RESULT_TYPE = 'sensitive_tool_blocked';

/**
 * Which runtime is answering the turns — `scripts/chat-stream-e2e.sh` is the
 * one place that knows, and it exports this. Native tool decisions carry
 * the interrupt and call identities. The SDK uses the root action fields.
 * Local default is the native-runtime dev stack, as elsewhere.
 */
const IS_NATIVE_RUNTIME = (process.env['E2E_WORKER'] ?? 'rust') === 'rust';

test.afterEach(async () => {
  // Restored here rather than at the end of the test body: this hook runs
  // after a FAILURE too, and a failure is exactly when the policy would
  // otherwise be left behind for every later spec on the same stack.
  await setToolkitGuardrails(EMPTY_TOOLKIT_GUARDRAILS);
});

test('a sensitive tool call pauses, a rejection blocks the effect, and a read-only approval runs', async ({
  page,
}) => {
  // No worker skip: see the header. Both runtimes implement the same
  // sensitive-tool contract — one is a port of the other — and the single
  // field they disagree about is handled at step 4.
  // Two paused turns, two human decisions and four model round trips, on top
  // of two form flows.
  test.setTimeout(540_000);

  const suffix = String(Date.now() % 1_000_000);
  const toolkitName = `${AUTOTEST_PREFIX}htk-${suffix}`;
  const agentName = `${AUTOTEST_PREFIX}htkagent-${suffix}`;
  /** Unique per run, so its appearance in the stored result names THIS decision. */
  const denialComment = `AUTOTESTDENIAL${suffix}`;

  await clearMockToolJournal(page);
  const spec = await fetchMockToolSpec(page);

  // ── 1. The toolkit, the agent, the attach ───────────────────────────────
  //
  // No credential step. The mock tool API is anonymous, and an `openapi`
  // toolkit with no `openapi_configuration` is now materializable: the freeze
  // writes `{}` for the absent reference rather than a JSON `null`, which the
  // native worker's `merged_auth_settings` refused outright. `chat.toolkit.spec.ts`
  // carries the full measurement; this journey shares the consequence, because
  // a toolkit that cannot materialize pauses on nothing.
  const { projectId, toolkitId } = await createOpenApiToolkitThroughForm(page, toolkitName, spec.text);

  const agent = await createAgentThroughForm(page, agentName);
  expect(agent.projectId, 'the agent and the toolkit must share a project').toBe(projectId);
  const storedAgent = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agent.agentId}`,
  );
  const storedMeta =
    ((await storedAgent.json()) as { version_details?: { meta?: Record<string, unknown> } }).version_details
      ?.meta ?? {};
  const pinned = await page.request.put(
    `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${projectId}/${agent.agentId}/${agent.versionId}`,
    { data: { meta: storedMeta, llm_settings: { model_name: MOCK_MODEL } } },
  );
  expect(pinned.status(), `the mock model must be pinnable: ${(await pinned.text()).slice(0, 300)}`)
    .toBeLessThan(300);

  // No navigation: `createAgentThroughForm` leaves the browser on the agent's
  // edit page, which is where the Tools panel and the Chat button live. A
  // `goto` would have to guess that page's route (`/agents/$tab/$agentId`), and
  // guessing it wrong looks exactly like a missing Tools panel.
  await expect(
    page.getByTestId('agent-toolkits-section'),
    'the save must land on the agent edit page, where the Tools panel lives',
  ).toBeVisible({ timeout: 30_000 });
  await attachToolkitThroughPicker(page, toolkitName);

  // ── 2. Mark BOTH operations sensitive ───────────────────────────────────
  //
  // Both, and in one write: the effectful one produces the deny leg and the
  // read-only one the approve leg, and re-writing the policy between the two
  // turns would make the second leg depend on a mid-conversation change this
  // journey does not otherwise exercise.
  await setToolkitGuardrails({
    ...EMPTY_TOOLKIT_GUARDRAILS,
    // Keyed by toolkit TYPE, matched on a canonical key (lowercase,
    // non-alphanumerics stripped) — `internal/domain/guardrails/policy.go`.
    sensitive_tools: { openapi: [MOCK_TOOL_EFFECTFUL_OPERATION, MOCK_TOOL_READ_OPERATION] },
  });

  // ── 3. Chat ─────────────────────────────────────────────────────────────
  //
  // The edit page's own URL is captured rather than reconstructed: the approve
  // leg comes back here to open a SECOND conversation, and guessing the route
  // (`/agents/$tab/$agentId`) wrong would look like a missing Chat button.
  const agentEditUrl = page.url();

  /** Open a new conversation with this agent through the edit page's Chat button. */
  async function openConversation(): Promise<string> {
    const created = page.waitForResponse(
      (r) =>
        /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
        r.request().method() === 'POST',
      { timeout: 45_000 },
    );
    await expect(page.getByTestId('chat-with-agent-button')).toBeEnabled({ timeout: 30_000 });
    await page.getByTestId('chat-with-agent-button').click();
    const body = (await (await created).json()) as { id?: string | number };
    const id = String(body.id ?? '');
    expect(id, 'the Chat button must create a conversation').not.toBe('');
    await page.waitForURL(new RegExp(`/app/chat/${id}(?:[/?#]|$)`), { timeout: 45_000 });
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 30_000 });
    return id;
  }

  const conversationId = await openConversation();

  // ── 4. TURN ONE — the EFFECTFUL call, rejected ──────────────────────────
  const input = page.getByTestId('chat-message-input');
  const effectfulStarted = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await input.fill(callToolPrompt(MOCK_TOOL_EFFECTFUL_OPERATION, `create an item ${suffix}`));
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();
  expect(
    (await effectfulStarted).status(),
    'the sensitive turn was refused before it could pause',
  ).toBe(200);

  const card = page.getByTestId('chat-hitl-actions').last();
  await expect(
    card,
    'the effectful call did not pause — is the sensitive_tools policy the runtime saw the one that was written?',
  ).toBeVisible({ timeout: 180_000 });
  await expect(card, 'the card must name the action it is asking about').toContainText(
    MOCK_TOOL_EFFECTFUL_OPERATION,
    { timeout: 30_000 },
  );

  // THE discriminating assertion: the CONTROLS. A pause that renders its
  // warning and no buttons is a run nothing can resume, and it looks like a
  // rendered card either way.
  const approve = card.getByRole('button', { name: 'Approve', exact: true });
  const reject = card.getByRole('button', { name: 'Reject', exact: true });
  await expect(approve, 'the sensitive pause offered no approve control').toBeVisible({ timeout: 30_000 });
  await expect(reject, 'the sensitive pause offered no reject control').toBeVisible();

  // The identity of the paused call, read from the STORE — none of it renders,
  // and it is what binds the decision below to this exact invocation.
  const interrupt = await readStoredHitlInterrupt(page, projectId, conversationId);
  expect(interrupt.guardrail_type, 'the pause must be the sensitive-tool one').toBe('sensitive_tool');
  expect(
    interrupt.available_actions ?? [],
    'a sensitive pause the user cannot decline is worse than no pause at all',
  ).toContain('reject');
  expect(interrupt.tool_name, 'the pause must name the operation the model asked for').toBe(
    MOCK_TOOL_EFFECTFUL_OPERATION,
  );
  expect(interrupt.toolkit_type, 'the pause must name the toolkit type the policy is keyed by').toBe('openapi');
  // THE ONE LEG-SHAPED ASSERTION IN THIS FILE, and the reason lives here
  // rather than in a skip that would have cost the other ~40.
  //
  // The native runtime stamps the LangChain call id onto the pause
  // (`direct_hitl.rs` → `events.rs`'s `hitl_interrupts[].tool_call_id`), so
  // the decision can be bound to the exact invocation it was taken for. The
  // SDK cannot: `sensitive_tool_guard.py` wraps the tool FUNCTION and never
  // sees a call id, so its interrupt payload has no such key at all — measured
  // on a python-worker stack, where this read returns `''` while every other
  // identity field below is populated. (Its only `tool_call_id` writer is the
  // parallel Application fan-out, whose `parallel_sensitive_tools` guardrail
  // the worker refuses outright.)
  //
  // Asserted on the native leg ALONE rather than dropped, because there it is
  // a real contract and a regression that stopped emitting it would otherwise
  // be invisible. What binds the decision on BOTH legs is `interrupt_id`, and
  // that is asserted unconditionally on the next line and used again at
  // step 5 against `resolved_hitl_interrupt_ids`.
  const blockedCallId = interrupt.tool_call_id ?? '';
  if (IS_NATIVE_RUNTIME) {
    expect(blockedCallId, 'the pause must carry the call id the decision is recorded against').not.toBe('');
  }
  const blockedInterruptId = interrupt.interrupt_id ?? '';
  expect(blockedInterruptId, 'the pause must carry an interrupt id').not.toBe('');

  // Reject it, with a comment. `available_actions` carries
  // `block_with_comment`, so the card renders the comment control and its
  // "Reject" sends `block_with_comment` with whatever was typed — that is the
  // path a user actually takes, so it is the path driven here.
  const rejected = page.waitForResponse(
    (r) => CONTINUE_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await card.getByPlaceholder('Add a comment (optional)...').fill(denialComment);
  await reject.click();

  const rejectResponse = await rejected;
  expect(
    rejectResponse.status(),
    `the rejection was refused: ${(await rejectResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  const rejectRequest = rejectResponse.request();
  expect(
    new URL(rejectRequest.url()).searchParams.get('execution_contract'),
    'the decision must go out on the HITL continuation contract',
  ).toBe('agent.continue.hitl.v1');
  const rejectBody = JSON.parse(rejectRequest.postData() ?? '{}') as {
    hitl_resume?: boolean;
    hitl_action?: string;
    hitl_value?: unknown;
    hitl_decisions?: readonly {
      interrupt_id?: string;
      tool_call_id?: string;
      action?: string;
      value?: unknown;
    }[];
  };
  expect(rejectBody.hitl_resume, 'the body must declare itself a HITL resume').toBe(true);
  if (IS_NATIVE_RUNTIME) {
    expect(rejectBody.hitl_decisions, 'the decision must bind the exact paused call').toEqual([
      {
        interrupt_id: blockedInterruptId,
        tool_call_id: blockedCallId,
        action: 'block_with_comment',
        value: denialComment,
      },
    ]);
    expect(rejectBody.hitl_action, 'a bound decision must not also send a root action').toBeUndefined();
    expect(rejectBody.hitl_value, 'the comment belongs to the bound decision').toBeUndefined();
  } else {
    expect(rejectBody.hitl_action, 'the comment control declines with block_with_comment').toBe(
      'block_with_comment',
    );
    expect(rejectBody.hitl_value, 'the typed comment must reach the route').toBe(denialComment);
  }

  // ── 5. The block, as the RUNTIME recorded it ────────────────────────────
  //
  // The mock's continuation script quotes the tool result verbatim, so the
  // stored reply is where `blocked_tool_result` becomes readable. The comment
  // is unique to this run, so finding it proves the decision reached the
  // runtime rather than merely the route.
  // The settle signal is the mock's END sentinel, not the comment. The comment
  // sits near the FRONT of a ~770-byte blocked payload, and polling on it
  // settled on a 370-byte prefix — the assertions below then read a row that
  // had not finished arriving. See `MOCK_CALL_TOOL_SENTINEL`.
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 180_000,
    message:
      'the rejection was accepted but no reply quoting the block was stored — the decision ' +
      'reached the route and not the runtime',
    contains: MOCK_CALL_TOOL_SENTINEL,
  });

  const afterReject = await readStoredTranscript(page, projectId, conversationId);
  const blockedAnswer = afterReject.filter((row) => row.role === 'assistant').at(-1);
  const blockedText = blockedAnswer?.content ?? '';
  expect(
    blockedText,
    'the runtime must replace the declined call with its structured blocked result',
  ).toContain(BLOCKED_RESULT_TYPE);
  expect(
    blockedText,
    'the comment typed into the card must reach the runtime and become the denial reason — ' +
      'a resume that reached the route and not the runtime would still store a second reply',
  ).toContain(denialComment);
  expect(blockedText, 'the blocked result must name the tool that was declined').toContain(
    MOCK_TOOL_EFFECTFUL_OPERATION,
  );
  expect(blockedText, 'the blocked result must name the toolkit the tool came from').toContain(toolkitName);
  expect(
    blockedAnswer?.metadata['resolved_hitl_interrupt_ids'] ?? [],
    'the decision must be recorded against the interrupt the pause raised, not merely against the turn',
  ).toContain(blockedInterruptId);

  // THE assertion the store cannot make: the remote effect did not happen.
  expect(
    (await readMockToolJournal(page)).filter((entry) => entry.method === 'POST'),
    'the call was declined and the effectful operation ran anyway',
  ).toEqual([]);
  // And its reply never reached the model. Independent of the journal: a tool
  // that ran and whose journal write was lost would still leak its receipt
  // into the transcript here.
  expect(
    blockedText,
    'the declined call’s own success receipt reached the model — it was executed',
  ).not.toContain(MOCK_TOOL_CREATE_SENTINEL);

  await expect(
    page.getByTestId('chat-message-input'),
    'the composer must be released once the declined run ends',
  ).toBeEditable({ timeout: 120_000 });

  // ── 6. TURN TWO — the READ-ONLY call, approved ──────────────────────────
  //
  // The runtime admits an approved replay only for a read-only tool, so this
  // is the leg that proves approval EXECUTES rather than merely resolving the
  // pause. The effectful counterpart is refused by design — see this file's
  // header — and is therefore not attempted.
  //
  // IN A NEW CONVERSATION. That began as a WORKAROUND: a conversation whose
  // previous turn went through a HITL resume refused the next start with 422
  // `unsupported_agent_execution` — measured 3/3 against this stack, while a
  // second turn after an ordinary turn and after a non-paused TOOL turn both
  // answered 200. Sending the second turn into the same conversation therefore
  // asserted a race: it passed once, when the send beat the resume's own
  // persistence, and failed on the rerun.
  //
  // THAT REFUSAL IS FIXED. The overlap gate refuses while the conversation
  // holds a response marked as being written, and the start path's settle wait
  // (`resolveAfterCurrentResponseSettles`,
  // services/elitea-main/internal/infra/db/repos/agent_start.go) used to answer
  // with the pre-settle refusal whenever the resume's terminal write landed
  // between its resolve and its settle probe — which is where a resumed run
  // puts it, because `pipeline_finish` and the terminal write are milliseconds
  // apart there. The wait now re-resolves on that report, and
  // `chat.hitl.spec.ts` sends its follow-up into the SAME conversation at the
  // moment the composer is released, which is the assertion that would catch a
  // regression.
  //
  // The separate conversation STAYS, for a reason of its own: the two legs are
  // different decisions on different operations, and giving each its own
  // conversation keeps the approve leg's transcript free of the reject leg's
  // blocked result — so `readMockToolJournal` and the stored-answer assertions
  // below speak about this decision alone.
  await page.goto(agentEditUrl);
  await expect(page.getByTestId('agent-toolkits-section')).toBeVisible({ timeout: 30_000 });
  const approveConversationId = await openConversation();
  const approveInput = page.getByTestId('chat-message-input');

  const readStarted = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await approveInput.fill(callToolPrompt(MOCK_TOOL_READ_OPERATION, `read the status ${suffix}`));
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 15_000 });
  await page.getByTestId('chat-send-button').click();
  expect((await readStarted).status(), 'the read-only sensitive turn was refused').toBe(200);

  const readCard = page.getByTestId('chat-hitl-actions').last();
  await expect(
    readCard,
    'a read-only operation named by the policy must pause too — sensitivity is the policy’s call, not the method’s',
  ).toBeVisible({ timeout: 180_000 });
  await expect(readCard).toContainText(MOCK_TOOL_READ_OPERATION, { timeout: 30_000 });
  const readInterrupt = await readStoredHitlInterrupt(page, projectId, approveConversationId);
  expect(readInterrupt.tool_name, 'the second pause must name the read-only operation').toBe(
    MOCK_TOOL_READ_OPERATION,
  );
  const approvedInterruptId = readInterrupt.interrupt_id ?? '';
  expect(approvedInterruptId, 'the second pause must carry an interrupt id').not.toBe('');
  expect(approvedInterruptId, 'the second pause must carry its own interrupt id').not.toBe(
    blockedInterruptId,
  );
  const approvedCallId = readInterrupt.tool_call_id ?? '';
  if (IS_NATIVE_RUNTIME) {
    expect(approvedCallId, 'the second pause must carry its exact call id').not.toBe('');
  }

  const approved = page.waitForResponse(
    (r) => CONTINUE_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await readCard.getByRole('button', { name: 'Approve', exact: true }).click();
  const approveResponse = await approved;
  expect(
    approveResponse.status(),
    `the approval was refused: ${(await approveResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  expect(
    new URL(approveResponse.request().url()).searchParams.get('execution_contract'),
    'the approval must use the HITL continuation contract',
  ).toBe('agent.continue.hitl.v1');
  const approveBody = JSON.parse(approveResponse.request().postData() ?? '{}') as {
    hitl_resume?: boolean;
    hitl_action?: string;
    hitl_decisions?: readonly {
      interrupt_id?: string;
      tool_call_id?: string;
      action?: string;
      value?: unknown;
    }[];
  };
  expect(approveBody.hitl_resume, 'the approval must declare a HITL resume').toBe(true);
  if (IS_NATIVE_RUNTIME) {
    expect(approveBody.hitl_decisions, 'the approval must bind the exact paused call').toEqual([
      {
        interrupt_id: approvedInterruptId,
        tool_call_id: approvedCallId,
        action: 'approve',
        value: '',
      },
    ]);
    expect(approveBody.hitl_action, 'a bound approval must not also send a root action').toBeUndefined();
  } else {
    expect(approveBody.hitl_action, 'the approve control must send the approve action').toBe('approve');
  }

  // Same settle signal, and here it is load-bearing in the other direction:
  // the assertion below is a NEGATIVE one (`not.toContain`), which a
  // half-written row satisfies for free.
  await expectStoredAssistantAnswer(page, projectId, approveConversationId, {
    timeout: 180_000,
    message:
      'the approval was accepted but no reply quoting the tool result was stored — the read-only ' +
      'replay was refused (the runtime logs direct_hitl.unsupported_capability) or never ran',
    contains: MOCK_CALL_TOOL_SENTINEL,
  });

  const afterApprove = await readStoredTranscript(page, projectId, approveConversationId);
  const approvedAnswer = afterApprove.filter((row) => row.role === 'assistant').at(-1);
  // THE discriminator between the two legs. A denied call is answered with the
  // blocked payload; an approved read-only call is answered with whatever the
  // tool's own transport produced. Requiring the blocked payload to be ABSENT
  // is what distinguishes "approved and executed" from "resolved the pause and
  // blocked anyway", which look identical on screen.
  expect(
    approvedAnswer?.content ?? '',
    'the approved reply must quote the result of the read-only operation',
  ).toContain(MOCK_TOOL_READ_OPERATION);
  expect(
    approvedAnswer?.content ?? '',
    'the approved read-only call was answered with a BLOCKED result — approval did not execute it',
  ).not.toContain(BLOCKED_RESULT_TYPE);
  expect(
    approvedAnswer?.metadata['resolved_hitl_interrupt_ids'] ?? [],
    'the approval must be recorded against its own interrupt',
  ).toContain(approvedInterruptId);

  expect(
    afterApprove.filter((row) => row.isError).map((row) => row.content.slice(0, 200)),
    'no row may be flagged is_error — a refused resume is stored as an assistant row and renders like an answer',
  ).toEqual([]);

  // Still no effectful call, for the whole conversation. Restated after the
  // second turn because an approval that replayed the WRONG persisted call
  // would surface exactly here.
  expect(
    (await readMockToolJournal(page)).filter((entry) => entry.method === 'POST'),
    'the effectful operation ran during the approved read-only turn',
  ).toEqual([]);

  // Cleanup is best-effort and deliberately last.
  await page.request.delete(`${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agent.agentId}`);
  await page.request.delete(`${BASE_URL}/api/v2/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`);
});
