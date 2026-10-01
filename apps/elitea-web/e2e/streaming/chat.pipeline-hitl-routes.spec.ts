/**
 * A pipeline's OWN `hitl` node: the graph pauses on its own structure — no
 * model call is needed to produce the pause at all — and the Approve/Reject
 * decision a person makes in the browser must resume the RIGHT branch.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS IS A DIFFERENT CLAIM FROM EVERY OTHER HITL JOURNEY IN THIS DIRECTORY
 * ─────────────────────────────────────────────────────────────────────────────
 * `chat.hitl.spec.ts` is the runtime's `ask_user` INTERNAL TOOL — a model
 * decides to call it, mid-turn. `chat.toolkit-hitl.spec.ts` is a TOOLKIT
 * OPERATION an operator marked sensitive — a model decides to call the tool,
 * and the pause is a gate in front of the call. Both need the model to choose
 * to pause.
 *
 * A pipeline `type: hitl` node needs neither. It is graph structure, not a
 * tool: the compiler admits it as one of eight node families
 * (`services/elitea-worker-rust/src/agents/graph/compiler.rs`) and
 * `HitlNodeDefinition::interrupt_data`
 * (`services/elitea-worker-rust/src/agents/graph/hitl.rs`) stamps
 * `guardrail_type: "pipeline_hitl"` — a THIRD shape `ChatHitlActions.tsx`
 * branches on, distinct from `sensitive_tool`/`parallel_sensitive_tools`
 * (which render "Authorize"/"Block") and from `clarifying_question` (which
 * renders the answer-options card). `pipeline_hitl` falls through to the
 * plain `available_actions`-driven card — Approve/Reject/Edit buttons, no
 * special-cased branch — and nothing in this directory had exercised THAT
 * fallthrough before this file: `chat.toolkit-hitl.spec.ts` is always
 * `sensitive_tool`, and `chat.hitl.spec.ts` is always `clarifying_question`.
 *
 * The graph here routes the two decisions to two DIFFERENT downstream `llm`
 * nodes, each pinning a distinguishing fixed SYSTEM prompt — not because the
 * pause needs a model, but because *proving which branch actually ran* does:
 * the mock echoes only the last USER message (identical on both routes), so
 * the SYSTEM prompt `deploy/mock-llm/server.py`'s journal records is the one
 * observable that tells "resumed onto LLM_APPROVED" apart from "resumed onto
 * LLM_REJECTED". `task: {type: variable, value: input}` is the same
 * proven-working shape `chat.pipeline-execution.spec.ts` uses for its own
 * echo marker, so the SAME turn also proves the resumed run reached a real
 * node and did not just vanish into a dangling route.
 *
 * `edit_state_key` is deliberately UNSET: `HitlNodeDefinition::action_is_available`
 * gates Edit on `edit_state_key.is_some() && routes.edit != "END"`, and
 * driving that third route needs an editable value round-tripped through
 * `EditControl`'s own submit shape — a third turn this file does not spend.
 * The routes S1 ledger's ELITEA-1363/1366 ("all three routes", "across
 * models") and the Swarm-mode wrapper ELITEA-1364/1365/1367 (an AGENT with
 * Swarm mode enabled, the pipeline attached as a TOOLKIT, and the model
 * CHOOSING to invoke it) are recorded in the ledger as narrower or
 * unaddressed by this file — see `S/port/ledger-S1-stream.tsv`.
 *
 * WHY IT LIVES HERE: a pipeline turn needs the FULL standalone stack (runtime
 * plane, worker, model) — `journeys/**` has none of them. `--workers=1`
 * (`chat-stream` project) is why this file drives BOTH routes off ONE
 * pipeline, via the same "navigate away and back mints a fresh conversation"
 * mechanic `chat.pipeline-execution.spec.ts` already proves, rather than
 * paying a second pipeline-authoring round trip for a second `test()`.
 *
 * RUST ONLY. `STANDALONE_WORKER=rust` is what this repository's chat-stream
 * lane runs; the `hitl` node family is native-runtime graph compiler
 * territory (`agents/graph/hitl.rs`) with no SDK-worker equivalent measured
 * here, so this file makes no claim about the python leg.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  readCallerPersonalProjectId,
  readMockLlmJournal,
  fillComposer,
} from '../fixtures/api';
import { createPipelineThroughApi, deletePipeline, type CreatedPipeline } from '../fixtures/pipelines';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
/** The REST resume — `POST …/continue_predict/prompt_lib/{project}/{conversationUuid}` (same route `chat.hitl.spec.ts` resumes on). */
const CONTINUE_RE = /\/elitea_core\/continue_predict\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The two fixed system-prompt tags each downstream `llm` node is pinned with — the one observable the mock's journal carries per route (see header). */
const APPROVED_TAG = 'ROUTE_TAG_APPROVED';
const REJECTED_TAG = 'ROUTE_TAG_REJECTED';

/**
 * One `hitl` node (`review`) routing Approve/Reject to two distinct `llm`
 * nodes. Shape validated against
 * `services/elitea-worker-rust/src/agents/graph/compiler_tests.rs`'s own
 * whole-document `hitl` fixture (chained `hitl` nodes, no `output`/
 * `transition` on either) and `hitl_tests.rs`'s per-node fixture (the
 * `user_message` fstring placeholder). The `llm` nodes reuse the exact
 * `input_mapping` shape `chat.pipeline-execution.spec.ts`'s `STARTER_TEMPLATE`
 * already proves compiles and runs on this stack.
 */
const HITL_ROUTES_TEMPLATE = `state:
  input:
    type: str
  messages:
    type: list
entry_point: review
nodes:
  - id: review
    type: hitl
    input:
      - input
    user_message:
      type: fstring
      value: "Review: {input}"
    routes:
      approve: llm_approved
      reject: llm_rejected
  - id: llm_approved
    type: llm
    input:
      - input
    input_mapping:
      system:
        type: fstring
        value: You are the ${APPROVED_TAG} assistant.
      task:
        type: variable
        value: input
    output:
      - messages
    transition: END
  - id: llm_rejected
    type: llm
    input:
      - input
    input_mapping:
      system:
        type: fstring
        value: You are the ${REJECTED_TAG} assistant.
      task:
        type: variable
        value: input
    output:
      - messages
    transition: END
`;

/** A per-run marker the mock echoes back verbatim, tying a stored answer to the turn that asked for it. */
function marker(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}${Date.now() % 1_000_000}${Math.floor(Math.random() * 46_656).toString(36)}`;
}

/**
 * Open the pipeline editor's own test-chat pane and MINT its conversation —
 * the same mechanic `chat.pipeline-execution.spec.ts`'s `openTestChat` proves:
 * the pane creates its conversation on the first composer interaction, and a
 * fresh mount (a reload, or a navigate-away-and-back) always mints a NEW one.
 */
async function openTestChat(page: Page, pipeline: CreatedPipeline): Promise<string> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  const pane = page.getByTestId('edit-pipeline-test-chat');
  await expect(pane, 'the editor’s test chat must mount').toBeVisible({ timeout: 60_000 });

  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  const input = pane.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 30_000 });
  await input.click();
  const response = await conversationCreated;
  expect(
    response.status(),
    `the pane must create its own conversation: ${(await response.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
  const conversationId = String(((await response.json()) as { id?: string | number }).id ?? '');
  expect(conversationId, 'the pane must create a conversation before it can send').not.toBe('');
  return conversationId;
}

/** Send one message in the editor's test chat and require the START POST to be ADMITTED. */
async function sendTurn(page: Page, prompt: string): Promise<void> {
  const pane = page.getByTestId('edit-pipeline-test-chat');
  const sendButton = await fillComposer(pane, prompt);
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  await sendButton.click();
  const startResponse = await started;
  expect(
    startResponse.status(),
    `the pipeline turn was refused: ${(await startResponse.text()).slice(0, 400)}`,
  ).toBe(200);
}

/**
 * Wait for the `hitl` node's pause, click the named action, and require the
 * resume route to be ADMITTED. Returns nothing further: which branch the
 * resume actually reached is read from the mock's own journal by the caller.
 */
async function resumeHitl(page: Page, action: 'Approve' | 'Reject'): Promise<void> {
  const pane = page.getByTestId('edit-pipeline-test-chat');
  const card = pane.getByTestId('chat-hitl-actions').first();
  await expect(card, 'the pipeline reached its hitl node but no pause card was rendered').toBeVisible({
    timeout: 60_000,
  });
  /* elitea_issues: #5403 — "Pipeline HITL node does not display configured User Message" (falls back to a generic "Choose the action to proceed"): does not reproduce for the fstring mapping type — the templated message renders. */
  await expect(card, 'the card must show the templated review message').toContainText('Review:', {
    timeout: 20_000,
  });

  const button = card.getByRole('button', { name: action, exact: true });
  await expect(button, `the pause offered no ${action} control`).toBeVisible({ timeout: 20_000 });
  await expect(button, `${action} must be usable once the pause is rendered`).toBeEnabled();

  const resumed = page.waitForResponse(
    (r) => CONTINUE_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await button.click();
  const resumeResponse = await resumed;
  expect(
    resumeResponse.status(),
    `the ${action} decision was refused: ${(await resumeResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  const resumeBody = JSON.parse(resumeResponse.request().postData() ?? '{}') as {
    hitl_resume?: boolean;
    hitl_action?: string;
  };
  expect(resumeBody.hitl_resume, 'the resume body must declare itself a HITL resume').toBe(true);
  expect(resumeBody.hitl_action, `the click must send the ${action} action`).toBe(action.toLowerCase());
}

/** The resolved review card is also finalized; only the continuation row proves the resumed leg completed. */
async function expectResumedAnswer(
  page: Page,
  projectId: string,
  conversationId: string,
  token: string,
): Promise<void> {
  await expect
    .poll(
      async () => {
        const stored = await page.request.get(
          `${BASE_URL}/api/v2/elitea_core/messages/prompt_lib/${projectId}/${conversationId}`,
        );
        if (!stored.ok()) return { completed: false, includesToken: false };
        const body = (await stored.json()) as {
          items?: readonly {
            role?: string;
            content?: string;
            metadata?: { is_error?: boolean; pipeline_hitl_parent?: unknown };
          }[];
        };
        // BeginCurrentAgentPipelineHitlContinuation stamps this parent on the
        // resumed row; the finalized pause instead has pipeline_hitl_resolution.
        const assistant = body.items?.find(
          (item) => item.role === 'assistant' && item.metadata?.pipeline_hitl_parent !== undefined,
        );
        return {
          completed: assistant?.metadata?.is_error === false,
          includesToken: assistant?.content?.includes(token) === true,
        };
      },
      { timeout: 180_000, message: 'the resumed HITL leg must finalize its own successful answer' },
    )
    .toEqual({ completed: true, includesToken: true });
}

/** Bind the route proof to this project's exact turn, rather than the latest request from any conversation. */
async function expectTurnRoute(page: Page, projectId: string, token: string, tag: string): Promise<void> {
  let instructions: readonly string[] = [];
  await expect
    .poll(
      async () => {
        const entries = await readMockLlmJournal(page, projectId);
        instructions = entries
          .filter(
            (entry) =>
              entry.credential === `mock-key-project-${projectId}` &&
              entry.history.some((message) => message.role === 'user' && message.text === token),
          )
          .map((entry) => entry.instructions);
        return instructions.length;
      },
      { timeout: 30_000, message: 'the mock must record exactly one model request for this resumed turn' },
    )
    .toBe(1);
  expect(instructions[0], 'this decision must reach its own downstream system prompt').toContain(tag);
}

test('a pipeline hitl node resumes each decision onto its OWN route — Approve, then Reject', async ({ page }) => {
  // One pipeline, driven twice (`--workers=1` — see header): pause is free (no
  // model call), but each resumed leg costs one. Two full round trips plus the
  // "navigate away and back" remount for the second. Bounded well under this.
  test.setTimeout(420_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona must work inside its own project').not.toBe('');
  const name = `${AUTOTEST_PREFIX}hitlroute-${Date.now() % 1_000_000}`;
  const pipeline = await createPipelineThroughApi(page.request, name, {
    projectId,
    instructions: HITL_ROUTES_TEMPLATE,
  });

  try {
    // ── Run A: Approve ──────────────────────────────────────────────────
    const approveConversation = await openTestChat(page, pipeline);
    const approveToken = marker('approve');
    await sendTurn(page, approveToken);
    await resumeHitl(page, 'Approve');

    await expectResumedAnswer(page, projectId, approveConversation, approveToken);

    // THE branch proof. The mock echoes the SAME token on either route (the
    // `task: {type: variable, value: input}` mapping is identical on both
    // `llm` nodes), so only the SYSTEM prompt its journal recorded tells
    // "resumed onto llm_approved" apart from "resumed onto llm_rejected".
    await expectTurnRoute(page, projectId, approveToken, APPROVED_TAG);

    // ── Run B: Reject, in a FRESH conversation off the SAME pipeline ────
    // A second `test()` would re-author the pipeline for no reason: the graph
    // under test is the same document, and `chat.pipeline-execution.spec.ts`
    // already measures that a navigate-away-and-back remount mints its own
    // conversation rather than resuming the first.
    await page.goto(`${BASE_URL}/app/pipelines/all`);
    await page.waitForURL('**/app/pipelines/all**', { timeout: 20_000 });

    const rejectConversation = await openTestChat(page, pipeline);
    expect(
      rejectConversation,
      'the second mount must mint its own conversation rather than resume the first',
    ).not.toBe(approveConversation);

    const rejectToken = marker('reject');
    await sendTurn(page, rejectToken);
    await resumeHitl(page, 'Reject');

    await expectResumedAnswer(page, projectId, rejectConversation, rejectToken);
    await expectTurnRoute(page, projectId, rejectToken, REJECTED_TAG);
  } finally {
    await deletePipeline(page.request, pipeline);
  }
});
