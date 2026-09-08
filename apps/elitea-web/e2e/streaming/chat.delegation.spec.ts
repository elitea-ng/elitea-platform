/**
 * DELEGATION, AND WHO IS ON THE OTHER END: an agent handing work to its
 * sub-agent, a two-level chain doing it twice, and a PUBLISHED agent answering
 * in a chat beside a private one.
 *
 * ── What this ports ────────────────────────────────────────────────────────
 *
 * The legacy API suite's sub-agent and published-agent prediction cases: a
 * parent that delegates and records the sub-agent's answer, a two-level chain
 * A→B→C that runs end to end, chatting with a published agent, and a
 * conversation holding a private AND a published agent where the one addressed
 * is the one that replies.
 *
 * ── Why the legacy versions could not be ported as they stand ──────────────
 *
 * Every one of them depends on a model CHOOSING to delegate: the legacy agents
 * carry instructions like "never answer directly, always call the sub-agent",
 * and the assertions then read the trace for the call. The offline mock makes
 * no such choice — `chat.nested-agent.spec.ts` says so in as many words and
 * deliberately asserts admission only, because an assertion on the child being
 * called went pass, pass, fail on three consecutive runs against a real model.
 *
 * So the choice is taken out of the model. `deploy/mock-llm/server.py`'s
 * `[[mock:call_tool <toolName> <arguments>]]` marker scripts the call the model
 * emits, arguments included, and the runtime then does the delegating for real:
 * it resolves the reference, compiles the child, runs it as a turn of its own
 * against the same mock, feeds the result back, and the parent's continuation
 * quotes it. Nothing about the hop is faked — only the parent model's decision
 * to make it is. That is what turns "the model usually delegates" into an
 * assertion that either holds or does not.
 *
 * The task the marker carries is what makes the CHILD's answer identifiable: it
 * is a per-run marker, the child echoes it (the mock echoes the last user
 * message), the parent quotes the tool result verbatim, and the marker can only
 * have reached the parent's stored reply by travelling down and back. A
 * two-level chain nests one marker inside another's task, so B's own prompt is
 * the instruction to call C.
 *
 * ── Chatting with a published agent from ANOTHER project ───────────────────
 *
 * The legacy platform lets a conversation in one project hold a participant
 * that names an agent in the PUBLIC project (`entity_meta.project_id` crosses
 * the boundary), and that is how a marketplace agent is chatted with. This
 * platform used to refuse it by construction: the turn resolver runs inside the
 * conversation's own tenant schema and demanded that the participant's project
 * be the conversation's own, so a participant naming the catalogue's copy
 * resolved no rows and every send answered 422 — while the participant add
 * itself succeeded, so the conversation looked healthy and only its turns were
 * dead.
 *
 * `ResolveCurrentApplicationTurn`
 * (services/elitea-main/internal/db/queries/agent_chat.sql) now admits one
 * foreign project, the public one, and only for a version that is `published`;
 * the version is then read out of the catalogue project's own schema. The third
 * case below runs both halves in one conversation: the author's published clone
 * answering in its own project, and the CATALOGUE twin answering across the
 * project line.
 *
 * The counter-half — an agent private to a THIRD project stays refused, and a
 * DRAFT in the catalogue project stays refused — needs projects this rig does
 * not have, and is pinned next to the resolver instead
 * (agent_catalogue_turn_postgres_integration_test.go).
 *
 * Lives in `streaming/` because every case runs real turns — a parent turn, a
 * child turn and, for the chain, a grandchild turn — which need the FULL
 * standalone stack (`chat-stream` project, `scripts/chat-stream-e2e.sh`).
 */
import { expect, test, type Page } from '@playwright/test';
import { randomUUID } from 'node:crypto';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  MOCK_CALL_TOOL_SENTINEL,
  agentAsToolName,
  callToolWithArgumentsPrompt,
  clearMockLlmJournal,
  createAgentWithVersion,
  expectStoredAssistantAnswer,
  readCallerIdentity,
  readCallerPersonalProjectId,
  readCatalogue,
  readApplicationVersions,
  resolveCatalogueProjectId,
  readMockLlmJournal,
  readStoredMessageGroups,
  readStoredTranscript,
} from '../fixtures/api';

const API = `${BASE_URL}/api/v2`;
const APPLICATION_CONTRACT = 'agent.execute.application.v1';

/** The model every agent here is pinned to — the wire name, as `seed-llm` writes it. */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/** A marker unique to one turn, which the mock echoes back verbatim. */
function marker(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}${Date.now() % 1_000_000}`;
}

interface Agent {
  readonly id: string;
  readonly versionId: string;
  readonly name: string;
}

/** The function name the runtime offers this agent under, on this leg. */
function toolNameOf(agent: Agent): string {
  return agentAsToolName({ agentId: agent.id, versionId: agent.versionId, name: agent.name });
}

/**
 * An agent whose version is publishable and pinned to the deterministic model.
 *
 * The instructions are long enough to clear the pre-publish quality gate (a
 * version under 50 characters raises a CRITICAL issue and the publish is
 * refused for a reason that has nothing to do with the case), and the model is
 * pinned WITHOUT `model_project_id`: naming a project there makes the publish
 * hard-check compare it against the catalogue project and refuse with
 * `llm_not_shared`, which is a different journey's subject.
 */
async function createAgent(page: Page, projectId: string, name: string): Promise<Agent> {
  const created = await createAgentWithVersion(
    page.request,
    name,
    {
      instructions:
        'You are a delegation fixture. Carry out the task you are given, or hand it to the ' +
        'saved agent you have been attached to, and report exactly what came back.',
      welcomeMessage: 'Give me something to delegate.',
      conversationStarters: ['Delegate this task.'],
      model: { modelName: MOCK_MODEL },
      meta: { step_limit: 25, internal_tools: [] },
    },
    projectId,
    `${AUTOTEST_PREFIX}delegation fixture agent for the streaming suite`,
  );
  return { id: created.id, versionId: created.versionId, name };
}

/**
 * Attach `child` to `parent`, and read the reference back off the PARENT.
 *
 * The URL names the CHILD and the body names the PARENT — the direction the
 * agent page's own picker sends (`useAgentPipelineAssociation`), and the one
 * `chat.agent-tools.spec.ts` records as answering 200 while writing nothing.
 * The read-back is what makes this a write: the freeze builds the runtime
 * reference from that projection, and a relation stored against the wrong
 * version leaves the parent's list empty behind a 2xx.
 */
async function attachChild(
  page: Page,
  projectId: string,
  parent: Agent,
  child: Agent,
): Promise<void> {
  const attached = await page.request.patch(
    `${API}/elitea_core/application_relation/prompt_lib/${projectId}/${child.id}/${child.versionId}`,
    {
      data: {
        application_id: Number(parent.id),
        version_id: Number(parent.versionId),
        has_relation: true,
      },
    },
  );
  expect(
    attached.status(),
    `the child must attach to the parent version: ${(await attached.text()).slice(0, 300)}`,
  ).toBeLessThan(300);

  const stored = await page.request.get(
    `${API}/elitea_core/application/prompt_lib/${projectId}/${parent.id}`,
  );
  const detail = (await stored.json()) as {
    version_details?: {
      tools?: readonly {
        type?: string;
        name?: string;
        config?: { application_id?: number; application_version_id?: number };
      }[];
    };
  };
  const reference = (detail.version_details?.tools ?? []).find(
    (tool) => tool.type === 'application' && String(tool.config?.application_id ?? '') === child.id,
  );
  expect(
    reference,
    `the parent version carries no \`application\` reference to ${child.name} — the attach was a no-op`,
  ).toBeDefined();
  expect(
    String(reference?.config?.application_version_id ?? ''),
    'the reference must name the child VERSION the runtime will compile',
  ).toBe(child.versionId);
}

interface CreatedConversation {
  readonly id: string;
  readonly uuid: string;
}

async function createConversation(
  page: Page,
  projectId: string,
  name: string,
): Promise<CreatedConversation> {
  const created = await page.request.post(
    `${API}/elitea_core/conversations/prompt_lib/${projectId}`,
    { data: { name, is_private: true } },
  );
  expect(
    created.status(),
    `the conversation must be created: ${(await created.text()).slice(0, 300)}`,
  ).toBe(201);
  const body = (await created.json()) as { id?: unknown; uuid?: unknown };
  const conversation = { id: String(body.id ?? ''), uuid: String(body.uuid ?? '') };
  expect(conversation.uuid, 'the message route addresses a conversation by uuid').not.toBe('');
  return conversation;
}

interface StoredParticipant {
  readonly id: string;
  readonly entityName: string;
  readonly entityMeta: Record<string, unknown>;
}

async function addParticipants(
  page: Page,
  projectId: string,
  conversationId: string,
  entries: readonly Record<string, unknown>[],
): Promise<readonly StoredParticipant[]> {
  const added = await page.request.post(
    `${API}/elitea_core/participants/prompt_lib/${projectId}/${conversationId}`,
    { data: entries },
  );
  expect(
    added.status(),
    `the participants must be added: ${(await added.text()).slice(0, 300)}`,
  ).toBe(200);
  const rows = (await added.json()) as readonly {
    id?: unknown;
    entity_name?: unknown;
    entity_meta?: unknown;
  }[];
  return (Array.isArray(rows) ? rows : []).map((row) => ({
    id: String(row.id ?? ''),
    entityName: String(row.entity_name ?? ''),
    entityMeta:
      typeof row.entity_meta === 'object' && row.entity_meta !== null
        ? (row.entity_meta as Record<string, unknown>)
        : {},
  }));
}

/** The participant body for one agent, in the shape the resolver joins on. */
function agentParticipantBody(
  agent: { readonly id: string; readonly versionId: string; readonly name: string },
  projectId: string,
): Record<string, unknown> {
  return {
    entity_name: 'application',
    entity_meta: { id: agent.id, project_id: Number(projectId), name: agent.name },
    entity_settings: { version_id: agent.versionId },
  };
}

/** The `user` participant the resolver's author join needs. */
function userParticipantBody(userId: string): Record<string, unknown> {
  return { entity_name: 'user', entity_meta: { id: Number(userId) } };
}

/** POST one agent turn. Returns the raw response so a caller can assert a refusal. */
async function postTurn(
  page: Page,
  input: {
    readonly projectId: string;
    readonly conversation: CreatedConversation;
    readonly participantId: string;
    readonly prompt: string;
  },
) {
  return page.request.post(
    `${API}/elitea_core/messages/prompt_lib/${input.projectId}/${input.conversation.uuid}` +
      `?execution_contract=${APPLICATION_CONTRACT}`,
    {
      data: {
        project_id: Number(input.projectId),
        conversation_uuid: input.conversation.uuid,
        participant_id: Number(input.participantId),
        question_id: randomUUID(),
        interaction_uuid: randomUUID(),
        payload: { user_input: input.prompt },
      },
    },
  );
}

/** POST one agent turn and require it to be ADMITTED. */
async function startTurn(
  page: Page,
  input: {
    readonly projectId: string;
    readonly conversation: CreatedConversation;
    readonly participantId: string;
    readonly prompt: string;
  },
): Promise<void> {
  const started = await postTurn(page, input);
  expect(
    started.status(),
    `the turn was refused: ${(await started.text()).slice(0, 400)}`,
  ).toBe(200);
}

/** Every `tool_call` trace step of a conversation, with its heavy fields. */
async function readToolCallSteps(
  page: Page,
  projectId: string,
  conversationId: string,
): Promise<readonly { readonly toolName: string; readonly toolOutput: string; readonly isError: boolean }[]> {
  const list = await page.request.get(
    `${API}/elitea_core/message_traces/prompt_lib/${projectId}/${conversationId}?kind=tool_call`,
  );
  if (!list.ok()) return [];
  const rows =
    ((await list.json()) as { rows?: readonly { id?: unknown; message_group_id?: unknown }[] }).rows ?? [];
  const steps: { toolName: string; toolOutput: string; isError: boolean }[] = [];
  for (const row of rows) {
    const detail = await page.request.get(
      `${API}/elitea_core/message_trace/prompt_lib/${projectId}/${String(row.id ?? '')}` +
        `?message_group_id=${String(row.message_group_id ?? '')}`,
    );
    if (!detail.ok()) continue;
    const body = (await detail.json()) as {
      tool_name?: unknown;
      tool_output?: unknown;
      is_error?: unknown;
    };
    steps.push({
      toolName: String(body.tool_name ?? ''),
      toolOutput: typeof body.tool_output === 'string' ? body.tool_output : '',
      isError: body.is_error === true,
    });
  }
  return steps;
}

/** Delete an agent, best effort. Parents before children: the relation goes with the parent. */
async function deleteAgents(page: Page, projectId: string, ...agents: readonly Agent[]): Promise<void> {
  for (const agent of agents) {
    await page.request.delete(`${API}/elitea_core/application/prompt_lib/${projectId}/${agent.id}`);
  }
}

/* ────────────────────────────────────────────────────────────────────────── */

test('a parent agent delegates to its sub-agent, and the sub-agent’s own answer is what it reports', async ({
  page,
}) => {
  // Two agents, a relation, an admission, the parent's model call, the CHILD's
  // whole turn, and the parent's continuation.
  test.setTimeout(420_000);

  await clearMockLlmJournal(page);
  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project, and the setup waits for one').not.toBe('');
  const caller = await readCallerIdentity(page.request);

  const stamp = Date.now() % 1_000_000;
  const child = await createAgent(page, projectId, `${AUTOTEST_PREFIX}sub-${stamp}`);
  const parent = await createAgent(page, projectId, `${AUTOTEST_PREFIX}orc-${stamp}`);
  const conversation = await createConversation(page, projectId, marker('delconv'));

  try {
    await attachChild(page, projectId, parent, child);

    const participants = await addParticipants(page, projectId, conversation.id, [
      userParticipantBody(caller.id),
      agentParticipantBody(parent, projectId),
    ]);
    const orchestrator = participants.find((participant) => participant.entityName === 'application');
    expect(orchestrator, 'the parent must be a participant of the conversation').toBeDefined();

    // The name the runtime offers the child under. Both runtimes derive it from
    // the SAME stored reference and spell it differently, so the marker names
    // the one this leg uses; the offer assertion below prints what was really
    // sent, so a naming change reads as a naming change.
    const childTool = toolNameOf(child);
    const task = `${marker('subtask')} report ready`;
    await startTurn(page, {
      projectId,
      conversation,
      participantId: orchestrator?.id ?? '',
      prompt: callToolWithArgumentsPrompt(childTool, { task }, 'delegate this and quote the answer'),
    });

    // The parent's continuation ends on the mock's sentinel, which is the only
    // safe settle signal: a tool result is long and the stored row is readable
    // while it is still being written, so a poll on anything nearer the front
    // returns a prefix.
    await expectStoredAssistantAnswer(page, projectId, conversation.id, {
      timeout: 300_000,
      contains: MOCK_CALL_TOOL_SENTINEL,
      message:
        'the parent stored no completed answer — a delegating turn that fails mid-hop lands here ' +
        'as an is_error row with empty content, which is what an unresolvable child reference does',
    });

    const transcript = await readStoredTranscript(page, projectId, conversation.id);
    const reply = transcript.filter((row) => row.role === 'assistant').at(-1)?.content ?? '';
    expect(
      reply,
      'the parent must report the result of the SUB-AGENT tool it was told to call',
    ).toContain(childTool);
    // THE HOP, in one assertion. The task marker exists only inside the
    // arguments of the scripted call; the mock's continuation script quotes the
    // TOOL RESULT and never the prompt, so this text can only have arrived by
    // travelling into the child, being echoed by the child's own model call,
    // and coming back as the tool's result.
    expect(
      reply,
      'the sub-agent’s own answer is not in the parent’s reply — the call was made and its result discarded, ' +
        'or the child never ran',
    ).toContain(task);
    expect(
      transcript.filter((row) => row.isError),
      'no row may be flagged is_error — a refused turn is stored as an assistant row and reads like an answer',
    ).toEqual([]);

    // The OFFER, server-side: these are the function names the runtime put in
    // the parent's model request. A relation that was written but never
    // compiled contributes none, and the turn would then have answered a plain
    // echo — which the assertions above would blame on the mock.
    const journal = await readMockLlmJournal(page);
    const offer = journal.find((entry) => entry.mode === 'call_tool');
    expect(
      offer,
      'the mock never took the call_tool branch — the marker did not reach the parent’s model call',
    ).toBeDefined();
    expect(
      offer?.tools ?? [],
      'the child was never offered to the parent as a callable tool',
    ).toContain(childTool);
    expect(
      journal.map((entry) => entry.mode),
      'no continuation carrying a tool result — the parent asked for the delegation and it never happened',
    ).toContain('call_tool_resumed');

    // ── The tool-output hygiene rule, with a sub-agent in the loop ────────
    //
    // The legacy regression: with a sub-agent present, the tool result was
    // stored as the repr of the runtime's own message object rather than as its
    // content, so the reader saw `content='…' tool_call_id='call_…'` instead of
    // an answer. Asserted on the stored trace step, which is where the worker
    // writes it (`_trace_text` falls back to `str(value)` for anything it
    // cannot serialise), and on the reply the reader is shown.
    await expect
      .poll(async () => (await readToolCallSteps(page, projectId, conversation.id)).map((step) => step.toolName), {
        timeout: 60_000,
        message:
          'the delegation left no tool_call trace step — the conversation would redraw with no ' +
          'record of the sub-agent that ran',
      })
      .toContain(childTool);
    for (const step of await readToolCallSteps(page, projectId, conversation.id)) {
      for (const repr of ['tool_call_id=', 'ToolMessage(']) {
        expect(
          step.toolOutput.includes(repr),
          `the \`${step.toolName}\` step's tool_output carries \`${repr}\` — the sub-agent's result ` +
            `was recorded as a runtime object instead of as its content: ${step.toolOutput.slice(0, 300)}`,
        ).toBe(false);
      }
    }
  } finally {
    await page.request.delete(
      `${API}/elitea_core/conversation/prompt_lib/${projectId}/${conversation.id}`,
    );
    await deleteAgents(page, projectId, parent, child);
  }
});

test('a two-level chain delegates twice, and the grandchild’s answer comes back through both hops', async ({
  page,
}) => {
  // Three agents, two relations and THREE turns' worth of model calls: the
  // parent's two, the child's two and the grandchild's one.
  test.setTimeout(600_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);

  const stamp = Date.now() % 1_000_000;
  const grandchild = await createAgent(page, projectId, `${AUTOTEST_PREFIX}gc-${stamp}`);
  const child = await createAgent(page, projectId, `${AUTOTEST_PREFIX}mid-${stamp}`);
  const parent = await createAgent(page, projectId, `${AUTOTEST_PREFIX}top-${stamp}`);
  const conversation = await createConversation(page, projectId, marker('chainconv'));

  try {
    await attachChild(page, projectId, child, grandchild);
    await attachChild(page, projectId, parent, child);

    const participants = await addParticipants(page, projectId, conversation.id, [
      userParticipantBody(caller.id),
      agentParticipantBody(parent, projectId),
    ]);
    const orchestrator = participants.find((participant) => participant.entityName === 'application');
    expect(orchestrator, 'the parent must be a participant of the conversation').toBeDefined();

    // The inner marker rides inside the outer one's `task`, so the CHILD's own
    // prompt is the instruction to call the GRANDCHILD. The mock reads the
    // marker that closes last, so the nesting survives — see `_marker_body`.
    const childTool = toolNameOf(child);
    const grandchildTool = toolNameOf(grandchild);
    const deepTask = `${marker('deeptask')} report ready`;
    const middleTask = callToolWithArgumentsPrompt(
      grandchildTool,
      { task: deepTask },
      'delegate this and quote the answer',
    );
    await startTurn(page, {
      projectId,
      conversation,
      participantId: orchestrator?.id ?? '',
      prompt: callToolWithArgumentsPrompt(
        childTool,
        { task: middleTask },
        'delegate this and quote the answer',
      ),
    });

    await expectStoredAssistantAnswer(page, projectId, conversation.id, {
      timeout: 420_000,
      contains: MOCK_CALL_TOOL_SENTINEL,
      message:
        'the two-level chain stored no completed answer — a hop that fails lands here as an ' +
        'is_error row, and the nesting caps refuse the third tier the same way',
    });

    const reply = (await readStoredTranscript(page, projectId, conversation.id))
      .filter((row) => row.role === 'assistant')
      .at(-1)?.content ?? '';
    // The GRANDCHILD's task marker, in the top-level reply. It was written into
    // the innermost call's arguments and can only be here if the parent called
    // the child, the child called the grandchild, the grandchild answered, and
    // both results were carried back up.
    expect(
      reply,
      'the grandchild’s answer never reached the top of the chain — one of the two hops did not happen',
    ).toContain(deepTask);

    // Both hops, in the store. The runtime flattens a nested call into the same
    // conversation's trace, which is what makes the grandchild's step readable
    // from the top-level conversation at all.
    await expect
      .poll(async () => (await readToolCallSteps(page, projectId, conversation.id)).map((step) => step.toolName), {
        timeout: 90_000,
        message:
          'the conversation’s trace does not name both hops — a chain that ran only its first hop ' +
          'still answers, and answers in the same shape',
      })
      .toEqual(expect.arrayContaining([childTool, grandchildTool]));

    expect(
      (await readStoredTranscript(page, projectId, conversation.id)).filter((row) => row.isError),
      'no row may be flagged is_error',
    ).toEqual([]);
  } finally {
    await page.request.delete(
      `${API}/elitea_core/conversation/prompt_lib/${projectId}/${conversation.id}`,
    );
    await deleteAgents(page, projectId, parent, child, grandchild);
  }
});

test('a published agent answers in a mixed conversation, from its own project and from the catalogue', async ({
  page,
}) => {
  // A publish, then THREE turns in one conversation: the private agent, the
  // author's published clone, and the catalogue twin across the project line.
  test.setTimeout(900_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project').not.toBe('');
  const catalogueProjectId = await resolveCatalogueProjectId(page.request);
  expect(
    projectId,
    'the author project must NOT be the catalogue project, or the publish writes no twin and ' +
      'the two halves of a publish cannot be told apart',
  ).not.toBe(catalogueProjectId);
  const caller = await readCallerIdentity(page.request);

  const stamp = Date.now() % 1_000_000;
  const privateAgent = await createAgent(page, projectId, `${AUTOTEST_PREFIX}priv-${stamp}`);
  const authored = await createAgent(page, projectId, `${AUTOTEST_PREFIX}pub-${stamp}`);
  const conversation = await createConversation(page, projectId, marker('mixedconv'));
  let publishedVersionId = '';
  let catalogueAgentId = '';
  let catalogueVersionId = '';

  try {
    // ── Publish, and let the catalogue prove the twin was written ─────────
    const releaseName = `rel-${stamp}`;
    const published = await page.request.post(
      `${API}/elitea_core/publish/prompt_lib/${projectId}/${authored.versionId}`,
      { data: { version_name: releaseName } },
    );
    expect(
      published.status(),
      `the agent must be publishable: ${(await published.text()).slice(0, 400)}`,
    ).toBe(200);
    const publishBody = (await published.json()) as Record<string, unknown>;
    publishedVersionId = String(publishBody['public_version_id'] ?? '');
    catalogueAgentId = String(publishBody['catalog_agent_id'] ?? '');
    catalogueVersionId = String(publishBody['catalog_version_id'] ?? '');
    expect(publishedVersionId, 'the publish must clone the version it published').not.toBe(
      authored.versionId,
    );
    expect(
      catalogueAgentId,
      `the publish wrote no catalogue twin: ${JSON.stringify(publishBody).slice(0, 300)}`,
    ).not.toBe('');
    const clone = (await readApplicationVersions(page.request, authored.id, projectId)).find(
      (version) => version.id === publishedVersionId,
    );
    expect(clone?.status, 'the clone the conversation will address must be the PUBLISHED one').toBe(
      'published',
    );
    // Polled: the twin is written in the publish transaction and the catalogue
    // read is a separate connection.
    await expect
      .poll(async () => (await readCatalogue(page.request)).some((row) => row.name === authored.name), {
        timeout: 30_000,
        message: 'the published agent never appeared in the catalogue — the cross-schema twin was not written',
      })
      .toBe(true);

    // ── One conversation, both agents ─────────────────────────────────────
    const participants = await addParticipants(page, projectId, conversation.id, [
      userParticipantBody(caller.id),
      agentParticipantBody(privateAgent, projectId),
      // The PUBLISHED clone, addressed by the version the publish created.
      agentParticipantBody(
        { id: authored.id, versionId: publishedVersionId, name: authored.name },
        projectId,
      ),
    ]);
    const privateParticipant = participants.find(
      (participant) => String(participant.entityMeta['id'] ?? '') === privateAgent.id,
    );
    const publishedParticipant = participants.find(
      (participant) => String(participant.entityMeta['id'] ?? '') === authored.id,
    );
    expect(privateParticipant, 'the private agent must be in the conversation').toBeDefined();
    expect(publishedParticipant, 'the published agent must be in the conversation').toBeDefined();

    // ── Address the PRIVATE one ───────────────────────────────────────────
    const privatePrompt = `${marker('toprivate')} which of you answered?`;
    await startTurn(page, {
      projectId,
      conversation,
      participantId: privateParticipant?.id ?? '',
      prompt: privatePrompt,
    });
    await expectStoredAssistantAnswer(page, projectId, conversation.id, {
      timeout: 300_000,
      contains: privatePrompt,
      message: 'the private agent stored no answer in the mixed conversation',
    });
    const afterPrivate = await readStoredMessageGroups(page, projectId, conversation.id);
    expect(
      afterPrivate.at(-1)?.authorParticipantId,
      'the reply came from a participant the turn did not address — in a mixed conversation the ' +
        'addressed agent is the only one that may answer',
    ).toBe(privateParticipant?.id);

    // ── …then the PUBLISHED one, in the same conversation ─────────────────
    const publishedPrompt = `${marker('topublished')} which of you answered?`;
    await startTurn(page, {
      projectId,
      conversation,
      participantId: publishedParticipant?.id ?? '',
      prompt: publishedPrompt,
    });
    // `contains` is not decoration here, it is what makes the wait about THIS
    // turn: the helper reads the NEWEST assistant row, and the previous turn's
    // row is already finished — so without a marker of its own this would
    // settle instantly on the private agent's answer and assert nothing.
    await expectStoredAssistantAnswer(page, projectId, conversation.id, {
      timeout: 300_000,
      contains: publishedPrompt,
      message:
        'the published agent stored no answer — its version is the publish CLONE, read from the ' +
        'author project’s own schema, and a resolver that cannot find it refuses the turn instead',
    });
    const afterPublished = await readStoredMessageGroups(page, projectId, conversation.id);
    expect(
      afterPublished.at(-1)?.authorParticipantId,
      'the published agent’s turn was answered by another participant — addressing one agent in a ' +
        'mixed conversation must reach that agent',
    ).toBe(publishedParticipant?.id);
    expect(
      (await readStoredTranscript(page, projectId, conversation.id)).filter((row) => row.isError),
      'no row may be flagged is_error',
    ).toEqual([]);

    // ── THE CATALOGUE'S OWN COPY, CHATTED WITH FROM ANOTHER PROJECT ───────
    //
    // This is the marketplace case, and it is the one the participant add
    // already allowed and the turn used to refuse: nothing checks the project a
    // participant names, so the conversation could hold the catalogue twin and
    // every send answered 422.
    //
    // Two things make the turn answerable now. The resolver admits a
    // participant whose `entity_meta.project_id` is the catalogue project AND
    // whose version is `published`, and it reads that version out of the
    // CATALOGUE project's schema rather than this conversation's — which is the
    // only schema it is in. Everything else stays here: the conversation, the
    // history, the execution and its budget are this project's.
    //
    // A private agent in a stranger's project is still refused; that boundary
    // is pinned by the Postgres cases next to the resolver, because it needs a
    // third project this rig does not have.
    expect(catalogueVersionId, 'the publish must name the catalogue twin’s version').not.toBe('');
    const crossProject = await addParticipants(page, projectId, conversation.id, [
      agentParticipantBody(
        { id: catalogueAgentId, versionId: catalogueVersionId, name: authored.name },
        catalogueProjectId,
      ),
    ]);
    const twinParticipant = crossProject.find(
      (participant) => String(participant.entityMeta['id'] ?? '') === catalogueAgentId,
    );
    expect(
      twinParticipant,
      'the catalogue twin must be addable as a participant of a conversation in another project',
    ).toBeDefined();
    const cataloguePrompt = `${marker('tocatalogue')} which of you answered?`;
    const answered = await postTurn(page, {
      projectId,
      conversation,
      participantId: twinParticipant?.id ?? '',
      prompt: cataloguePrompt,
    });
    expect(
      answered.status(),
      'a turn addressed at the PUBLISHED catalogue twin was refused: ' +
        `${(await answered.text()).slice(0, 400)}`,
    ).toBe(200);
    // The same `contains` discipline as the two turns above: the newest
    // assistant row is the previous turn's, already finished, so only this
    // turn's own marker makes the wait about this turn.
    await expectStoredAssistantAnswer(page, projectId, conversation.id, {
      timeout: 300_000,
      contains: cataloguePrompt,
      message:
        'the catalogue twin stored no answer — its version lives in the catalogue project’s ' +
        'schema, so a resolver that read only this conversation’s schema cannot find it',
    });
    const afterCatalogue = await readStoredMessageGroups(page, projectId, conversation.id);
    expect(
      afterCatalogue.at(-1)?.authorParticipantId,
      'the catalogue twin’s turn was answered by another participant',
    ).toBe(twinParticipant?.id);
    expect(
      (await readStoredTranscript(page, projectId, conversation.id)).filter((row) => row.isError),
      'the cross-project turn stored an is_error row',
    ).toEqual([]);
  } finally {
    if (publishedVersionId !== '') {
      await page.request.post(
        `${API}/elitea_core/unpublish/prompt_lib/${projectId}/${publishedVersionId}`,
        { data: {} },
      );
    }
    await page.request.delete(
      `${API}/elitea_core/conversation/prompt_lib/${projectId}/${conversation.id}`,
    );
    // The withdrawal first, then the delete: a live published version refuses
    // the delete of the agent that owns it, so a cleanup that only deleted
    // would leave the agent, its clone AND its catalogue row behind.
    await deleteAgents(page, projectId, authored, privateAgent);
  }
});
