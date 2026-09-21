/**
 * A PIPELINE ATTACHED TO AN AGENT AS A TOOL, WITH SWARM MODE ON (#939 group 3).
 *
 * `chat.pipeline-hitl-routes.spec.ts` proves a pipeline's own `hitl` node
 * end to end — chatted with DIRECTLY, in the pipeline's own test pane. Its
 * header records what it does not cover, and this file is exactly that:
 * ELITEA-1363..1367 put the same pipeline behind an AGENT, with Swarm mode
 * enabled and the pipeline attached as one of that agent's tools, and ask
 * whether the HITL prompt still reaches the person through the indirection.
 *
 * ─────────────────────────────────────────────────────────────────────────
 * IT DOES NOW — AND ONE HALF OF THE PREMISE REMAINS AN HONEST DEGRADE
 * ─────────────────────────────────────────────────────────────────────────
 *
 * 1. THE PIPELINE IS INVOKED AS A TOOL (#973, closed in fix wave 5). The
 *    native runtime used to compile exactly one kind of Application child — a
 *    nested `LlmAgent` — and refused a stored pipeline during ASSEMBLY, so one
 *    attached pipeline killed EVERY turn of that agent. Wave 3 stopped the
 *    bricking by skipping the child; wave 5 admits it.
 *
 *    `services/elitea-worker-rust/src/agents/application_pipeline.rs` compiles
 *    the child's stored graph and offers it as one `task` tool. Its `hitl`
 *    node pauses inside the child, the pause's own pending checkpoint travels
 *    on the descendant event the parent's durable session already persists,
 *    and the card reaches the chat through the parent's existing nested
 *    interrupt path — which is what this test drives end to end.
 *
 * 2. SWARM MODE IS A NO-OP ON THIS RUNTIME. `swarm` is in
 *    `PLATFORM_INTERNAL_TOOLS` — the list of names the native runtime
 *    RECOGNIZES AND SKIPS (`internal_tools.rs`) — so enabling it binds no
 *    tool and only adds a "this tool is unavailable" notice to the turn. That
 *    half is an honest degrade the product reports (#866), not a defect, and
 *    the test asserts it as a FACT rather than pretending the toggle did
 *    something.
 *
 * The fixture it builds — attach a pipeline to an agent as a tool and read the
 * reference back off the PARENT version — is the capability #939 group 3 asked
 * for.
 *
 * RUST ONLY, like its sibling, and gated rather than merely claimed: the
 * python (SDK) worker SERVES Swarm mode, so assertion 2's precondition is a
 * fact about this leg and not about that one (measured — see the `test.skip`
 * on the test itself). What the SDK worker does with a child pipeline's
 * `hitl` pause is a separate question this file makes no claim about.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  agentAsToolName,
  AUTOTEST_PREFIX,
  callToolWithArgumentsPrompt,
  createAgentWithVersion,
  expectStoredAssistantAnswer,
  fillComposer,
  PUBLISHABLE_TAGS,
  readCallerPersonalProjectId,
} from '../fixtures/api';
import { createPipelineThroughApi, deletePipeline, type CreatedPipeline } from '../fixtures/pipelines';

const API = `${BASE_URL}/api/v2`;

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** Same pinning rule as every other file in this lane: an unpinned version runs on the project default. */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/** The internal tool the source cases' precondition turns on. */
const SWARM = 'swarm';

/**
 * One `hitl` node routing Approve/Reject to two distinct `llm` nodes — the
 * same document `chat.pipeline-hitl-routes.spec.ts` proves compiles, pauses
 * and resumes when the pipeline is chatted with directly. Reused verbatim in
 * shape so that a failure here cannot be about the graph.
 */
const HITL_TEMPLATE = `state:
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
        value: You are the ROUTE_TAG_APPROVED assistant.
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
        value: You are the ROUTE_TAG_REJECTED assistant.
      task:
        type: variable
        value: input
    output:
      - messages
    transition: END
`;

function marker(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}${Date.now() % 1_000_000}`;
}

interface AttachedPipeline {
  readonly pipeline: CreatedPipeline;
  /** The function name the runtime would offer the pipeline under, if it admitted one. */
  readonly toolName: string;
}

/**
 * THE FIXTURE #939 GROUP 3 ASKED FOR: attach a pipeline to an agent as one of
 * its tools, and prove the reference really landed on the PARENT version.
 *
 * The URL names the CHILD and the body names the PARENT — the direction the
 * agent page's own picker sends, and the same call `chat.delegation.spec.ts`
 * makes for a sub-AGENT. The read-back is what makes this a write rather than
 * a 200: the freeze builds the runtime reference from this projection, so a
 * relation stored against the wrong version would leave the parent's tool list
 * empty behind a success.
 */
async function attachPipelineAsTool(
  page: Page,
  projectId: string,
  parent: { readonly id: string; readonly versionId: string },
  pipeline: CreatedPipeline,
): Promise<AttachedPipeline> {
  const attached = await page.request.patch(
    `${API}/elitea_core/application_relation/prompt_lib/${projectId}/${pipeline.id}/${pipeline.versionId}`,
    { data: { application_id: Number(parent.id), version_id: Number(parent.versionId), has_relation: true } },
  );
  expect(
    attached.status(),
    `the pipeline must attach to the agent version: ${(await attached.text()).slice(0, 300)}`,
  ).toBeLessThan(300);

  const stored = await page.request.get(`${API}/elitea_core/application/prompt_lib/${projectId}/${parent.id}`);
  const detail = (await stored.json()) as {
    version_details?: {
      tools?: readonly {
        type?: string;
        config?: { application_id?: number; application_version_id?: number };
      }[];
    };
  };
  const reference = (detail.version_details?.tools ?? []).find(
    (tool) => tool.type === 'application' && String(tool.config?.application_id ?? '') === pipeline.id,
  );
  expect(
    reference,
    'the agent version carries no `application` reference to the pipeline — the attach was a no-op',
  ).toBeDefined();
  expect(
    String(reference?.config?.application_version_id ?? ''),
    'the reference must name the pipeline VERSION the runtime would compile',
  ).toBe(pipeline.versionId);

  return {
    pipeline,
    toolName: agentAsToolName({ agentId: pipeline.id, versionId: pipeline.versionId, name: 'pipeline' }),
  };
}

/** Open a chat with the agent and mint its conversation, the way the agent page's Chat button does. */
async function openAgentChat(page: Page, agentId: string): Promise<string> {
  await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversationId = String(((await (await conversationCreated).json()) as { id?: unknown }).id ?? '');
  expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 45_000 });
  return conversationId;
}

/*
 * onetest: ELITEA-1363, ELITEA-1364, ELITEA-1365, ELITEA-1366, ELITEA-1367
 * (pipeline-nodes-hitl) — ONE journey for five cases because all five are the
 * same mechanism observed with different decorations (all three routes, two
 * sequential HITL nodes, the chat entry point, the agent entry point, a second
 * model). The decorations are covered where they are cheap to observe rather
 * than by five browser turns of the same shape: the third route, the two
 * sequential gates and the decline path are asserted against the runtime
 * itself in `application_pipeline_tests.rs`, and this journey drives the
 * INDIRECTION the five cases actually share — agent → attached pipeline →
 * card → decision → answer — once, through the browser.
 */
test('a Swarm-mode agent can invoke an attached pipeline, and its HITL prompt reaches the chat', async ({ page }) => {
  // RUST LEG ONLY, and measured rather than assumed. On the python (SDK) leg
  // `/runtime_capabilities` reports `internal_tools.swarm` TRUE — that worker
  // SERVES Swarm mode — so assertion 2 below, which pins the NATIVE runtime's
  // honest "recognized and skipped" degrade (#866) as a fact, is a statement
  // about this leg alone and fails there before the pipeline is ever reached.
  // The pipeline half is NOT the reason for the gate: the SDK worker builds
  // pipeline children through its own application toolkit, and what it does
  // with a child's `hitl` pause is simply not measured by this file.
  test.skip(
    (process.env['E2E_WORKER'] ?? 'rust') !== 'rust',
    'the SDK worker serves Swarm mode, so this file’s swarm precondition is a rust-leg fact',
  );
  test.setTimeout(420_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'the chat persona must own a personal project (#290)').not.toBe('');

  const stamp = String(Date.now()).slice(-7);
  const pipelineName = `${AUTOTEST_PREFIX}swarmpipe-${stamp}`;
  const agentName = `${AUTOTEST_PREFIX}swarmag-${stamp}`;
  let pipeline: CreatedPipeline | undefined;

  try {
    pipeline = await createPipelineThroughApi(page.request, pipelineName, {
      projectId,
      instructions: HITL_TEMPLATE,
    });

    // SWARM MODE ON — the source cases' precondition, stored on the agent's
    // own version exactly as the Tools panel's switch writes it.
    const agent = await createAgentWithVersion(
      page.request,
      agentName,
      {
        instructions:
          'You coordinate work. When a review is needed, hand it to the pipeline you have been ' +
          'given and report exactly what comes back.',
        welcomeMessage: 'Give me something to review.',
        conversationStarters: ['Review this.'],
        model: { modelName: MOCK_MODEL },
        meta: { step_limit: 25, internal_tools: [SWARM] },
        tags: PUBLISHABLE_TAGS,
      },
      projectId,
      `${AUTOTEST_PREFIX}swarm pipeline-tool fixture`,
    );

    const attachment = await attachPipelineAsTool(page, projectId, agent, pipeline);

    // The deployment's own verdict on the precondition, asserted rather than
    // assumed: `swarm` is a recognized-and-skipped name on this runtime, so
    // the toggle the cases require binds nothing. Stated here so a future
    // reader knows the premise was checked and not glossed.
    const capabilities = await page.request.get(`${API}/elitea_core/runtime_capabilities`);
    expect(capabilities.ok()).toBe(true);
    const served = (await capabilities.json()) as { internal_tools?: Record<string, boolean> };
    expect(
      served.internal_tools?.[SWARM],
      'this deployment does not serve Swarm mode — the cases’ precondition is unmet by the runtime, ' +
        'which reports it honestly rather than silently ignoring the toggle',
    ).toBe(false);

    const conversationId = await openAgentChat(page, agent.id);

    // The model is SCRIPTED to invoke the pipeline: the cases' "send a message
    // that triggers the pipeline toolkit execution", without depending on a
    // real model choosing to.
    const review = marker('review');
    const sendButton = await fillComposer(
      page,
      callToolWithArgumentsPrompt(attachment.toolName, { task: `Review this: ${review}` }, 'hand it over'),
    );
    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    await sendButton.click();
    expect((await started).status(), 'the turn must be admitted').toBe(200);

    // THE ASSERTION THE FIVE CASES SHARE: the pipeline's HITL node pauses, and
    // its card reaches the chat THROUGH the agent — the indirection is the
    // whole subject.
    const card = page.getByTestId('chat-hitl-actions').first();
    await expect(card, 'the pipeline’s HITL prompt must surface in the agent’s chat').toBeVisible({
      timeout: 180_000,
    });
    const reject = card.getByRole('button', { name: 'Reject', exact: true });
    await expect(reject, 'the HITL card must offer Reject').toBeVisible({ timeout: 15_000 });
    await reject.click();

    // And the Reject route runs to an answer, rather than the run dying at the
    // pause.
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 240_000,
      contains: review,
      message: 'the rejected pipeline produced no answer through its agent',
    });
  } finally {
    if (pipeline !== undefined) await deletePipeline(page.request, pipeline);
  }
});
