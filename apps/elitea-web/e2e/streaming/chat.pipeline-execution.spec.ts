/**
 * RUNNING a pipeline from the editor's own test chat: what a transcript looks
 * like after one turn, after two, after a navigation, and after a graph the
 * runtime cannot compile.
 *
 * Ported BY USE CASE from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/pipelines/
 * test_pipeline_execution.py`, six tests), each test below naming the legacy
 * tests it answers.
 *
 * ── How this differs from the two pipeline turn journeys already here ────
 *
 * `chat.pipeline.spec.ts` proves a pipeline turn RUNS THE GRAPH — its
 * discriminator is an execution-trace step naming the pipeline node, which is
 * the only thing that can tell a compiled graph from a fall-through to the
 * direct-agent assembler. `chat.pipeline-authored.spec.ts` proves the same for
 * a graph authored on the canvas. Both send exactly ONE message and neither
 * looks at the transcript as a whole.
 *
 * The legacy execution suite is about the transcript: does it start empty,
 * does each send add both rows, is the user's own message there beside the
 * answer, does a second send accumulate rather than replace, does the pane
 * still work after navigating away, and does an unrunnable pipeline fail
 * visibly instead of hanging. None of that was covered.
 *
 * ── Where the assertions are read ───────────────────────────────────────
 *
 * The STORE, through `readStoredTranscript`/`expectStoredAssistantAnswer`,
 * never the message bubbles. The legacy tests count DOM nodes and wait for the
 * count to be stable for three seconds; on this platform the stream runs ahead
 * of the store, a refused turn renders its failure AS an assistant card, and a
 * row that is still being written carries no `is_error` key at all — so "two
 * bubbles appeared and stopped changing" cannot tell a finished answer from a
 * refusal or from a turn that is still running. `e2e/fixtures/api.ts`'s two
 * helpers carry the measurements.
 *
 * ── Why the pipelines are created through the FORM ──────────────────────
 *
 * This project signs in as the chat persona, which works inside its OWN
 * personal project (#290). The create POST's URL is where that project id
 * comes from; a hardcoded `1` reads a pipeline that does not exist there —
 * measured, `application not found` on a pipeline that had just saved
 * (`pipelines.graph-authoring.spec.ts` records it). The graph then arrives by
 * version PUT for the reason `chat.pipeline.spec.ts`'s header gives: the YAML
 * pane is a CodeMirror document and `fill()` on it re-indents every newline,
 * so a typed multi-line document is not the document typed.
 *
 * Lives in `streaming/` because a pipeline turn needs the FULL standalone
 * stack — runtime plane, worker and model (`scripts/chat-stream-e2e.sh`). The
 * `chromium`/`webkit` journeys stack has none of them.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  expectStoredAssistantAnswer,
  expectStoredTurnRefusal,
  fillComposer,
  readStoredTranscript,
} from '../fixtures/api';

const APPLICATIONS_RE = /\/elitea_core\/applications\/prompt_lib\/(\d+)/;
/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/**
 * The document a pipeline created through `/pipelines/create` is stored with
 * (`src/shared/lib/pipelineStarterTemplate.ts`), reproduced here because
 * nothing under `e2e/` imports from `src/`.
 *
 * It is the smallest graph BOTH runtimes run, and each line is load-bearing
 * for a measured reason its own source records: every `state:` entry is a
 * MAPPING rather than a bare type name (the SDK's `set_defaults` writes into
 * each entry and raises `TypeError` on a string, which killed a turn before
 * any node ran), and `input_mapping` carries `system` AND `task` (the SDK
 * selects its pipeline branch on `'system' in func_args`). `task` reads the
 * builtin `input`, which is where the turn's own text is seeded — that is what
 * makes the answer below an echo of the prompt this file sent.
 */
const STARTER_TEMPLATE = `state:
  input:
    type: str
  messages:
    type: list
entry_point: LLM_1
nodes:
  - id: LLM_1
    type: llm
    input:
      - input
    input_mapping:
      system:
        type: fstring
        value: You are a helpful assistant.
      task:
        type: variable
        value: input
    output:
      - messages
    transition: END
`;

/**
 * A document the runtime cannot compile: no nodes at all.
 *
 * The legacy `test_empty_pipeline_execution` uses "a pipeline with only an END
 * node", which is the same thing — `END` is the graph's terminal and has no
 * node entry of its own. The compiler refuses a zero-node document outright
 * (`compiler.rs:459`, mirrored client-side by `graphAdmission.helpers.ts`'s
 * `document.node-count`), so this is a pipeline that is stored, opened,
 * admitted by the API and then cannot run — exactly the state the legacy test
 * is about.
 */
const EMPTY_TEMPLATE = `state:
  input:
    type: str
  messages:
    type: list
entry_point: END
nodes: []
`;

/** One pipeline created through the real form, carrying every id the reads below need. */
interface CreatedPipeline {
  readonly projectId: string;
  readonly pipelineId: string;
  readonly versionId: string;
  readonly versionName: string;
}

/**
 * Create a pipeline through its own form and store `instructions` with the
 * version PUT — the shape `chat.pipeline.spec.ts` establishes, including its
 * assertion that the create page really stored a PIPELINE row.
 */
async function createPipelineWithGraph(page: Page, name: string, instructions: string): Promise<CreatedPipeline> {
  const created = page.waitForResponse(
    (r) => APPLICATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await page.goto(`${BASE_URL}/app/pipelines/create`);
  await expect(page.getByTestId('agent-name-input')).toBeVisible({ timeout: 30_000 });
  await page.getByTestId('agent-name-input').fill(name);
  await page.getByTestId('agent-description-input').fill(`${name} description`);
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('pipeline-save-button').click();

  const response = await created;
  expect(response.status(), `the pipeline must be created: ${(await response.text()).slice(0, 300)}`).toBe(201);
  const projectId = APPLICATIONS_RE.exec(new URL(response.url()).pathname)?.[1] ?? '';
  expect(projectId, 'the pipeline must belong to a project').not.toBe('');
  const body = (await response.json()) as {
    id?: string;
    version_details?: { id?: string; name?: string; agent_type?: string };
  };
  const pipeline: CreatedPipeline = {
    projectId,
    pipelineId: String(body.id ?? ''),
    versionId: String(body.version_details?.id ?? ''),
    versionName: String(body.version_details?.name ?? 'latest'),
  };
  expect(pipeline.pipelineId, 'the created pipeline must carry an id').toMatch(/^\d+$/);
  expect(pipeline.versionId, 'the created pipeline must carry a version').not.toBe('');
  expect(
    body.version_details?.agent_type,
    'the pipelines create page must store a PIPELINE row — an agent here means the page dropped `forPipeline`',
  ).toBe('pipeline');

  const saved = await page.request.put(
    `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${pipeline.projectId}/${pipeline.pipelineId}/${pipeline.versionId}`,
    {
      data: {
        name: pipeline.versionName,
        agent_type: 'pipeline',
        instructions,
        conversation_starters: [],
        variables: [],
        meta: { step_limit: 25, internal_tools: [] },
        pipeline_settings: { nodes: [], edges: [] },
      },
    },
  );
  expect(saved.status(), `the graph must reach the version row: ${(await saved.text()).slice(0, 300)}`).toBeLessThan(300);

  // Read back what the SERVER stored, not what was sent: `instructions` is the
  // exact string the compiler parses, so a store that trimmed or re-encoded it
  // fails the turn later with a message about a malformed graph rather than
  // about the write that mangled it.
  const stored = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${pipeline.projectId}/${pipeline.pipelineId}`,
  );
  const detail = (await stored.json()) as { version_details?: { instructions?: string } };
  expect(detail.version_details?.instructions, 'the stored instructions ARE the graph').toBe(instructions);

  return pipeline;
}

/**
 * Open the editor's own test chat and MINT its conversation, returning the id.
 *
 * The pane creates the conversation on the first composer interaction rather
 * than on mount, so opening a pipeline to read its graph leaves none behind —
 * and that response is the only place this conversation's id is ever
 * published (unlike `chat.pipeline.spec.ts`, there is no Chat button whose
 * click could carry it).
 *
 * The page is reloaded first so the pane reads the version this file just
 * wrote rather than the one the create page left it holding.
 */
async function openTestChat(page: Page, pipeline: CreatedPipeline): Promise<string> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.pipelineId}`);
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
    `the editor pane must create its own conversation: ${(await response.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
  const conversationId = String(((await response.json()) as { id?: string | number }).id ?? '');
  expect(conversationId, 'the pane must create a conversation before it can send').not.toBe('');
  return conversationId;
}

/**
 * Send one message in the editor's test chat and require the START POST to be
 * ADMITTED. Returns nothing — what the turn produced is read from the store.
 *
 * 422 is the status every admission refusal produces and its body is the
 * sentence a maintainer will search for, so it is spelled into the failure.
 */
async function sendTurn(page: Page, prompt: string): Promise<void> {
  const pane = page.getByTestId('edit-pipeline-test-chat');
  // `fillComposer` retries the fill: the pane discards one that lands while it
  // is still resolving its conversation, and the Send control is rendered only
  // while the composer holds text. The response budget is armed AFTER it, so a
  // slow composer cannot be reported as an unadmitted turn.
  const sendButton = await fillComposer(pane, prompt);
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  await sendButton.click();
  const startResponse = await started;
  expect(startResponse.status(), `the pipeline turn was refused: ${(await startResponse.text()).slice(0, 400)}`).toBe(200);
}

/** A per-run marker the offline mock echoes back, so an answer can be tied to the turn that asked for it. */
function marker(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}${Date.now() % 1_000_000}`;
}

/*
 * Legacy: `TestExecutePipeline::test_pipeline_response_is_meaningful`,
 * `TestExecutePipeline::test_message_count_starts_at_zero_and_grows`,
 * `TestPipelineChatMessages::test_user_message_visible` and
 * `TestPipelineChatMessages::test_multiple_executions_accumulate`.
 *
 * One journey, because all four are claims about the same transcript at
 * successive points in one conversation, and each turn costs a real model
 * call in a lane that runs `--workers=1`. Two turns rather than the legacy's
 * three: "grew by exactly two, twice" is the whole of the accumulation claim,
 * and a third adds a third of the runtime for a repetition of it.
 */
test('a pipeline answers in the editor’s test chat, and each send appends the question and the answer', async ({
  page,
}) => {
  test.setTimeout(420_000);
  // 32 characters is the form's own `maxLength`; a longer name is silently
  // truncated and every later lookup by it finds nothing.
  const name = `${AUTOTEST_PREFIX}run-${Date.now() % 1_000_000}`;
  const pipeline = await createPipelineWithGraph(page, name, STARTER_TEMPLATE);
  const conversationId = await openTestChat(page, pipeline);

  // The transcript STARTS EMPTY. The legacy test reads `0` off the DOM; this
  // reads the store, which is also the only place a welcome message or a
  // leftover row from another journey would show up.
  expect(
    await readStoredTranscript(page, pipeline.projectId, conversationId),
    'a pipeline conversation must start with no messages',
  ).toHaveLength(0);

  const first = marker('first');
  await sendTurn(page, `Reply to this: ${first}`);
  // `contains` is the discriminator: the offline mock echoes the last user
  // message, so an answer carrying this run's own marker cannot be a cached
  // reply, another conversation's, or the error card a refused turn renders.
  await expectStoredAssistantAnswer(page, pipeline.projectId, conversationId, {
    timeout: 180_000,
    contains: first,
    message: 'the pipeline turn stored no answer — the graph was admitted and then produced no result',
  });

  const afterFirst = await readStoredTranscript(page, pipeline.projectId, conversationId);
  expect(afterFirst, 'one send must store exactly the question and the answer').toHaveLength(2);
  // The USER's own message is in the transcript beside the answer — the legacy
  // `test_user_message_visible`. Read in `sort_order=asc`, so `[0]` is the
  // question and `[1]` the reply; a transcript route that answered in the
  // documented `created_at DESC` default would fail here rather than silently
  // reverse the conversation (#603).
  expect(afterFirst[0]?.role).toBe('user');
  expect(afterFirst[0]?.content).toContain(first);
  expect(afterFirst[1]?.role).toBe('assistant');
  expect(afterFirst[1]?.isError, 'the stored answer must not be a refusal').toBe(false);

  const second = marker('second');
  await sendTurn(page, `Reply to this: ${second}`);
  // `contains: second` — NOT "an answer arrived" — and the difference is what
  // this journey found: the native runtime used to finalise a real, non-error
  // assistant row for this turn carrying the FIRST turn's answer verbatim,
  // because the graph's checkpoint thread is the conversation and ADK's
  // executor restores a finished run's checkpoint (empty `pending_nodes`) and
  // executes no node. `EliteaGraphAgent::starting_a_fresh_run` is the fix; the
  // helper prints the received string, so a regression reads as "expected the
  // second marker, got the first".
  await expectStoredAssistantAnswer(page, pipeline.projectId, conversationId, {
    timeout: 180_000,
    contains: second,
    message: 'the second turn stored no answer of its own — a row carrying the FIRST answer means the run was not re-run',
  });

  const afterSecond = await readStoredTranscript(page, pipeline.projectId, conversationId);
  // ACCUMULATED, not replaced. Exactly four: a `>= 4` would also pass on a
  // pane that stored the same turn twice, and the legacy test guards the same
  // upper bound for the same reason.
  expect(afterSecond, 'a second send must append, not replace').toHaveLength(4);
  expect(afterSecond[2]?.content).toContain(second);
  expect(afterSecond[3]?.isError).toBe(false);
  // The FIRST exchange survived the second turn, verbatim — the half a length
  // check cannot give.
  expect(afterSecond[0]?.content).toContain(first);
  expect(afterSecond[1]?.uid, 'the first answer must be the same row, not a rewritten one').toBe(afterFirst[1]?.uid);

  await page.request.delete(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${pipeline.projectId}/${pipeline.pipelineId}`,
  );
});

/*
 * Legacy: `TestPipelineExecutionEdgeCases::test_navigate_away_and_reexecute` —
 * "navigate away from the pipeline and back, then run it again".
 *
 * The claim is that the editor's chat pane is re-armed by a fresh mount rather
 * than by whatever the first visit left in memory. It is worth its own journey
 * because the pane mints its conversation on first interaction and unmounts
 * with the panel (`ChatPanel` renders `renderChat` only while expanded), so
 * "it worked once" says nothing about the second mount — and a second mount
 * that reused the FIRST conversation would be a different defect, which is
 * why the two ids are compared.
 */
test('a pipeline still runs after navigating away from its editor and back', async ({ page }) => {
  test.setTimeout(420_000);
  const name = `${AUTOTEST_PREFIX}nav-${Date.now() % 1_000_000}`;
  const pipeline = await createPipelineWithGraph(page, name, STARTER_TEMPLATE);

  const firstConversation = await openTestChat(page, pipeline);
  const before = marker('before');
  await sendTurn(page, `Reply to this: ${before}`);
  await expectStoredAssistantAnswer(page, pipeline.projectId, firstConversation, {
    timeout: 180_000,
    contains: before,
    message: 'the first turn never answered, so this journey cannot say anything about the second',
  });

  // Away, to a real screen — the list the editor was reached from.
  await page.goto(`${BASE_URL}/app/pipelines/all`);
  await page.waitForURL('**/app/pipelines/all**', { timeout: 20_000 });

  // …and back. `openTestChat` reloads the editor, so this is a cold mount.
  const secondConversation = await openTestChat(page, pipeline);
  expect(
    secondConversation,
    'a fresh mount must mint its own conversation rather than resume the first',
  ).not.toBe(firstConversation);

  const after = marker('after');
  await sendTurn(page, `Reply to this: ${after}`);
  await expectStoredAssistantAnswer(page, pipeline.projectId, secondConversation, {
    timeout: 180_000,
    contains: after,
    message: 'the pipeline stopped being runnable after a navigation away from its editor and back',
  });

  const transcript = await readStoredTranscript(page, pipeline.projectId, secondConversation);
  expect(transcript, 'the second conversation holds its own single exchange').toHaveLength(2);
  expect(transcript[0]?.content).toContain(after);

  await page.request.delete(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${pipeline.projectId}/${pipeline.pipelineId}`,
  );
});

/*
 * Legacy: `TestPipelineExecutionEdgeCases::test_empty_pipeline_execution` —
 * "a pipeline with no nodes should return an error or some response; it should
 * not hang or crash the UI".
 *
 * Stated here as the sharper claim the legacy test gestures at: the turn is
 * ADMITTED, and the refusal comes back as a FINISHED assistant row flagged
 * `metadata.is_error`. Both halves matter and each rules out a different
 * failure. A start that answered 422 would mean the graph never reached the
 * runtime at all; a conversation that never grows an assistant row is the hang
 * the legacy test is about, and it is what a runtime that dropped an
 * uncompilable graph without telling anyone would leave behind.
 *
 * The refusal is expected from the WORKER rather than from the API on purpose:
 * elitea-main runs no server-side pipeline validation — there is no route that
 * could (`pipelines.validation.spec.ts`'s header records the same fact, which
 * is why the EDITOR had to grow its own admission gate). If this ever starts
 * failing on the start POST's status, the finding is that a server-side gate
 * appeared, and the fix is to assert it there.
 */
test('a pipeline the runtime cannot compile fails its turn visibly, rather than hanging', async ({ page }) => {
  test.setTimeout(300_000);
  const name = `${AUTOTEST_PREFIX}empty-${Date.now() % 1_000_000}`;
  const pipeline = await createPipelineWithGraph(page, name, EMPTY_TEMPLATE);
  const conversationId = await openTestChat(page, pipeline);

  await sendTurn(page, 'Reply to this: autotest empty pipeline');

  await expectStoredTurnRefusal(page, pipeline.projectId, conversationId, {
    timeout: 180_000,
    message:
      'a zero-node pipeline neither answered nor stored a refusal — the turn was admitted and then vanished, ' +
      'which is the hang the legacy `test_empty_pipeline_execution` exists to catch',
  });

  // The user's own message is still there. A refusal must not swallow the
  // question: the transcript a person comes back to has to show what they
  // asked as well as that it failed.
  const transcript = await readStoredTranscript(page, pipeline.projectId, conversationId);
  expect(transcript[0]?.role, 'the question must survive a refused turn').toBe('user');
  expect(transcript[0]?.content).toContain('autotest empty pipeline');

  await page.request.delete(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${pipeline.projectId}/${pipeline.pipelineId}`,
  );
});
