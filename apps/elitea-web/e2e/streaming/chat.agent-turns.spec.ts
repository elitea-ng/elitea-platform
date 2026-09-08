/**
 * WHAT A TURN IS OFFERED, AND WHO ANSWERS IT: the bare-model turn, the tool a
 * conversation's own toolkit puts in front of it, and the rule that a tool
 * result reaches the reader as plain content.
 *
 * ── What this ports ────────────────────────────────────────────────────────
 *
 * The legacy API suite's message-creation cases that need a real turn: chatting
 * with a MODEL and no agent, invoking one named tool of a toolkit from that
 * chat, the toolkit and the agent participants a conversation holds being
 * offered to a bare-model turn automatically, and the regression that a tool's
 * result must never be rendered as a runtime object's repr. Every one of them
 * was written against a model that decides; each is restated here against the
 * offline mock's scripted answers and its two journals, which is what makes
 * them deterministic rather than probable.
 *
 * ── Why the turns are driven over the API, not through the composer ────────
 *
 * The subject is WHICH PARTICIPANTS a conversation holds and what the runtime
 * then does with them: a toolkit sitting beside the model, an agent sitting
 * beside it, a turn addressed at the model while both are there. The composer
 * offers no way to build those conversations — it creates exactly one `user`
 * and one `dummy` participant for an ad-hoc chat (`useChatBoxSend.ts`'s
 * `adhocParticipants`) — so a browser-driven version of these cases would have
 * to fall back to the same REST calls with a page in the way.
 * `chat.messageRefusals.spec.ts` already drives this route directly for the
 * same reason. What is asserted is the STORE and the mock's journals, never a
 * bubble.
 *
 * ── The two contracts, and why the bare-model one is a different route ─────
 *
 * `agent.execute.application.v1` addresses an AGENT participant and reads its
 * version. `agent.execute.adhoc.v1` addresses the `dummy` participant that
 * stands for "the model itself" and carries `llm_settings` in the request —
 * the legacy body's `llm_settings` key, in the same place. The resolver behind
 * it (`ResolveCurrentAdhocTurn`) is also where the auto-injection this file is
 * about happens: it unions the conversation's TOOLKIT participants and its
 * AGENT participants into the turn's `tools` array, so a bare-model turn is
 * offered them without anything being attached to anything.
 *
 * ── What is NOT here, and why ──────────────────────────────────────────────
 *
 *  - THE LEGACY DIRECT-INVOCATION FIELD. The legacy suite invoked one tool by
 *    name with a `tool_call_input` body key and no model in the loop. This
 *    platform has no such REST field — the only sender of `tool_call_input` is
 *    the toolkit test pane's socket path (`useToolkitChatDispatch.hooks.ts`),
 *    and the standalone stack builds the app with no socket at all. The USE
 *    CASE — "invoke one NAMED tool of a toolkit from a chat and see the run's
 *    outcome" — is ported instead by naming the operation in the turn: the
 *    mock's `[[mock:call_tool <operationId>]]` marker makes the model call
 *    exactly that operation, and the journals then say whether it was offered,
 *    dispatched and answered. The half that is genuinely lost is the legacy
 *    `meta.execution_time_seconds > 0` assertion, which has no field here.
 *
 *  - A CONVERSATION-LEVEL TOOLKIT INJECTED INTO AN AGENT TURN. The legacy
 *    suite has that case (a toolkit-less agent picking up a toolkit that sits
 *    in the conversation). This platform refuses it BY CONSTRUCTION:
 *    `ResolveCurrentApplicationTurn` carries
 *    `AND NOT EXISTS (… chat_participants … entity_name = 'toolkit' …)`, so an
 *    agent turn in a conversation that holds any toolkit participant resolves
 *    no rows and the send answers 422. It is an unported behaviour, not a
 *    flake, and it is left unasserted here rather than pinned: a test of the
 *    refusal would freeze the gap as if it were the contract.
 *
 *  - AN AGENT THAT OWNS A TOOLKIT CALLING IT. Already covered end to end, from
 *    the form, by `chat.toolkit.spec.ts`.
 *
 * Lives in `streaming/` because every case here runs a real turn, which needs
 * the FULL standalone stack (`chat-stream` project, `scripts/chat-stream-e2e.sh`).
 */
import { expect, test, type Page } from '@playwright/test';
import { randomUUID } from 'node:crypto';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  EMPTY_TOOLKIT_GUARDRAILS,
  MOCK_CALL_TOOL_SENTINEL,
  MOCK_TOOL_EFFECTFUL_OPERATION,
  MOCK_TOOL_READ_OPERATION,
  agentAsToolName,
  callToolPrompt,
  clearMockLlmJournal,
  createAgentWithVersion,
  createOpenApiToolkitThroughForm,
  expectStoredAssistantAnswer,
  fetchMockToolSpec,
  readCallerIdentity,
  readCallerPersonalProjectId,
  readMockLlmJournal,
  readStoredMessageGroups,
  readStoredTranscript,
  setToolkitGuardrails,
} from '../fixtures/api';

const API = `${BASE_URL}/api/v2`;

/** The two execution contracts this file drives. */
const ADHOC_CONTRACT = 'agent.execute.adhoc.v1';

/**
 * The model the turns run on, as the WIRE names it.
 *
 * `vllm/…` and not the bare display name: bifrost resolves the provider from
 * the model string alone, so a bare `E2E-MOCK-MODEL` never reaches the seeded
 * credential (`deploy/scripts/standalone-stack.sh`'s `seed-llm`). The same pin
 * `chat.toolkit.spec.ts` puts on an agent version, here in the request's own
 * `llm_settings` because an ad-hoc turn has no version to read.
 */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/** A marker unique to one turn, which the mock echoes back verbatim. */
function marker(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}${Date.now() % 1_000_000}`;
}

interface CreatedConversation {
  readonly id: string;
  /** The message route addresses a conversation by UUID, never by the numeric id. */
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
  expect(conversation.uuid, 'the create must answer the uuid the message route addresses').not.toBe(
    '',
  );
  return conversation;
}

/** One participant row, as the add answers it back. */
interface StoredParticipant {
  readonly id: string;
  readonly entityName: string;
  readonly entityMeta: Record<string, unknown>;
}

/**
 * Add participants and hand back the whole list the server now holds.
 *
 * The route answers the CONVERSATION's participants rather than the rows it
 * just wrote, so the ids below are read out of that list by identity — a
 * participant row is deduplicated on `(entity_name, entity_meta)`, so an add
 * that found an existing row answers the same shape as one that created it.
 */
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

/** The `user` + `dummy` pair an ad-hoc conversation needs before it can answer. */
function adhocParticipantBodies(userId: string): readonly Record<string, unknown>[] {
  return [
    { entity_name: 'user', entity_meta: { id: Number(userId) } },
    {
      entity_name: 'dummy',
      entity_meta: { name: MOCK_MODEL },
      entity_settings: { llm_settings: { model_name: MOCK_MODEL, stream: true } },
    },
  ];
}

/**
 * Start one turn and require it to be ADMITTED.
 *
 * 422 `unsupported_agent_execution` is what every resolver miss produces — a
 * participant that is not in the conversation, a version that names no row, a
 * conversation-level toolkit on an agent turn — and its body is the sentence a
 * maintainer will search for, so it is spelled into the failure.
 */
async function startTurn(
  page: Page,
  input: {
    readonly projectId: string;
    readonly conversation: CreatedConversation;
    readonly participantId: string;
    readonly prompt: string;
    readonly contract: string;
    readonly llmSettings?: Readonly<Record<string, unknown>>;
  },
): Promise<void> {
  const body: Record<string, unknown> = {
    project_id: Number(input.projectId),
    conversation_uuid: input.conversation.uuid,
    participant_id: Number(input.participantId),
    question_id: randomUUID(),
    interaction_uuid: randomUUID(),
    payload: { user_input: input.prompt },
  };
  if (input.llmSettings !== undefined) body['llm_settings'] = input.llmSettings;
  const started = await page.request.post(
    `${API}/elitea_core/messages/prompt_lib/${input.projectId}/${input.conversation.uuid}` +
      `?execution_contract=${input.contract}`,
    { data: body },
  );
  expect(
    started.status(),
    `the turn was refused: ${(await started.text()).slice(0, 400)}`,
  ).toBe(200);
}

/** The `llm_settings` a bare-model turn carries — the legacy body's own key. */
const ADHOC_LLM_SETTINGS = { model_name: MOCK_MODEL, stream: true } as const;

/**
 * The stored trace steps of one conversation, with the heavy fields.
 *
 * TWO reads, because the platform serves them as two: the list is the light
 * projection a resting chip draws and carries no `tool_output` at all, and the
 * detail requires `message_group_id` as a scoping parameter rather than as an
 * optional filter. The `tool_output` this file asserts on lives only in the
 * second.
 */
async function readToolCallSteps(
  page: Page,
  projectId: string,
  conversationId: string,
): Promise<readonly { readonly toolName: string; readonly toolOutput: string; readonly isError: boolean }[]> {
  const list = await page.request.get(
    `${API}/elitea_core/message_traces/prompt_lib/${projectId}/${conversationId}?kind=tool_call`,
  );
  if (!list.ok()) return [];
  const rows = ((await list.json()) as { rows?: readonly { id?: unknown; message_group_id?: unknown }[] })
    .rows ?? [];
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

/**
 * The two spellings a raw LangChain `ToolMessage` leaves in text.
 *
 * The legacy regression (a tool result rendered as a runtime object rather than
 * as its content) is recognisable by either: `repr(ToolMessage)` opens with the
 * class name and always carries the `tool_call_id=` keyword. The python
 * worker's own trace writer is where such a value would enter — `_trace_text`
 * falls back to `str(value)` for anything it cannot serialise as JSON — so this
 * is asserted on the stored text a reader sees AND on the trace step behind it.
 */
const RUNTIME_OBJECT_REPRS = ['tool_call_id=', 'ToolMessage('] as const;

function assertNoRuntimeRepr(text: string, where: string): void {
  for (const repr of RUNTIME_OBJECT_REPRS) {
    expect(
      text.includes(repr),
      `${where} carries \`${repr}\` — the tool's result reached the reader as a runtime object ` +
        `instead of as its content: ${text.slice(0, 300)}`,
    ).toBe(false);
  }
}

/* ────────────────────────────────────────────────────────────────────────── */

test('a bare-model turn answers, and the stored exchange pairs the question with the reply', async ({
  page,
}) => {
  // One model call and the stream back, plus the store settling behind it.
  test.setTimeout(240_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project, and the setup waits for one').not.toBe('');
  const caller = await readCallerIdentity(page.request);
  const prompt = `${marker('bare')} what is 2+2?`;

  const conversation = await createConversation(page, projectId, marker('bareconv'));
  try {
    const participants = await addParticipants(
      page,
      projectId,
      conversation.id,
      adhocParticipantBodies(caller.id),
    );
    const model = participants.find((participant) => participant.entityName === 'dummy');
    expect(
      model,
      'a bare-model chat resolves against a `dummy` participant, and the add answered none',
    ).toBeDefined();

    await startTurn(page, {
      projectId,
      conversation,
      participantId: model?.id ?? '',
      prompt,
      contract: ADHOC_CONTRACT,
      llmSettings: ADHOC_LLM_SETTINGS,
    });

    // The legacy case required the reply to be a bare number, which is an
    // assertion about a model that can do arithmetic. The mock ECHOES, so the
    // discriminating claim here is stronger and does not depend on the model:
    // the stored answer carries THIS turn's marker, so it cannot be a cached
    // reply, another conversation's, or the error card a refused turn stores in
    // the same place.
    await expectStoredAssistantAnswer(page, projectId, conversation.id, {
      timeout: 180_000,
      contains: prompt,
      message:
        'the bare-model turn stored no answer — an ad-hoc turn was admitted and then produced ' +
        'nothing, which is what an unresolvable model credential looks like from here',
    });

    // ── The exchange, as the legacy contract states it ────────────────────
    const groups = await readStoredMessageGroups(page, projectId, conversation.id);
    expect(
      groups.map((group) => group.items.length),
      'one POST must store exactly two groups — the question and the reply — with one text item each',
    ).toEqual([1, 1]);
    const [question, answer] = groups;
    expect(question?.content, 'the question group must hold what was sent').toBe(prompt);
    expect(
      question?.sentToId,
      'the question must be addressed to the participant the request named',
    ).toBe(model?.id);
    expect(
      answer?.authorParticipantId,
      'the reply must be written by the participant the question was addressed to',
    ).toBe(model?.id);
    expect(answer?.replyToId, 'the reply must point back at its own question').toBe(question?.id);
    expect(
      answer?.content ?? '',
      'the reply must be the answer to THIS question',
    ).toContain(prompt);

    expect(
      (await readStoredTranscript(page, projectId, conversation.id)).filter((row) => row.isError),
      'no row may be flagged is_error — a refused turn is stored as an assistant row and reads like an answer',
    ).toEqual([]);
  } finally {
    await page.request.delete(
      `${API}/elitea_core/conversation/prompt_lib/${projectId}/${conversation.id}`,
    );
  }
});

test('a toolkit in the conversation is offered to a bare-model turn, and the operation the turn names runs', async ({
  page,
}) => {
  // A form flow, a model call, a tool dispatch and a second model call.
  test.setTimeout(420_000);

  // The guardrail policy is GLOBAL and shared with `chat.toolkit-hitl.spec.ts`,
  // which marks these very operations sensitive. Cleared here for the reason
  // `chat.toolkit.spec.ts` states: a run killed between that spec's two hooks
  // leaves the entry behind, and this turn would then park on a pause nobody
  // answers instead of calling anything.
  await setToolkitGuardrails(EMPTY_TOOLKIT_GUARDRAILS);
  await clearMockLlmJournal(page);

  const caller = await readCallerIdentity(page.request);
  const spec = await fetchMockToolSpec(page);
  const toolkitName = `${AUTOTEST_PREFIX}tkc-${Date.now() % 1_000_000}`;
  // Through the FORM, because that is the only place `settings.selected_tools`
  // is derived from the specification rather than typed by the caller — a
  // toolkit whose selection does not match what the document declares offers
  // the model nothing, and this case is about what gets OFFERED.
  const toolkit = await createOpenApiToolkitThroughForm(page, toolkitName, spec.text);
  const projectId = toolkit.projectId;
  const prompt = callToolPrompt(MOCK_TOOL_READ_OPERATION, marker('tkcall'));

  const conversation = await createConversation(page, projectId, marker('tkconv'));
  try {
    const participants = await addParticipants(page, projectId, conversation.id, [
      ...adhocParticipantBodies(caller.id),
      // The whole subject of this case: a TOOLKIT standing in the conversation,
      // attached to nothing. `entity_meta.id` is what the resolver joins
      // `elitea_tools` on, and `project_id` is what scopes that join.
      {
        entity_name: 'toolkit',
        entity_meta: { id: toolkit.toolkitId, project_id: Number(projectId), name: toolkitName },
      },
    ]);
    const model = participants.find((participant) => participant.entityName === 'dummy');
    expect(model, 'the ad-hoc turn still needs its `dummy` participant').toBeDefined();
    expect(
      participants.filter((participant) => participant.entityName === 'toolkit'),
      'the toolkit must be in the conversation, or nothing is being auto-injected',
    ).toHaveLength(1);

    await startTurn(page, {
      projectId,
      conversation,
      participantId: model?.id ?? '',
      prompt,
      contract: ADHOC_CONTRACT,
      llmSettings: ADHOC_LLM_SETTINGS,
    });

    // Polled on the mock's END sentinel rather than on the operation name: the
    // stored row is readable while it is still being written and the name
    // arrives in its first words, so a poll on the name settles mid-reply.
    await expectStoredAssistantAnswer(page, projectId, conversation.id, {
      timeout: 240_000,
      contains: MOCK_CALL_TOOL_SENTINEL,
      message:
        'no stored answer quoting a tool result — either the conversation toolkit was never ' +
        'injected into the ad-hoc turn (the turn then answers with a plain echo) or the call ' +
        'was never dispatched',
    });

    // ── THE AUTO-INJECTION PROOF, taken server-side ───────────────────────
    //
    // These are the function names the RUNTIME put in the model request. The
    // turn addressed the MODEL and nothing was attached to anything, so a
    // toolkit that reached this list can only have come from the conversation's
    // participants — which is the behaviour the legacy case is about. A turn
    // whose toolkit was dropped at assembly still answers, and answers in the
    // same shape.
    const journal = await readMockLlmJournal(page);
    const offer = journal.find((entry) => entry.mode === 'call_tool');
    expect(
      offer,
      'the mock never took the call_tool branch — the marker did not reach the model, so nothing was scripted',
    ).toBeDefined();
    expect(
      offer?.tools ?? [],
      'the conversation toolkit was never offered to the bare-model turn — it was not injected',
    ).toEqual(expect.arrayContaining([MOCK_TOOL_READ_OPERATION, MOCK_TOOL_EFFECTFUL_OPERATION]));
    // THE DISPATCH PROOF. The mock takes this branch only when the transcript it
    // receives already holds a `tool` message, which exists only because the
    // runtime executed the call the model asked for and fed the result back.
    expect(
      journal.map((entry) => entry.mode),
      'no continuation carrying a tool result — the model named the operation and the runtime never called it',
    ).toContain('call_tool_resumed');

    // ── THE OUTPUT-HYGIENE RULE ───────────────────────────────────────────
    //
    // The mock's continuation quotes the tool result VERBATIM, so the stored
    // reply is where a runtime object's repr would surface to the reader. The
    // trace step is where it would surface to the trace pane. Both are checked,
    // because they are written by different code on each runtime.
    const transcript = await readStoredTranscript(page, projectId, conversation.id);
    const reply = transcript.filter((row) => row.role === 'assistant').at(-1)?.content ?? '';
    expect(reply, 'the finished reply must name the operation the turn asked for').toContain(
      MOCK_TOOL_READ_OPERATION,
    );
    assertNoRuntimeRepr(reply, 'the stored reply');
    expect(
      transcript.filter((row) => row.isError),
      'no row may be flagged is_error — a refused turn is stored as an assistant row',
    ).toEqual([]);

    // The trace projection is written from the same frames the stream carries
    // and finishes after the reply text does, so this is polled rather than
    // read once. Its failure line says the step is missing, which is the other
    // thing that can be wrong here.
    await expect
      .poll(async () => (await readToolCallSteps(page, projectId, conversation.id)).map((step) => step.toolName), {
        timeout: 60_000,
        message:
          'the turn dispatched a call but the stored trace holds no tool_call step for it — the ' +
          'trace pane would redraw this conversation with no record of the tool that ran',
      })
      .toContain(MOCK_TOOL_READ_OPERATION);
    for (const step of await readToolCallSteps(page, projectId, conversation.id)) {
      assertNoRuntimeRepr(step.toolOutput, `the \`${step.toolName}\` trace step's tool_output`);
    }
  } finally {
    await page.request.delete(
      `${API}/elitea_core/conversation/prompt_lib/${projectId}/${conversation.id}`,
    );
    await page.request.delete(
      `${API}/elitea_core/tool/prompt_lib/${projectId}/${toolkit.toolkitId}`,
    );
  }
});

test('an agent in the conversation is offered to a bare-model turn as a tool, and does not answer it', async ({
  page,
}) => {
  test.setTimeout(300_000);

  await clearMockLlmJournal(page);
  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);

  const agentName = `${AUTOTEST_PREFIX}ptcp-${Date.now() % 1_000_000}`;
  const agent = await createAgentWithVersion(
    page.request,
    agentName,
    {
      instructions: 'You answer questions about this test fixture and nothing else.',
      // Pinned for the reason `chat.hitl.spec.ts` gives: an agent turn runs on
      // `version_details.llm_settings.model_name`, and an empty `llm_settings`
      // falls back to the project catalogue's default.
      model: { modelName: MOCK_MODEL },
      meta: { step_limit: 25, internal_tools: [] },
    },
    projectId,
  );
  const prompt = `${marker('nohijack')} what is 2+2?`;

  const conversation = await createConversation(page, projectId, marker('mixconv'));
  try {
    const participants = await addParticipants(page, projectId, conversation.id, [
      ...adhocParticipantBodies(caller.id),
      // `entity_settings.version_id` is load-bearing and deliberately sent:
      // the resolver joins the agent's version through it, so a participant
      // added without one contributes no tool at all. The legacy suite has a
      // case that adds agent participants WITHOUT a version and still expects
      // the turn to answer — it does answer here too, but it answers with the
      // agent offered to nobody, so that variant would assert an absence.
      {
        entity_name: 'application',
        entity_meta: { id: agent.id, project_id: Number(projectId), name: agentName },
        entity_settings: { version_id: agent.versionId },
      },
    ]);
    const model = participants.find((participant) => participant.entityName === 'dummy');
    const agentParticipant = participants.find(
      (participant) => participant.entityName === 'application',
    );
    expect(agentParticipant, 'the agent must be in the conversation').toBeDefined();

    // Addressed at the MODEL, with the agent standing right there.
    await startTurn(page, {
      projectId,
      conversation,
      participantId: model?.id ?? '',
      prompt,
      contract: ADHOC_CONTRACT,
      llmSettings: ADHOC_LLM_SETTINGS,
    });

    await expectStoredAssistantAnswer(page, projectId, conversation.id, {
      timeout: 180_000,
      contains: prompt,
      message:
        'the bare-model turn stored no answer while an agent participant was present — an agent ' +
        'in the conversation must not take the turn over',
    });

    // ── The agent did not HIJACK the turn ─────────────────────────────────
    // The only field that can say so: every reply here echoes the same question,
    // so the author id is what tells "the model answered" from "the agent did".
    const groups = await readStoredMessageGroups(page, projectId, conversation.id);
    expect(
      groups.at(-1)?.authorParticipantId,
      'the reply was written by the AGENT participant — a turn addressed at the model was taken over',
    ).toBe(model?.id);

    // ── …and the agent WAS offered, as a callable tool ────────────────────
    // The other half of the same behaviour, and the one a stored answer cannot
    // show. The runtime presents a saved agent to the model under a name it
    // derives itself; `agentAsToolName` carries both runtimes' rules, and the
    // failure prints what was really offered so a naming change reads as one.
    const expected = agentAsToolName({
      agentId: agent.id,
      versionId: agent.versionId,
      name: agentName,
    });
    const journal = await readMockLlmJournal(page);
    const offers = journal.filter((entry) => entry.tools.length > 0).flatMap((entry) => entry.tools);
    expect(
      offers,
      'the agent participant was never offered to the bare-model turn as a callable tool',
    ).toContain(expected);

    expect(
      (await readStoredTranscript(page, projectId, conversation.id)).filter((row) => row.isError),
      'no row may be flagged is_error',
    ).toEqual([]);
  } finally {
    await page.request.delete(
      `${API}/elitea_core/conversation/prompt_lib/${projectId}/${conversation.id}`,
    );
    await page.request.delete(`${API}/elitea_core/application/prompt_lib/${projectId}/${agent.id}`);
  }
});
