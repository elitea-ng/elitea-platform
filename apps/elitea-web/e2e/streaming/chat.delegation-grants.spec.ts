/**
 * THE SPECIALIST SUB-AGENT, END TO END (ADR-0044 Amendment 1, A1.6; epic 2156
 * story S7, issue 2163; closes the report in issue 2137).
 *
 * The report: Agent1 holds a toolkit (git in the report) and is attached to
 * Agent2, which holds none. Asked for that toolkit, Agent2 got nothing, because
 * a delegated agent only ever used what its caller also held. Amendment 1
 * makes the attach a reviewed grant: an editor of Agent2 approves what Agent1
 * brings, and the specialist then works without widening Agent2.
 *
 * ## What is real, and what is scripted
 *
 * Only the parent model's DECISION to delegate is scripted (the mock's
 * `[[mock:call_tool <agent tool> {"task": …}]]` marker, as in
 * `chat.delegation.spec.ts`). Everything else is the product: the attach writes
 * the grant, Main computes the policy and the principal bound, the Rust worker
 * enforces the grant on the edge, the refusal reaches the model as a tool
 * result and the chat as a notice, and the approval is clicked in the real
 * review dialog. The "git" toolkit is the mock's own OpenAPI tool (the same
 * fixture `chat.toolkit.spec.ts` uses), cloned with its operations renamed per
 * toolkit so a request's offered tools say WHICH toolkit reached the callee.
 *
 * ## How each scenario of the issue is asserted
 *
 *  1. Before approval: the chat notice with Review delegation (read from the
 *     stored answer, and seen in the browser), the model's refusal in the
 *     reply, no model request by the specialist, and the delegation step's
 *     `delegation_refusal`.                       -> "J1"
 *  2. An editor approves in the dialog; the same request runs: the specialist's
 *     model request offers the toolkit, and the delegation step records the
 *     grant id and digest.                         -> "J1"
 *  3. A user without access to the toolkit is refused (principal bound), with no
 *     Review delegation. NOT driven here: the chat persona is the only person
 *     who owns the project, and every other persona is either a global
 *     administrator or a project viewer that cannot run the chain at all. It is
 *     covered where a second principal is real: the Rust worker's principal
 *     tests (`libs/rust/agent-runtime/src/delegation_tests.rs`), Main's
 *     principal sources against a real Postgres
 *     (`worker_principal_postgres_integration_test.go`), the notice's
 *     `can_review: false` card in `chat.delegationNotice.spec.ts`.
 *  4. An approver without access to the toolkit cannot approve its item. NOT
 *     driven here, for the same reason (a second person with the edit
 *     permission and without the toolkit). Covered against a real Postgres by
 *     `TestDelegationGrantPartialApprovalByAnApproverWithoutAccess` and
 *     `TestGovernedDelegationApprovesOnlyWhatTheApproversHold`, and in the
 *     dialog's own unit tests (the item is disabled, with the reason).
 *  5. Drift: a second toolkit is added to the specialist; the next run uses
 *     only the first, the grant is `stale`, ONE notification is sent, the
 *     chat says it ran without the new toolkit, and re-approval in the dialog
 *     restores it.                                -> "J2"
 *  6. Transitivity: with only Agent1's edge approved, a call that reaches
 *     Agent1's own sub-agent is refused naming that second edge, and approving
 *     that edge lets it run.                       -> "J3"
 *  7. `report` runs an unapproved delegation and records it on the step;
 *     `enforce` refuses.                           -> "J4"
 *
 * The maker-checker of the governed path (the proposer cannot approve; a
 * second person can) is a stack flag this journey does not own. It is covered
 * against a real Postgres by `TestDelegationGrantMakerCheckerInCodeAndInTheTable`
 * and `TestGovernedDelegationIsApprovedThroughItsChangeRequest`
 * (`services/elitea-main/internal/infra/db/repos`).
 *
 * Lives in `streaming/` because every case runs real turns on the full
 * standalone stack (`chat-stream` project, `scripts/chat-stream-e2e.sh`).
 */
import { expect, request as playwrightRequest, test, type Page } from '@playwright/test';
import { randomUUID } from 'node:crypto';

import { BASE_URL, STORAGE_STATE } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  MOCK_CALL_TOOL_SENTINEL,
  MOCK_TOOL_EFFECTFUL_OPERATION,
  MOCK_TOOL_READ_OPERATION,
  agentAsToolName,
  callToolWithArgumentsPrompt,
  clearMockLlmJournal,
  createAgentWithVersion,
  expectStoredAssistantAnswer,
  fetchMockToolSpec,
  readCallerIdentity,
  readCallerPersonalProjectId,
  readMockLlmJournal,
  readStoredTranscript,
} from '../fixtures/api';

const API = `${BASE_URL}/api/v2`;
const APPLICATION_CONTRACT = 'agent.execute.application.v1';
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';
const DELEGATION_SECTION = `${API}/admin/plugin_config_values/administration/delegation`;
const REFUSAL_CODE = 'DELEGATION_GRANT_MISSING';

interface Agent {
  readonly id: string;
  readonly versionId: string;
  readonly name: string;
}

interface Toolkit {
  readonly id: string;
  readonly name: string;
  /** The operation names this toolkit offers a model: `mock_tool_status_<tag>`. */
  readonly readOperation: string;
}

interface Grant {
  readonly id: number;
  readonly callee_application_id: number;
  readonly state: string;
  readonly digest: string;
  readonly drift?: { readonly added?: readonly { readonly kind: string; readonly name: string }[] };
}

interface Notice {
  readonly kind: string;
  readonly reason: string;
  readonly code?: string;
  readonly grant_id?: number;
  readonly can_review?: boolean;
  readonly edge: { readonly caller_id?: number; readonly callee_id?: number; readonly kind: string };
  readonly missing: readonly { readonly kind: string; readonly name: string }[];
}

interface Conversation {
  readonly id: string;
  readonly uuid: string;
  /** The root agent's participant row: the turn is addressed to it. */
  readonly participantId: string;
}

function stampOf(): number {
  return Date.now() % 1_000_000;
}

function escaped(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

function toolNameOf(agent: Agent): string {
  return agentAsToolName({ agentId: agent.id, versionId: agent.versionId });
}

/** Everything one journey created, removed in dependency order whatever happened. */
class Cleanup {
  private readonly agents: Agent[] = [];
  private readonly toolkits: Toolkit[] = [];
  private readonly conversations: Conversation[] = [];

  private readonly page: Page;
  private readonly projectId: string;

  constructor(page: Page, projectId: string) {
    this.page = page;
    this.projectId = projectId;
  }

  agent(agent: Agent): Agent {
    this.agents.push(agent);
    return agent;
  }

  toolkit(toolkit: Toolkit): Toolkit {
    this.toolkits.push(toolkit);
    return toolkit;
  }

  conversation(conversation: Conversation): Conversation {
    this.conversations.push(conversation);
    return conversation;
  }

  async run(): Promise<void> {
    const request = this.page.request;
    for (const conversation of this.conversations) {
      await request.delete(`${API}/elitea_core/conversation/prompt_lib/${this.projectId}/${conversation.id}`).catch(() => {});
    }
    // Callers before callees: a relation goes with its caller.
    for (const agent of [...this.agents].reverse()) {
      await request.delete(`${API}/elitea_core/application/prompt_lib/${this.projectId}/${agent.id}`).catch(() => {});
    }
    for (const toolkit of this.toolkits) {
      await request.delete(`${API}/elitea_core/tool/prompt_lib/${this.projectId}/${toolkit.id}`).catch(() => {});
    }
  }
}

async function createAgent(page: Page, projectId: string, name: string): Promise<Agent> {
  const created = await createAgentWithVersion(
    page.request,
    name,
    {
      instructions:
        'You are a delegation grant fixture. Carry out the task you are given, or hand it to the ' +
        'saved agent you have been attached to, and report exactly what came back.',
      welcomeMessage: 'Give me something to delegate.',
      conversationStarters: ['Delegate this task.'],
      model: { modelName: MOCK_MODEL },
      meta: { step_limit: 25, internal_tools: [] },
    },
    projectId,
    `${AUTOTEST_PREFIX}delegation grant fixture agent for the streaming suite`,
  );
  return { id: created.id, versionId: created.versionId, name };
}

/**
 * The mock's OpenAPI tool as a toolkit whose operations carry `tag`, so two
 * toolkits never offer a model the same function name and a request's `tools`
 * say which toolkit reached it. The mock's routes are keyed by path, so a
 * renamed `operationId` is still served.
 */
async function createToolkit(page: Page, projectId: string, label: string, tag: string): Promise<Toolkit> {
  const spec = await fetchMockToolSpec(page);
  const read = `${MOCK_TOOL_READ_OPERATION}_${tag}`;
  const effectful = `${MOCK_TOOL_EFFECTFUL_OPERATION}_${tag}`;
  const text = spec.text
    .split(MOCK_TOOL_READ_OPERATION)
    .join(read)
    .split(MOCK_TOOL_EFFECTFUL_OPERATION)
    .join(effectful);
  const name = `${AUTOTEST_PREFIX}dgtk-${label}-${String(stampOf())}`;
  const created = await page.request.post(`${API}/elitea_core/tools/prompt_lib/${projectId}`, {
    data: { name, type: 'openapi', settings: { spec: text, openapi_configuration: {}, selected_tools: [read, effectful] } },
  });
  expect(created.status(), `the toolkit must be created: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const id = String(((await created.json()) as { id?: unknown }).id ?? '');
  expect(id, 'the created toolkit must carry an id').not.toBe('');
  return { id, name, readOperation: read };
}

async function attachToolkit(page: Page, projectId: string, toolkit: Toolkit, agent: Agent): Promise<void> {
  const attached = await page.request.patch(`${API}/elitea_core/tool/prompt_lib/${projectId}/${toolkit.id}`, {
    data: { entity_version_id: Number(agent.versionId), entity_id: Number(agent.id), entity_type: 'agent', has_relation: true },
  });
  expect(attached.status(), `the toolkit attach was refused: ${(await attached.text()).slice(0, 300)}`).toBeLessThan(300);
}

/** Attach `child` to `parent` as a tool: the route also proposes the pending grant of the edge. */
async function attachChild(page: Page, projectId: string, parent: Agent, child: Agent): Promise<void> {
  const attached = await page.request.patch(
    `${API}/elitea_core/application_relation/prompt_lib/${projectId}/${child.id}/${child.versionId}`,
    { data: { application_id: Number(parent.id), version_id: Number(parent.versionId), has_relation: true } },
  );
  expect(attached.status(), `the child must attach: ${(await attached.text()).slice(0, 300)}`).toBeLessThan(300);
}

async function readGrants(page: Page, projectId: string, caller: Agent, history = false): Promise<readonly Grant[]> {
  const response = await page.request.get(
    `${API}/elitea_core/delegation_grants/prompt_lib/${projectId}/${caller.id}${history ? '?history=true' : ''}`,
  );
  expect(response.ok(), `the grant list was refused: ${(await response.text()).slice(0, 300)}`).toBe(true);
  return ((await response.json()) as { grants?: readonly Grant[] }).grants ?? [];
}

async function grantOf(page: Page, projectId: string, caller: Agent, callee: Agent): Promise<Grant> {
  const grant = (await readGrants(page, projectId, caller)).find((row) => String(row.callee_application_id) === callee.id);
  expect(grant, `attaching ${callee.name} to ${caller.name} must have proposed a grant`).toBeDefined();
  return grant as Grant;
}

async function approveByApi(page: Page, projectId: string, caller: Agent, callee: Agent): Promise<Grant> {
  const pending = await grantOf(page, projectId, caller, callee);
  expect(pending.state, 'a fresh attach proposes a pending grant, never an approved one').toBe('pending');
  const approved = await page.request.post(
    `${API}/elitea_core/delegation_grant/prompt_lib/${projectId}/${caller.id}/${String(pending.id)}/approve`,
  );
  expect(approved.ok(), `the approval was refused: ${(await approved.text()).slice(0, 300)}`).toBe(true);
  const grant = (await approved.json()) as Grant;
  expect(grant.state).toBe('approved');
  return grant;
}

async function createConversation(page: Page, projectId: string, caller: { readonly id: string }, root: Agent): Promise<Conversation> {
  const created = await page.request.post(`${API}/elitea_core/conversations/prompt_lib/${projectId}`, {
    data: { name: `${AUTOTEST_PREFIX}dgconv-${String(stampOf())}-${randomUUID().slice(0, 6)}`, is_private: true },
  });
  expect(created.status(), `the conversation must be created: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const body = (await created.json()) as { id?: unknown; uuid?: unknown };
  const ids = { id: String(body.id ?? ''), uuid: String(body.uuid ?? '') };
  const added = await page.request.post(`${API}/elitea_core/participants/prompt_lib/${projectId}/${ids.id}`, {
    data: [
      { entity_name: 'user', entity_meta: { id: Number(caller.id) } },
      {
        entity_name: 'application',
        entity_meta: { id: root.id, project_id: Number(projectId), name: root.name },
        entity_settings: { version_id: root.versionId },
      },
    ],
  });
  expect(added.status(), `the participants must be added: ${(await added.text()).slice(0, 300)}`).toBe(200);
  const participants = (await added.json()) as readonly { id?: unknown; entity_name?: unknown }[];
  const agentParticipant = participants.find((row) => row.entity_name === 'application');
  expect(agentParticipant, 'the root agent must be a participant').toBeDefined();
  return { ...ids, participantId: String(agentParticipant?.id ?? '') };
}

interface TurnResult {
  readonly conversation: Conversation;
  readonly reply: string;
  readonly notices: readonly Notice[];
}

/**
 * One turn of `root` in a conversation of its own, settled the way the other
 * delegation journeys settle: the stored row has no `is_error` and carries the
 * mock's closing sentinel, which only a continuation after the tool result
 * has.
 */
async function runTurn(
  page: Page,
  cleanup: Cleanup,
  projectId: string,
  caller: { readonly id: string },
  root: Agent,
  prompt: string,
): Promise<TurnResult> {
  const conversation = cleanup.conversation(await createConversation(page, projectId, caller, root));
  const started = await page.request.post(
    `${API}/elitea_core/messages/prompt_lib/${projectId}/${conversation.uuid}?execution_contract=${APPLICATION_CONTRACT}`,
    {
      data: {
        project_id: Number(projectId),
        conversation_uuid: conversation.uuid,
        participant_id: Number(conversation.participantId),
        question_id: randomUUID(),
        interaction_uuid: randomUUID(),
        payload: { user_input: prompt },
      },
    },
  );
  expect(started.status(), `the turn was refused: ${(await started.text()).slice(0, 400)}`).toBe(200);
  await expectStoredAssistantAnswer(page, projectId, conversation.id, {
    timeout: 300_000,
    contains: MOCK_CALL_TOOL_SENTINEL,
    message: 'the root agent stored no completed answer; a delegation that fails mid-hop lands here as an is_error row',
  });
  const transcript = await readStoredTranscript(page, projectId, conversation.id);
  const answer = transcript.filter((row) => row.role === 'assistant').at(-1);
  expect(answer?.isError, 'a refused delegation is a tool result, never a failed turn').toBe(false);
  const raw = answer?.metadata['delegation_notices_v1'];
  return { conversation, reply: answer?.content ?? '', notices: Array.isArray(raw) ? (raw as readonly Notice[]) : [] };
}

/** `[[mock:call_tool <callee> {"task": <task>}]]`: the root model's scripted decision to delegate. */
function delegate(callee: Agent, task: string): string {
  return callToolWithArgumentsPrompt(toolNameOf(callee), { task }, 'delegate this and quote the answer');
}

function lastUserText(entry: { readonly history: readonly { readonly role: string; readonly text: string }[] }): string {
  return entry.history.filter((message) => message.role === 'user').at(-1)?.text ?? '';
}

/** The model request a delegated agent made for `task` (its prompt is the bare task), if it ran. */
async function requestForTask(page: Page, task: string): Promise<{ readonly tools: readonly string[] } | undefined> {
  const journal = await readMockLlmJournal(page);
  return journal.find((entry) => lastUserText(entry).includes(task) && !lastUserText(entry).includes('[[mock:'));
}

interface DelegationStep {
  readonly name: string;
  readonly summary: Record<string, unknown>;
}

/** The `delegation` steps of the conversation's runs, from the run evidence (ADR-0042). */
async function readDelegationSteps(page: Page, projectId: string, conversation: Conversation): Promise<readonly DelegationStep[]> {
  const runs = await page.request.get(`${API}/elitea_core/conversation_runs/prompt_lib/${projectId}/${conversation.uuid}`);
  expect(runs.ok(), `the conversation's runs were refused: ${(await runs.text()).slice(0, 300)}`).toBe(true);
  const ids = ((await runs.json()) as { runs?: readonly { run_id?: string }[] }).runs?.map((run) => String(run.run_id ?? '')) ?? [];
  const steps: DelegationStep[] = [];
  for (const id of ids) {
    const stepsPage = await page.request.get(`${API}/elitea_core/run_steps/prompt_lib/${projectId}/${encodeURIComponent(id)}?limit=1000`);
    expect(stepsPage.ok(), `the run's steps were refused: ${(await stepsPage.text()).slice(0, 300)}`).toBe(true);
    const body = (await stepsPage.json()) as { steps?: readonly { kind?: string; name?: string; summary?: Record<string, unknown> }[] };
    for (const step of body.steps ?? []) {
      if (step.kind === 'delegation') steps.push({ name: step.name ?? '', summary: step.summary ?? {} });
    }
  }
  return steps;
}

async function delegationStepsOf(page: Page, projectId: string, conversation: Conversation): Promise<readonly DelegationStep[]> {
  let found: readonly DelegationStep[] = [];
  await expect
    .poll(
      async () => {
        found = await readDelegationSteps(page, projectId, conversation);
        return found.length;
      },
      { timeout: 60_000, message: 'the run recorded no delegation step' },
    )
    .toBeGreaterThan(0);
  return found;
}

/** The delegation dialog of `callee` on `caller`'s page, opened by the same link the chat notice carries. */
async function openReview(page: Page, caller: Agent, callee: Agent): Promise<ReturnType<Page['getByRole']>> {
  await page.goto(`${BASE_URL}/app/agents/all/${caller.id}?review_delegation=${callee.id}`);
  const dialog = page.getByRole('dialog', { name: `What ${callee.name} will be able to use` });
  await expect(dialog, 'the review of that exact edge must open on arrival').toBeVisible({ timeout: 45_000 });
  return dialog;
}

interface DelegationValues {
  readonly mode: string;
  readonly max_depth: number;
  readonly principal_bound: boolean;
}

/** Read or write the platform's Delegation section as the ADMIN persona (global state; restore it). */
async function delegationSection(write?: Partial<DelegationValues>): Promise<DelegationValues> {
  const admin = await playwrightRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.admin });
  try {
    const read = await admin.get(DELEGATION_SECTION);
    expect(read.ok(), `the Delegation section must be readable: ${(await read.text()).slice(0, 300)}`).toBe(true);
    const current = ((await read.json()) as { values: DelegationValues }).values;
    if (write === undefined) return current;
    const saved = await admin.put(DELEGATION_SECTION, { data: { values: { ...current, ...write } } });
    expect(saved.status(), `the Delegation section must be saved: ${(await saved.text()).slice(0, 300)}`).toBe(200);
    return current;
  } finally {
    await admin.dispose();
  }
}

/* ────────────────────────────────────────────────────────────────────────── */

test('J1: asking the parent for the specialist\'s toolkit is refused with Review delegation, and runs once an editor approves (#2137)', async ({
  page,
}) => {
  test.setTimeout(600_000);
  await clearMockLlmJournal(page);
  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);
  const cleanup = new Cleanup(page, projectId);
  try {
    const stamp = stampOf();
    const git = cleanup.toolkit(await createToolkit(page, projectId, 'git', 'git'));
    const specialist = cleanup.agent(await createAgent(page, projectId, `${AUTOTEST_PREFIX}dgspec-${String(stamp)}`));
    const parent = cleanup.agent(await createAgent(page, projectId, `${AUTOTEST_PREFIX}dgpar-${String(stamp)}`));
    await attachToolkit(page, projectId, git, specialist);
    await attachChild(page, projectId, parent, specialist);
    const proposed = await grantOf(page, projectId, parent, specialist);
    expect(proposed.state, 'an attach proposes, and approves nothing').toBe('pending');

    // 1. BEFORE approval.
    const task = `${AUTOTEST_PREFIX}commit${String(stampOf())} report ready`;
    const refused = await runTurn(page, cleanup, projectId, caller, parent, delegate(specialist, task));
    expect(refused.reply, 'the model must be told the delegation is refused').toContain(REFUSAL_CODE);
    expect(refused.reply, 'the specialist must not have run').not.toContain(task);
    expect(await requestForTask(page, task), 'the specialist made a model request although its delegation was refused').toBeUndefined();
    const notice = refused.notices.find((row) => row.kind === 'refused');
    expect(notice, 'the answer must carry the refusal notice').toBeDefined();
    expect(notice, `the stored notice: ${JSON.stringify(notice)}`).toMatchObject({ kind: 'refused', reason: 'no_grant', code: REFUSAL_CODE, can_review: true });
    expect(notice?.edge.caller_id).toBe(Number(parent.id));
    expect(notice?.edge.callee_id).toBe(Number(specialist.id));
    expect(notice?.missing.map((item) => item.kind)).toContain('toolkit');
    const refusedSteps = await delegationStepsOf(page, projectId, refused.conversation);
    expect(
      refusedSteps.some((step) => (step.summary['delegation_refusal'] as { code?: string } | undefined)?.code === REFUSAL_CODE),
      `the delegation step must record the refusal: ${JSON.stringify(refusedSteps)}`,
    ).toBe(true);

    // The person sees it in the chat, with the way to fix it.
    await page.goto(`${BASE_URL}/app/chat/${refused.conversation.id}`);
    const card = page.getByTestId('delegation-notice').first();
    await expect(card).toContainText(
      new RegExp(`${escaped(specialist.name)} could not use .+ from ${escaped(parent.name)}\\. This delegation is not approved\\.`),
      { timeout: 45_000 },
    );
    const review = card.getByRole('link', { name: 'Review delegation' });
    await expect(review).toBeVisible();

    // 2. An editor with access approves, in the dialog the notice opens.
    await review.click();
    const dialog = page.getByRole('dialog', { name: `What ${specialist.name} will be able to use` });
    await expect(dialog).toBeVisible({ timeout: 45_000 });
    await dialog.getByRole('button', { name: 'Approve', exact: true }).click();
    await expect(dialog).toBeHidden({ timeout: 30_000 });
    const approved = await grantOf(page, projectId, parent, specialist);
    expect(approved.state).toBe('approved');
    expect(approved.digest, 'an approved grant is pinned to a digest').toMatch(/^[0-9a-f]{64}$/);

    // The same request now runs.
    await clearMockLlmJournal(page);
    const task2 = `${AUTOTEST_PREFIX}commit${String(stampOf())} report ready`;
    const ran = await runTurn(page, cleanup, projectId, caller, parent, delegate(specialist, task2));
    expect(ran.reply, 'the specialist\'s own answer must come back through the parent').toContain(task2);
    expect(ran.reply).not.toContain(REFUSAL_CODE);
    expect(ran.notices.filter((row) => row.kind === 'refused'), 'an approved delegation shows no refusal').toEqual([]);
    expect(
      (await requestForTask(page, task2))?.tools ?? [],
      'the specialist must be offered the toolkit its grant lists, which its caller does not hold',
    ).toContain(git.readOperation);
    const steps = await delegationStepsOf(page, projectId, ran.conversation);
    const recorded = steps.find((step) => step.summary['delegation_grant_id'] !== undefined);
    expect(recorded, `the delegation step must record the grant: ${JSON.stringify(steps)}`).toBeDefined();
    expect(recorded?.summary['delegation_grant_id']).toBe(approved.id);
    expect(recorded?.summary['delegation_grant_digest']).toBe(approved.digest);
  } finally {
    await cleanup.run();
  }
});

test('J2: a toolkit added to the specialist leaves its grant stale, runs without it, notifies once, and re-approval restores it', async ({
  page,
}) => {
  test.setTimeout(720_000);
  await clearMockLlmJournal(page);
  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);
  const cleanup = new Cleanup(page, projectId);
  try {
    const stamp = stampOf();
    const first = cleanup.toolkit(await createToolkit(page, projectId, 'first', 'a'));
    const second = cleanup.toolkit(await createToolkit(page, projectId, 'second', 'b'));
    const specialist = cleanup.agent(await createAgent(page, projectId, `${AUTOTEST_PREFIX}dgdspec-${String(stamp)}`));
    const parent = cleanup.agent(await createAgent(page, projectId, `${AUTOTEST_PREFIX}dgdpar-${String(stamp)}`));
    await attachToolkit(page, projectId, first, specialist);
    await attachChild(page, projectId, parent, specialist);
    const approved = await approveByApi(page, projectId, parent, specialist);

    // The specialist gains a second toolkit after the approval.
    await attachToolkit(page, projectId, second, specialist);
    // Reading the caller's grants runs the drift check (and so does a write, shortly after it).
    await expect
      .poll(async () => (await grantOf(page, projectId, parent, specialist)).state, {
        timeout: 60_000,
        message: 'the grant must go stale when its callee gains a toolkit',
      })
      .toBe('stale');
    const stale = await grantOf(page, projectId, parent, specialist);
    expect(stale.id, 'a stale grant is the same grant, still covering what it recorded').toBe(approved.id);
    expect(stale.drift?.added?.map((item) => item.name)).toContain(`toolkit:${second.id}`);

    // The next run uses only what was approved: the second toolkit is narrowed out and shown.
    await clearMockLlmJournal(page);
    const task = `${AUTOTEST_PREFIX}drift${String(stampOf())} report ready`;
    const narrowed = await runTurn(page, cleanup, projectId, caller, parent, delegate(specialist, task));
    expect(narrowed.reply, 'the specialist still runs, on the part that was approved').toContain(task);
    const tools = (await requestForTask(page, task))?.tools ?? [];
    expect(tools, 'the approved toolkit is offered').toContain(first.readOperation);
    expect(tools, 'the toolkit added after the approval must be narrowed out').not.toContain(second.readOperation);
    const notice = narrowed.notices.find((row) => row.kind === 'narrowed');
    expect(notice, 'the answer must say what it ran without').toBeDefined();
    expect(notice).toMatchObject({ kind: 'narrowed', reason: 'stale', grant_id: approved.id });
    await page.goto(`${BASE_URL}/app/chat/${narrowed.conversation.id}`);
    await expect(page.getByTestId('delegation-notice').first()).toContainText(`${specialist.name} ran without`, { timeout: 45_000 });

    // The editors are told ONCE, however many times the drift is read or run.
    const readTold = async (): Promise<number> => {
      const notifications = await page.request.get(`${API}/notifications/notifications/prompt_lib/${projectId}?limit=1000`);
      expect(notifications.status(), `read notifications: ${(await notifications.text()).slice(0, 300)}`).toBe(200);
      const rows =
        ((await notifications.json()) as { rows?: readonly { event_type?: string; meta?: Record<string, unknown> }[] }).rows ?? [];
      return rows.filter(
        (row) => row.event_type === 'delegation_grant_stale' && String(row.meta?.['caller_application_id']) === parent.id,
      ).length;
    };
    await expect.poll(readTold, { timeout: 60_000, message: 'the editors must be told that the grant went stale' }).toBeGreaterThan(0);
    // Read the grants again (the check runs on a read) and let a duplicate show itself before the count is judged.
    await readGrants(page, projectId, parent);
    await page.waitForTimeout(3_000);
    expect(await readTold(), 'exactly one delegation_grant_stale notification for this edge and digest').toBe(1);

    // Re-approval in the dialog restores it, and keeps the old grant as superseded.
    const dialog = await openReview(page, parent, specialist);
    await dialog.getByRole('button', { name: 'Re-approve', exact: true }).click();
    await expect(dialog).toBeHidden({ timeout: 30_000 });
    const restored = await grantOf(page, projectId, parent, specialist);
    expect(restored.state).toBe('approved');
    expect(restored.id, 'a re-approval writes a new grant').not.toBe(approved.id);
    expect(restored.digest).not.toBe(approved.digest);
    expect(
      (await readGrants(page, projectId, parent, true)).find((row) => row.id === approved.id)?.state,
      'the old grant is kept as superseded',
    ).toBe('superseded');

    await clearMockLlmJournal(page);
    const task2 = `${AUTOTEST_PREFIX}drift${String(stampOf())} report ready`;
    const after = await runTurn(page, cleanup, projectId, caller, parent, delegate(specialist, task2));
    expect(after.reply).toContain(task2);
    expect(after.notices, 'a restored grant narrows nothing').toEqual([]);
    expect((await requestForTask(page, task2))?.tools ?? [], 'both toolkits are offered again').toEqual(
      expect.arrayContaining([first.readOperation, second.readOperation]),
    );
  } finally {
    await cleanup.run();
  }
});

test('J3: approving the first edge does not carry to the specialist\'s own sub-agent, which needs its own edge', async ({ page }) => {
  test.setTimeout(720_000);
  await clearMockLlmJournal(page);
  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);
  const cleanup = new Cleanup(page, projectId);
  try {
    const stamp = stampOf();
    const middleKit = cleanup.toolkit(await createToolkit(page, projectId, 'mid', 'm'));
    const deepKit = cleanup.toolkit(await createToolkit(page, projectId, 'deep', 'g'));
    const deep = cleanup.agent(await createAgent(page, projectId, `${AUTOTEST_PREFIX}dgz-${String(stamp)}`));
    const middle = cleanup.agent(await createAgent(page, projectId, `${AUTOTEST_PREFIX}dgy-${String(stamp)}`));
    const top = cleanup.agent(await createAgent(page, projectId, `${AUTOTEST_PREFIX}dgx-${String(stamp)}`));
    await attachToolkit(page, projectId, deepKit, deep);
    await attachToolkit(page, projectId, middleKit, middle);
    await attachChild(page, projectId, middle, deep);
    await attachChild(page, projectId, top, middle);
    // Only the first edge (top to middle) is approved.
    await approveByApi(page, projectId, top, middle);

    const deepTask = `${AUTOTEST_PREFIX}deep${String(stampOf())} report ready`;
    const prompt = callToolWithArgumentsPrompt(
      toolNameOf(middle),
      { task: delegate(deep, deepTask) },
      'delegate this and quote the answer',
    );
    const refused = await runTurn(page, cleanup, projectId, caller, top, prompt);
    expect(refused.reply, 'the call that reaches the specialist\'s own sub-agent is refused').toContain(REFUSAL_CODE);
    expect(refused.reply).not.toContain(deepTask);
    expect(await requestForTask(page, deepTask), 'the deep agent ran without an approved edge').toBeUndefined();
    const notice = refused.notices.find((row) => row.kind === 'refused');
    expect(notice, `the refusal must be shown, naming the second edge: ${JSON.stringify(refused.notices)}`).toBeDefined();
    expect(notice?.edge.caller_id, 'the refused edge starts at the middle agent, not at the top one').toBe(Number(middle.id));
    expect(notice?.edge.callee_id).toBe(Number(deep.id));

    // Its own approval, and the same chain runs.
    await approveByApi(page, projectId, middle, deep);
    await clearMockLlmJournal(page);
    const deepTask2 = `${AUTOTEST_PREFIX}deep${String(stampOf())} report ready`;
    const prompt2 = callToolWithArgumentsPrompt(
      toolNameOf(middle),
      { task: delegate(deep, deepTask2) },
      'delegate this and quote the answer',
    );
    const ran = await runTurn(page, cleanup, projectId, caller, top, prompt2);
    expect(ran.reply, 'the deep agent\'s answer comes back through both hops').toContain(deepTask2);
    expect((await requestForTask(page, deepTask2))?.tools ?? [], 'the deep agent holds what its own edge approved').toContain(
      deepKit.readOperation,
    );
  } finally {
    await cleanup.run();
  }
});

test('J4: report runs an unapproved delegation and records it, enforce refuses it', async ({ page }) => {
  test.setTimeout(720_000);
  await clearMockLlmJournal(page);
  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);
  const cleanup = new Cleanup(page, projectId);
  const original = await delegationSection();
  try {
    const stamp = stampOf();
    const kit = cleanup.toolkit(await createToolkit(page, projectId, 'rep', 'r'));
    const specialist = cleanup.agent(await createAgent(page, projectId, `${AUTOTEST_PREFIX}dgrspec-${String(stamp)}`));
    const parent = cleanup.agent(await createAgent(page, projectId, `${AUTOTEST_PREFIX}dgrpar-${String(stamp)}`));
    await attachToolkit(page, projectId, kit, specialist);
    await attachChild(page, projectId, parent, specialist);
    expect((await grantOf(page, projectId, parent, specialist)).state).toBe('pending');

    await delegationSection({ mode: 'report' });
    const task = `${AUTOTEST_PREFIX}report${String(stampOf())} report ready`;
    const reported = await runTurn(page, cleanup, projectId, caller, parent, delegate(specialist, task));
    expect(reported.reply, 'in report mode the unapproved delegation runs').toContain(task);
    expect(reported.reply).not.toContain(REFUSAL_CODE);
    expect(
      (await requestForTask(page, task))?.tools ?? [],
      'the specialist ran with its toolkit although nothing was approved',
    ).toContain(kit.readOperation);
    const steps = await delegationStepsOf(page, projectId, reported.conversation);
    const recorded = steps.find((step) => (step.summary['delegation_refusal'] as { code?: string } | undefined)?.code === REFUSAL_CODE);
    expect(recorded, `report must record the missing grant on the step: ${JSON.stringify(steps)}`).toBeDefined();
    expect(recorded?.summary['delegation_enforced'], 'recorded, and not enforced').toBe(false);
    expect(reported.notices.filter((row) => row.kind === 'refused'), 'nothing was refused, so no refusal is shown').toEqual([]);

    await delegationSection({ mode: 'enforce' });
    await clearMockLlmJournal(page);
    const task2 = `${AUTOTEST_PREFIX}enforce${String(stampOf())} report ready`;
    const enforced = await runTurn(page, cleanup, projectId, caller, parent, delegate(specialist, task2));
    expect(enforced.reply, 'enforce refuses the same delegation').toContain(REFUSAL_CODE);
    expect(enforced.reply).not.toContain(task2);
    expect(await requestForTask(page, task2)).toBeUndefined();
    expect(enforced.notices.some((row) => row.kind === 'refused' && row.reason === 'no_grant')).toBe(true);
  } finally {
    // The section is platform-wide state shared by every journey on this stack.
    await cleanup.run();
    await delegationSection({ mode: original.mode });
  }
});
