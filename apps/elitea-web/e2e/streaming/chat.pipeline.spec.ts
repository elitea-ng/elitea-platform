/**
 * The PIPELINE turn: a pipeline authored on the pipelines create page, given a
 * graph, and then talked to — end to end, on the native Rust runtime.
 *
 * `chat.agent.spec.ts` and `chat.agent-tools.spec.ts` cover the direct agent.
 * A pipeline is a different runtime entirely: `agent_type: 'pipeline'` routes
 * the frozen request to a SECOND assembler in the worker
 * (`NativeRuntimeKind::from_request` in
 * `services/elitea-worker-rust/src/agents/native_runtime.rs` picks
 * `PipelineNativeAgentAssembler` over the direct one, and never falls back),
 * which compiles a YAML graph and runs it through a checkpointed ADK
 * `StateGraph`. Nothing in the agent journeys exercises one line of that.
 *
 * WHAT THIS PINS, and why each half needs the other:
 *
 *  1. THE PIPELINE IS A DIFFERENT ROW, AND THE CREATE PAGE MUST WRITE IT.
 *     `/pipelines/create` renders the SAME `CreateAgentForm` the agents page
 *     does — the only difference is `useCreateApplicationInitialValues(true)`,
 *     which seeds `agentType: 'pipeline'`. A page that dropped that one flag
 *     would create a perfectly ordinary agent, the chat would answer, and
 *     every assertion short of reading `agent_type` back off the server would
 *     pass. So the create response is checked for it before anything else.
 *
 *  2. THE GRAPH LIVES IN `instructions`, NOT IN `pipeline_settings`.
 *     This is the single most confusable fact about pipelines here.
 *     `pipeline_settings` carries the flow editor's node/edge LAYOUT; the
 *     executable document is the YAML in `instructions`
 *     (`pages/pipelines/lib/editPipelineMappers.ts`'s `toVersionDraft`:
 *     "the pipeline graph IS the YAML"), and the worker reads exactly that
 *     (`PipelineDefinition::from_yaml(shell.instructions())`,
 *     `services/elitea-worker-rust/src/agents/pipeline.rs`). A pipeline whose
 *     `instructions` are empty is refused before any node runs —
 *     `bounded_instruction` keeps the non-empty rule for pipelines precisely
 *     because "an empty graph is not an unconstrained agent, it is a pipeline
 *     with nothing to run" (`agents/assembly.rs`).
 *
 *  3. THE CHAT BUTTON MUST CARRY BOTH PARTICIPANTS, AND THE PIPELINE ONE MUST
 *     STILL SAY `application`. A pipeline participant is `entity_name:
 *     'application'` with `entity_settings.agent_type: 'pipeline'` — the
 *     resolver's target join is `target_participant.entity_name =
 *     'application'` (`services/elitea-main/internal/db/queries/agent_chat.sql`),
 *     so a client that persisted the honest-looking `'pipeline'` there would
 *     attach cleanly and 422 on every send. `entity_settings.version_id` is
 *     load-bearing for the same reason it is for agents, and the `user`
 *     participant is the client's to add (the author join is an INNER JOIN).
 *     All three are asserted off the conversation the button created.
 *
 *  4. THE GRAPH ACTUALLY RAN. A stored non-empty answer is NOT enough on its
 *     own here: a pipeline that fell through to the direct-agent assembler
 *     would also answer, using the version's `instructions` as a system prompt
 *     — i.e. it would treat the YAML as prose and reply to it. The
 *     discriminator is the execution trace: a pipeline LLM node publishes its
 *     step under the NODE ID (`attrs.response_metadata.tool_name`), and a
 *     direct agent's thinking step carries `attrs: {}`. Measured on this stack
 *     against the native runtime, which is the only leg this file runs on —
 *     see the skip. (The SDK worker writes the same key for a DIRECT agent but
 *     fills it with the langgraph node name `"agent"`, and writes no trace step
 *     at all for a pipeline; that measurement is in the skip's note.) Hence the
 *     deliberately distinctive node id below — `answer` would have been
 *     indistinguishable from a coincidence.
 *
 * HOW THE PIPELINE IS AUTHORED, and the one half that is NOT through the UI.
 * The shell — name, description, `agent_type` — IS created by filling in the
 * real `/pipelines/create` form, because that is where defect class 1 lives.
 * The GRAPH is then written with the version PUT, sending exactly the body
 * `entities/application-form/model/mutations.ts`'s `toVersionWriteRequest`
 * builds for a pipeline save, because the UI's only path to it is the
 * drag-and-drop flow editor and its YAML pane is a CodeMirror document:
 * `fill()` on a CodeMirror `.cm-content` goes through CM6's own input
 * handling, which re-indents on every newline, so a multi-line YAML document
 * typed into it does not arrive as the document typed. A test that fought that
 * would be asserting an editor's auto-indent, not a pipeline turn. The CHAT
 * half — the button, the participants, the send, the stream — is driven
 * entirely through the browser, which is where the rest of the defects are.
 *
 * Lives in `streaming/` because a pipeline turn needs the full standalone
 * stack (`chat-stream` project, `scripts/chat-stream-e2e.sh`): the plain
 * journeys stack has no runtime plane, no worker and no model.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { AUTOTEST_PREFIX, expectStoredAssistantAnswer, fillComposer } from '../fixtures/api';

const APPLICATIONS_RE = /\/elitea_core\/applications\/prompt_lib\/(\d+)/;
/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/**
 * The node id, held apart from the document because it is an ASSERTION
 * TARGET, not decoration — step 6 reads it back out of the execution trace to
 * prove the graph compiler ran. Deliberately not `answer` (the state key it
 * writes) and not any English word the model might echo, so a match cannot be
 * a coincidence. Constrained by the compiler's own `valid_graph_id`:
 * ASCII alphanumerics plus `_ - . :`, at most 128 bytes
 * (`services/elitea-worker-rust/src/agents/graph/yaml.rs`).
 */
const LLM_NODE_ID = 'autotest_llm_node';

/**
 * The smallest pipeline the native runtime will run: ONE `llm` node, entered
 * directly, transitioning to END.
 *
 * Every line is load-bearing, and the compiler
 * (`services/elitea-worker-rust/src/agents/graph/compiler.rs`) refuses the
 * document outright if any is wrong — `PipelineConfigurationError` is not a
 * warning, it fails the turn at admission:
 *
 *  - `entry_point` must NAME one of the nodes, or "the pipeline entry point
 *    does not name a node".
 *  - `type: llm` is one of eight enabled families (`decision`, `agent`,
 *    `toolkit`/`mcp`, `hitl`, `llm`, `printer`, `router`, `state_modifier`);
 *    anything else is "a node type that is not enabled".
 *  - every key a node reads or writes must be DECLARED in `state`, unless it
 *    is one of the builtins (`input`, `messages`, `output`, `result`,
 *    `elitea_response`, …). `input` and `messages` are builtins — which is
 *    why only `answer` is declared — and `answer` is what the terminal result
 *    policy then hands back as the reply.
 *  - `transition: END` is what makes this node terminal. Without it the graph
 *    has no exit and the run has nothing to return.
 *
 * `input_mapping.task` is how the user's message reaches the node: `input` is
 * the builtin the turn's text is seeded into, and `task` is the LLM node's
 * prompt slot. Drop it and the node runs with no prompt.
 *
 * NO `tool_names`, deliberately: a toolkit alias here has to resolve against
 * the frozen tool snapshot, and an alias that does not is refused
 * (`invalid_pipeline_tool_scope`). Tools are a separate journey; this one is
 * about whether a pipeline runs at all.
 */
const PIPELINE_YAML = `state:
  answer: str
entry_point: ${LLM_NODE_ID}
nodes:
  - id: ${LLM_NODE_ID}
    type: llm
    input_mapping:
      task: {type: variable, value: input}
    input: [messages]
    output: [answer, messages]
    transition: END
`;

test('a pipeline authored on the pipelines create page runs its graph and answers in chat', async ({ page }) => {
  // ── WHY THE PYTHON LEG IS SKIPPED, AND THE HALF THAT WAS WRONG ──────────
  //
  // The old reason said "the SDK worker uses the flow-editor format". It does
  // not, and believing that would send anyone porting this journey to the
  // wrong field. `pipeline_settings` appears NOWHERE in the pinned elitea-sdk;
  // `client.py` reads `data['agent_type']`, `assistant.py`'s `runnable()`
  // branches to `pipeline()` on it, and `pipeline()` hands
  // `self.prompt` — which is `data['instructions']` — to `create_graph` as
  // `yaml_schema`. That is the SAME field, parsed as the SAME YAML, as
  // `PipelineDefinition::from_yaml(shell.instructions())`. The "IS_ERROR"
  // half of the old note was real; its explanation was not.
  //
  // RE-MEASURED against a python-worker standalone stack, twice, and the two
  // compilers disagree in two places instead:
  //
  //  1. THE `state` BLOCK'S SCHEMA. The document below declares `answer: str`.
  //     The native compiler admits that (`RawStateType::Name`) and equally
  //     admits `answer: {type: str}` (`RawStateType::Descriptor`,
  //     services/elitea-worker-rust/src/agents/graph/compiler.rs). The SDK's
  //     `set_defaults` (`langraph_agent.py`) assumes the descriptor form
  //     unconditionally — it does `v['value'] = …` on every entry — so a bare
  //     type NAME raises `TypeError` before any node runs. Observed:
  //     `{"event":"agent_execution_internal_failure","exception_name":
  //     "TypeError",…"function":"set_defaults"}` and an empty `is_error` row.
  //  2. THE TRACE, WHICH IS THIS SPEC'S ONLY PROOF THE GRAPH RAN. Rewritten to
  //     `answer: {type: str}` — the one form BOTH compilers accept — the
  //     python leg compiles the graph and STORES A REAL ANSWER (step 5 green).
  //     It then fails step 6 with an empty array: the SDK's pipeline run
  //     publishes no execution-trace step at all. Measured directly:
  //     `p_<project>.chat_message_trace_step` held zero rows for either
  //     pipeline turn, while every SDK DIRECT-agent turn in the same project
  //     wrote `thinking_step` rows carrying `response_metadata.tool_name:
  //     "agent"` — the langgraph node name, never the pipeline node id.
  //
  // Point 4 in the header is why that matters: without a step naming the node,
  // a python-leg run of this file could not tell a compiled graph from a
  // fall-through to the direct-agent assembler, which answers this YAML as
  // prose. A spec that cannot make that distinction is not this spec.
  //
  // So a python leg needs BOTH: the descriptor `state` form here (harmless on
  // the native leg, which accepts it), and a discriminator the SDK actually
  // emits. It is its own journey, not a flag on this one.
  //
  // E2E_WORKER comes from chat-stream-e2e.sh; local default = the
  // long-lived native-runtime dev stack.
  test.skip(
    (process.env['E2E_WORKER'] ?? 'rust') !== 'rust',
    "the SDK compiles the same `instructions` YAML but rejects a bare `state` type name, and its " +
      'pipeline run emits no trace step naming the node this spec reads',
  );
  // A whole pipeline turn — create, graph, admission, graph compile, a model
  // call and the stream back. Every wait below is bounded well under this, so
  // a real hang fails on its own step rather than on the clock.
  test.setTimeout(300_000);

  // Capped at 32 characters by the form's own `maxLength`, exactly as
  // `createAgentThroughForm` documents: `fill()` respects it, so a longer name
  // is silently truncated and every later lookup by it finds nothing.
  const name = `${AUTOTEST_PREFIX}pipe-${Date.now() % 1_000_000}`;

  // ── 1. Author the pipeline shell on the real create page ───────────────
  // Armed BEFORE the navigation: the response is what carries the project id,
  // the version and the `agent_type`, so the assertions below read what the
  // server actually wrote rather than what this test constructed.
  const created = page.waitForResponse(
    (r) => APPLICATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await page.goto(`${BASE_URL}/app/pipelines/create`);
  // The form is `CreateAgentForm`, so its fields carry the agent testids; only
  // the save button is the pipelines page's own.
  await expect(page.getByTestId('agent-name-input')).toBeVisible({ timeout: 30_000 });
  await page.getByTestId('agent-name-input').fill(name);
  await page.getByTestId('agent-description-input').fill(`${name} description`);
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('pipeline-save-button').click();

  const response = await created;
  expect(
    response.status(),
    `the pipeline must be created: ${(await response.text()).slice(0, 300)}`,
  ).toBe(201);
  const projectId = APPLICATIONS_RE.exec(new URL(response.url()).pathname)?.[1] ?? '';
  expect(projectId, 'the pipeline must belong to a project').not.toBe('');
  const body = (await response.json()) as {
    id?: string;
    version_details?: { id?: string; name?: string; agent_type?: string };
  };
  const pipelineId = body.id ?? '';
  expect(pipelineId, 'the created pipeline must carry an id').toMatch(/^\d+$/);
  const versionId = String(body.version_details?.id ?? '');
  expect(versionId, 'the created pipeline must carry a version, or the resolver joins nothing').not.toBe('');

  // Defect class 1, asserted at the point it is decided. Without this the rest
  // of this file would pass just as well against a plain agent.
  expect(
    body.version_details?.agent_type,
    'the pipelines create page must store a PIPELINE row — an agent here means the page dropped `forPipeline`',
  ).toBe('pipeline');

  // ── 2. Give it a graph ─────────────────────────────────────────────────
  // The version PUT with the body `toVersionWriteRequest` builds for a
  // pipeline save — see this file's header for why the graph does not come
  // through the flow editor. `pipeline_settings` is the empty layout
  // `useCreateApplicationInitialValues(true)` seeds; the runtime never reads
  // it, `instructions` is the executable half.
  const saved = await page.request.put(
    `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${projectId}/${pipelineId}/${versionId}`,
    {
      data: {
        name: body.version_details?.name ?? 'latest',
        agent_type: 'pipeline',
        instructions: PIPELINE_YAML,
        conversation_starters: [],
        variables: [],
        meta: { step_limit: 25, internal_tools: [] },
        pipeline_settings: { nodes: [], edges: [] },
      },
    },
  );
  expect(
    saved.status(),
    `the graph must reach the version row: ${(await saved.text()).slice(0, 300)}`,
  ).toBeLessThan(300);

  // Read back what the SERVER stored, not what was sent. `instructions` is the
  // exact string the compiler parses, so a store that trimmed, re-encoded or
  // dropped it fails the turn later with a message about a malformed graph
  // rather than about the write that mangled it.
  const stored = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${pipelineId}`,
  );
  const detail = (await stored.json()) as { version_details?: { instructions?: string; agent_type?: string } };
  expect(
    detail.version_details?.instructions,
    'the stored instructions ARE the graph — the compiler parses this string verbatim',
  ).toBe(PIPELINE_YAML);
  expect(detail.version_details?.agent_type, 'the save must not demote the pipeline to an agent').toBe('pipeline');

  // The save landed this browser on the pipeline's edit page; reload so the
  // Chat button below reads the version this test just wrote rather than the
  // one the page loaded before the PUT.
  await page.reload();

  // ── 3. Chat: both participants, and the pipeline still an `application` ─
  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  // Disabled until the current-author read resolves — the button refuses to
  // create a conversation it cannot name the user in, because that is exactly
  // the conversation that attaches cleanly and 422s on every send.
  await expect(page.getByTestId('chat-with-pipeline-button')).toBeEnabled({ timeout: 30_000 });
  await page.getByTestId('chat-with-pipeline-button').click();
  const conversation = (await (await conversationCreated).json()) as { id?: string | number };
  const conversationId = String(conversation.id ?? '');
  expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}$`), { timeout: 30_000 });

  // Through the CONVERSATION detail, because `.../participants/prompt_lib/...`
  // is write-only and answers 405 to a GET; the participant list is a member
  // of the conversation's own detail response.
  const conversationRead = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`,
  );
  const conversationBody = (await conversationRead.json()) as {
    participants?: readonly {
      entity_name?: string;
      entity_settings?: { version_id?: string | number; agent_type?: string };
    }[];
  };
  const participants = conversationBody.participants ?? [];
  expect(
    participants.map((p) => p.entity_name),
    'the resolver joins the author through the user mapping — without it every send is refused',
  ).toContain('user');
  // `'application'`, NOT `'pipeline'`: see defect class 3 in the header. This
  // find() is itself the assertion — a participant persisted under the other
  // spelling makes it `undefined` and the two expects below fail on it.
  const pipelineParticipant = participants.find((p) => p.entity_name === 'application');
  expect(
    pipelineParticipant,
    'a pipeline participant must still be named `application` — the resolver joins on that literal',
  ).toBeDefined();
  expect(
    String(pipelineParticipant?.entity_settings?.version_id ?? ''),
    'the participant must name the pipeline version, or the resolver joins nothing',
  ).toBe(versionId);
  expect(
    pipelineParticipant?.entity_settings?.agent_type,
    'the pipeline discriminator rides in entity_settings, which is what routes the worker to the graph assembler',
  ).toBe('pipeline');

  // ── 4. Send, and require the turn to be ADMITTED ───────────────────────
  // The composer is settled by `fillComposer` — which retries the fill,
  // because this pane discards one that lands while it is still resolving its
  // conversation, and run 33812152063 spent this test's whole budget clicking
  // at the Send control that absence leaves unrendered. The response budget is
  // armed only once that has resolved; see the helper for both halves.
  const sendButton = await fillComposer(page, `autotest pipeline ${Date.now()}`);
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  await sendButton.click();

  const startResponse = await started;
  // Spelled out because 422 is the status every admission refusal produces and
  // its body is the sentence a maintainer will search for. A malformed graph,
  // an empty `instructions`, a participant under the wrong `entity_name` and
  // an unadmitted internal tool all land here and all say the same thing.
  expect(
    startResponse.status(),
    `the pipeline turn was refused: ${(await startResponse.text()).slice(0, 400)}`,
  ).toBe(200);

  // ── 5. A stored answer, not an error card ──────────────────────────────
  // The STORED reply, not the on-screen bubble — the `IS_ERROR` discrimination
  // and why nothing on screen can stand in for it are in
  // `expectStoredAssistantAnswer`. No `contains`: the graph runs a real model,
  // which echoes nothing.
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 120_000,
    message: 'the pipeline turn stored no answer — the graph was admitted and then failed to produce a result',
  });

  // ── 6. The GRAPH ran — not the direct-agent assembler ──────────────────
  // The half a stored answer cannot give (header, defect class 4). A pipeline
  // node publishes its execution step under the NODE ID; a direct agent's
  // thinking step carries an empty `attrs`. So a fallback to the ordinary
  // runtime — which would happily treat this YAML as a system prompt and
  // answer anyway — cannot produce this row.
  //
  // POLLED, not read once: the trace projection is written from the same
  // frames the stream carries and finishes after the reply text does, so a
  // single read races the last write. Its failure line still says the graph
  // never ran, which is the real defect it guards.
  await expect
    .poll(
      async () => {
        const traces = await page.request.get(
          `${BASE_URL}/api/v2/elitea_core/message_traces/prompt_lib/${projectId}/${conversationId}`,
        );
        if (!traces.ok()) return [];
        const traceBody = (await traces.json()) as {
          rows?: readonly { attrs?: { response_metadata?: { tool_name?: string } } }[];
        };
        return (traceBody.rows ?? []).map((row) => row.attrs?.response_metadata?.tool_name ?? '');
      },
      {
        timeout: 60_000,
        message:
          `no execution step named the pipeline node \`${LLM_NODE_ID}\` — the turn answered, but the graph ` +
          'compiler never ran it, which is what a silent fall-through to the direct-agent runtime looks like',
      },
    )
    .toContain(LLM_NODE_ID);

  // Cleanup is best-effort and deliberately last: a failure above should leave
  // the pipeline in place for inspection.
  await page.request.delete(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${pipelineId}`,
  );
});
