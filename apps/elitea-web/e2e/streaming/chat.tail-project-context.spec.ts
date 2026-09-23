/**
 * Project Context INJECTION — the half `chat.project-context-injection.spec.ts`
 * says it does not prove: that the content saved in Settings > Project Context
 * actually reaches a turn's system prompt, that the per-agent "Ignore Project
 * Context" toggle keeps it out, and that a sub-agent never sees it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE GATE THESE TESTS USED TO DIE ON, AND WHAT REPLACED IT (#946)
 * ─────────────────────────────────────────────────────────────────────────────
 * Every test here was fail-marked, and they all failed at the same line — the
 * FIRST turn, not the injection assertion after it. Project Context was not
 * injected anywhere in the Go path: it was read-only display data whose only
 * runtime effect was an admission-time REFUSAL, spelled out in
 * `internal/db/queries/agent_chat.sql` as
 *
 *   AND NOT EXISTS (
 *       SELECT 1 FROM configuration AS project_context
 *       WHERE project_context.type = 'project_context'
 *         AND COALESCE(project_context.data ->> 'enabled', 'true') = 'true'
 *         AND COALESCE(project_context.data ->> 'content', '')  <> ''
 *   )
 *
 * — so a project whose context was enabled AND non-empty resolved ZERO rows for
 * every turn in it, and every send answered 422. The clause was duplicated in
 * all four resolve/insert variants, so it was every current-path entry point,
 * not one.
 *
 * That clause is gone from all four, and the content is INJECTED instead. The
 * start path reads the project's row and splices it onto the turn's
 * `instructions` as a delimited `<project_context>` block — the same field
 * recalled memories ride, which both workers already fold into the system
 * prompt (`internal/application/agentexecution/projectcontext.go`, and the
 * native runtime's `assembly.rs`). The per-agent "Ignore Project Context"
 * toggle is consulted there, off the frozen version's own `meta`, and a child
 * agent is never touched because a delegated child resolves its OWN version's
 * instructions inside the worker.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE SYSTEM PROMPT AND NOT THE ANSWER
 * ─────────────────────────────────────────────────────────────────────────────
 * The mock's reply echoes the last USER message, so "the agent returned the
 * phrase" is unreadable from the answer — it would read back the question.
 * `MockLlmJournalEntry.instructions` is the system prompt the request actually
 * carried, which is the only server-side, model-independent evidence of what
 * the runtime assembled. Same technique as `chat.longTermMemory.spec.ts` and
 * `chat.variables.spec.ts`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE LEG CONTRACT: BOTH (rust and python)
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing here is worker-shaped. The gate is in elitea-main's admission SQL,
 * upstream of either runtime, and the assertion past it reads the system
 * prompt out of the shared `application` field both workers decode.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE ROW IS PROJECT-WIDE, AND IT IS RESTORED IN A `finally`
 * ─────────────────────────────────────────────────────────────────────────────
 * There is exactly one `project_context` row per project, and an enabled one
 * refuses every turn every OTHER `chat-stream` spec sends. Leaving one behind
 * would not fail this file — it would fail the rest of the project, minutes
 * later, with an unrelated-looking 422. So each test restores the prior state
 * unconditionally, and the restore is what makes these tests safe to run on
 * the shared stack at all.
 *
 * THE RESTORE IS NOT ENOUGH ON ITS OWN, AND THAT IS WORTH STATING. While a
 * test HOLDS the row enabled, any other spec sending a turn in the same
 * personal project is refused too. `chat-stream` is serial —
 * `scripts/chat-stream-e2e.sh` passes `--workers=1`, and its own comment says
 * that pin "is the thing that makes this project serial" — so nothing else is
 * sending during that window. Measured the other way round: an ad-hoc
 * `npx playwright test --project=chat-stream` WITHOUT `--workers=1` refused a
 * concurrent `chat.tail-trace-steps.spec.ts` turn with exactly that 422. Run
 * this project through the script, or pass `--workers=1` by hand.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  agentAsToolName,
  callToolWithArgumentsPrompt,
  createAgentWithVersion,
  fillComposer,
  readCallerIdentity,
  readCallerPersonalProjectId,
  readMockLlmJournal,
  clearMockLlmJournal,
} from '../fixtures/api';

const API = `${BASE_URL}/api/v2`;

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The model the standalone stack seeds; overridable for the real-model lane. */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

interface ProjectContextBody {
  readonly content?: string;
  readonly enabled?: boolean;
}

function projectContextUrl(projectId: string): string {
  return `${API}/elitea_core/project_context/prompt_lib/${projectId}/project-context`;
}

/** Write the project's single context row, and require the write to land. */
async function setProjectContext(
  page: Page,
  projectId: string,
  body: ProjectContextBody,
): Promise<void> {
  const saved = await page.request.put(projectContextUrl(projectId), { data: body });
  expect(
    saved.status(),
    `the project context must be writable: ${(await saved.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
  // Read back rather than trust the 200. The route ECHOED the body sent while
  // the write died in the database until #888, which is exactly the shape a
  // precondition must never be allowed to have — see
  // `chat.project-context-injection.spec.ts`'s header.
  const stored = (await (await page.request.get(projectContextUrl(projectId))).json()) as ProjectContextBody;
  expect(stored.content ?? '', 'the project context stored is not the one that was written').toBe(
    body.content ?? '',
  );
  expect(stored.enabled ?? false, 'the project context toggle stored is not the one that was written').toBe(
    body.enabled ?? false,
  );
}

interface AgentFixture {
  readonly projectId: string;
  readonly agentId: string;
  readonly versionId: string;
  readonly name: string;
}

/** An agent pinned to the deterministic model, optionally ignoring project context. */
async function createAgent(
  page: Page,
  projectId: string,
  label: string,
  ignoreProjectContext = false,
  instructions = 'You are an autotest agent. Answer the question you are asked.',
): Promise<AgentFixture> {
  const name = `${AUTOTEST_PREFIX}pctx-${label}-${String(Date.now() % 1_000_000)}`;
  const agent = await createAgentWithVersion(
    page.request,
    name,
    {
      instructions,
      model: { modelName: MOCK_MODEL },
      // The Advanced panel's own key — `AgentVersionMeta.ignore_project_context`
      // (`src/features/agents/model/types.ts`), written through the same
      // version route the panel's Save uses.
      ...(ignoreProjectContext ? { meta: { ignore_project_context: true } } : {}),
    },
    projectId,
  );
  return { projectId, agentId: agent.id, versionId: agent.versionId, name };
}

/** A conversation the agent participates in, opened in the browser. */
async function openConversation(
  page: Page,
  projectId: string,
  callerId: string,
  agents: readonly AgentFixture[],
  label: string,
): Promise<string> {
  const created = await page.request.post(`${API}/elitea_core/conversations/prompt_lib/${projectId}`, {
    data: { name: `${AUTOTEST_PREFIX}pctxconv-${label}-${String(Date.now() % 1_000_000)}`, is_private: true },
  });
  expect(created.status(), `the conversation must be created: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const conversationId = String(((await created.json()) as { id?: unknown }).id ?? '');
  expect(conversationId, 'the created conversation must carry an id').not.toBe('');

  // BOTH participant kinds. A conversation POSTed straight to the API carries
  // none, and its first send 422s at admission because the resolver joins on
  // them — see `chat.pyodide-sandbox.spec.ts`'s header for the measurement.
  const participants = await page.request.post(
    `${API}/elitea_core/participants/prompt_lib/${projectId}/${conversationId}`,
    {
      data: [
        ...agents.map((agent) => ({
          entity_name: 'application',
          entity_meta: { id: agent.agentId, project_id: Number(projectId), name: agent.name },
          entity_settings: { version_id: agent.versionId },
        })),
        { entity_name: 'user', entity_meta: { id: Number(callerId) } },
      ],
    },
  );
  expect(
    participants.status(),
    `the participants must be added: ${(await participants.text()).slice(0, 300)}`,
  ).toBe(200);

  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 45_000 });
  return conversationId;
}

/**
 * Send one prompt through the composer and require the start to be ADMITTED.
 *
 * THIS is the line every test in this file used to fail on, and the reason the
 * failure message still names #946: the refusal was a 422 from the admission
 * SQL, not a later assertion about the prompt. Keeping the assertion here means
 * a regression of the gate is reported as itself rather than as "the phrase
 * never reached the prompt".
 */
async function sendTurn(page: Page, prompt: string): Promise<void> {
  const send = await fillComposer(page, prompt);
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  await send.click();
  const response = await started;
  expect(
    response.status(),
    'the turn was refused while a project context was enabled — the #946 admission gate is back (it ' +
      '422d every turn in a project whose context row is enabled and non-empty): ' +
      `${(await response.text()).slice(0, 300)}`,
  ).toBe(200);
}

/** Every system prompt the mock has been sent since the journal was cleared. */
async function systemPrompts(page: Page): Promise<readonly string[]> {
  return (await readMockLlmJournal(page)).map((entry) => entry.instructions);
}

/** Delete the agents a test made. Best effort, deliberately last. */
async function deleteAgents(page: Page, agents: readonly AgentFixture[]): Promise<void> {
  for (const agent of agents) {
    await page.request.delete(`${API}/elitea_core/application/prompt_lib/${agent.projectId}/${agent.agentId}`);
  }
}

/* onetest: ELITEA-0951, ELITEA-0943 (its run half) — Project Context saved with the toggle ON
 * reaches the system prompt of an agent run in its own conversation, and stops reaching it when the
 * toggle is turned OFF. */
test('Project Context reaches the system prompt of an agent’s own turn', async ({ page }) => {
  test.setTimeout(300_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona must work inside its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);
  const prior = (await (await page.request.get(projectContextUrl(projectId))).json()) as ProjectContextBody;

  const agents: AgentFixture[] = [];
  try {
    const phrase = `${AUTOTEST_PREFIX}QA-TEST-PHRASE ${String(Date.now() % 1_000_000)}: Green heron dives at twilight`;
    await setProjectContext(page, projectId, { content: phrase, enabled: true });

    const agent = await createAgent(page, projectId, 'own');
    agents.push(agent);
    await openConversation(page, projectId, caller.id, [agent], 'on');

    await clearMockLlmJournal(page);
    await sendTurn(page, `${AUTOTEST_PREFIX}What is the QA-TEST-PHRASE?`);
    await expect
      .poll(async () => (await systemPrompts(page)).some((text) => text.includes(phrase)), {
        timeout: 120_000,
        message: 'the enabled project context never reached the system prompt the model was sent',
      })
      .toBe(true);

    // ── The toggle OFF ──────────────────────────────────────────────────────
    //
    // A NEW conversation: the context is read at admission, and reusing the
    // first one would make this assertion depend on when the second turn was
    // admitted relative to the write.
    await setProjectContext(page, projectId, { content: phrase, enabled: false });
    await openConversation(page, projectId, caller.id, [agent], 'off');
    await clearMockLlmJournal(page);
    await sendTurn(page, `${AUTOTEST_PREFIX}What is the QA-TEST-PHRASE now?`);
    await expect
      .poll(async () => (await systemPrompts(page)).length, {
        timeout: 120_000,
        message: 'the second turn never reached the model at all',
      })
      .toBeGreaterThan(0);
    expect(
      (await systemPrompts(page)).filter((text) => text.includes(phrase)),
      'a DISABLED project context must not reach the prompt',
    ).toEqual([]);
  } finally {
    await setProjectContext(page, projectId, {
      content: prior.content ?? '',
      enabled: prior.enabled ?? false,
    });
    await deleteAgents(page, agents);
  }
});

/* onetest: ELITEA-0948 — Project Context is injected for an agent added as a participant to a Chat
 * page conversation, both for a newly created agent and for one that already existed. */
test('Project Context reaches an agent added as a Chat participant', async ({ page }) => {
  test.setTimeout(300_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona must work inside its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);
  const prior = (await (await page.request.get(projectContextUrl(projectId))).json()) as ProjectContextBody;

  const agents: AgentFixture[] = [];
  try {
    const phrase = `${AUTOTEST_PREFIX}QA-TEST-PHRASE ${String(Date.now() % 1_000_000)}: Red kite soars at midday`;

    // The "previously created" agent of the case's fourth step: made BEFORE
    // the context exists, so nothing about it can have been frozen against a
    // context that was not there yet.
    const existing = await createAgent(page, projectId, 'existing');
    agents.push(existing);

    await setProjectContext(page, projectId, { content: phrase, enabled: true });

    const fresh = await createAgent(page, projectId, 'fresh');
    agents.push(fresh);

    for (const agent of [fresh, existing]) {
      await openConversation(page, projectId, caller.id, [agent], agent === fresh ? 'fresh' : 'existing');
      await clearMockLlmJournal(page);
      await sendTurn(page, `${AUTOTEST_PREFIX}What is the QA-TEST-PHRASE?`);
      await expect
        .poll(async () => (await systemPrompts(page)).some((text) => text.includes(phrase)), {
          timeout: 120_000,
          message: `the project context never reached the system prompt of ${agent.name}`,
        })
        .toBe(true);
    }
  } finally {
    await setProjectContext(page, projectId, {
      content: prior.content ?? '',
      enabled: prior.enabled ?? false,
    });
    await deleteAgents(page, agents);
  }
});

/* onetest: ELITEA-0945 — the per-agent "Ignore Project Context" toggle keeps the context out of
 * THAT agent's prompt, and does not affect any other agent in the same project. */
test('the per-agent Ignore Project Context toggle keeps the context out of its own prompt', async ({
  page,
}) => {
  test.setTimeout(300_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona must work inside its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);
  const prior = (await (await page.request.get(projectContextUrl(projectId))).json()) as ProjectContextBody;

  const agents: AgentFixture[] = [];
  try {
    const phrase = `${AUTOTEST_PREFIX}QA-TEST-PHRASE ${String(Date.now() % 1_000_000)}: Golden crane flies at noon`;
    await setProjectContext(page, projectId, { content: phrase, enabled: true });

    const plain = await createAgent(page, projectId, 'plain', false);
    const ignoring = await createAgent(page, projectId, 'ignoring', true);
    agents.push(plain, ignoring);

    // The toggle PERSISTS — asserted on the stored version rather than on the
    // checkbox, because a toggle that renders checked and was never written is
    // exactly the failure the case's "reload the page and check" step names.
    const stored = await page.request.get(`${API}/elitea_core/application/prompt_lib/${projectId}/${ignoring.agentId}`);
    const meta =
      ((await stored.json()) as { version_details?: { meta?: Record<string, unknown> } }).version_details?.meta ??
      {};
    expect(meta['ignore_project_context'], 'the Ignore toggle must persist onto the version').toBe(true);

    await openConversation(page, projectId, caller.id, [plain], 'plain');
    await clearMockLlmJournal(page);
    await sendTurn(page, `${AUTOTEST_PREFIX}What is the QA-TEST-PHRASE?`);
    await expect
      .poll(async () => (await systemPrompts(page)).some((text) => text.includes(phrase)), {
        timeout: 120_000,
        message: 'the agent WITHOUT the toggle must still receive the context',
      })
      .toBe(true);

    await openConversation(page, projectId, caller.id, [ignoring], 'ignoring');
    await clearMockLlmJournal(page);
    await sendTurn(page, `${AUTOTEST_PREFIX}What is the QA-TEST-PHRASE?`);
    await expect
      .poll(async () => (await systemPrompts(page)).length, {
        timeout: 120_000,
        message: 'the ignoring agent’s turn never reached the model at all',
      })
      .toBeGreaterThan(0);
    expect(
      (await systemPrompts(page)).filter((text) => text.includes(phrase)),
      'an agent with Ignore Project Context ON must not receive the context',
    ).toEqual([]);
  } finally {
    await setProjectContext(page, projectId, {
      content: prior.content ?? '',
      enabled: prior.enabled ?? false,
    });
    await deleteAgents(page, agents);
  }
});

/* onetest: ELITEA-0952 — Project Context is injected for the MASTER agent only: a sub-agent it
 * delegates to receives none of it. */
test('Project Context is not passed down to a sub-agent', async ({ page }) => {
  test.setTimeout(420_000);

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona must work inside its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);
  const prior = (await (await page.request.get(projectContextUrl(projectId))).json()) as ProjectContextBody;

  const agents: AgentFixture[] = [];
  try {
    const phrase = `${AUTOTEST_PREFIX}QA-TEST-PHRASE ${String(Date.now() % 1_000_000)}: Azure falcon rests at midnight`;
    await setProjectContext(page, projectId, { content: phrase, enabled: true });

    // WHICH JOURNAL ENTRY IS THE CHILD'S. Both agents answer the same mock in
    // the same turn, and the entries carry no agent id — only the system
    // prompt the request was built from. So the child's own instructions carry
    // a sentence nothing else in this run says, and the assertions below read
    // the child's hop by that sentence rather than by counting hops. Counting
    // cannot work: the parent is asked TWICE (the tool call, then the
    // continuation that quotes the result), so "the prompt carrying the phrase
    // appears once" would fail on a perfectly correct run.
    const childSentinel = `${AUTOTEST_PREFIX}CHILD-AGENT-MARK-${String(Date.now() % 1_000_000)}`;
    const child = await createAgent(
      page,
      projectId,
      'child',
      false,
      `You are the sub-agent. ${childSentinel}. Answer the task you are given.`,
    );
    const parent = await createAgent(page, projectId, 'parent');
    agents.push(parent, child);

    // The child attached to the parent VERSION — the same relation route the
    // agent page's picker sends (`chat.delegation.spec.ts`'s `attachChild`).
    const attached = await page.request.patch(
      `${API}/elitea_core/application_relation/prompt_lib/${projectId}/${child.agentId}/${child.versionId}`,
      {
        data: {
          application_id: Number(parent.agentId),
          version_id: Number(parent.versionId),
          has_relation: true,
        },
      },
    );
    expect(
      attached.status(),
      `the child must attach to the parent version: ${(await attached.text()).slice(0, 300)}`,
    ).toBeLessThan(300);

    await openConversation(page, projectId, caller.id, [parent], 'master');
    await clearMockLlmJournal(page);
    // THE DELEGATION IS SCRIPTED, NOT HOPED FOR. The offline mock echoes the
    // last user message and chooses nothing, so a plain question produces one
    // hop and no child turn — which is exactly how this test failed on BOTH
    // legs (python AND rust, run 35321502745: "the delegation never produced a
    // second model hop"). `[[mock:call_tool …]]` is the repository's existing
    // answer to that (`chat.delegation.spec.ts`'s header carries the full
    // argument): the marker scripts the call the model emits and the runtime
    // then does the delegating for real — resolve, compile, run the child as a
    // turn of its own against the same mock, feed the result back. Only the
    // parent model's DECISION is faked; the hop, and therefore the child's own
    // system prompt, is real. The tool name is leg-aware — the native runtime
    // binds `elitea_agent_<id>_v_<version>` and the SDK worker the agent's
    // name — which `agentAsToolName` resolves from `E2E_WORKER`.
    await sendTurn(
      page,
      callToolWithArgumentsPrompt(
        agentAsToolName({ agentId: child.agentId, versionId: child.versionId, name: child.name }),
        { task: `${AUTOTEST_PREFIX}What is the QA-TEST-PHRASE?` },
        `${AUTOTEST_PREFIX}What is the QA-TEST-PHRASE? Delegate and quote the answer.`,
      ),
    );

    await expect
      .poll(async () => (await systemPrompts(page)).some((text) => text.includes(phrase)), {
        timeout: 180_000,
        message: 'the master agent never received the project context',
      })
      .toBe(true);

    // The CHILD's own hop, identified by the child's instructions rather than
    // by position or count: it is the only request built from them.
    await expect
      .poll(async () => (await systemPrompts(page)).some((text) => text.includes(childSentinel)), {
        timeout: 180_000,
        message:
          'the delegation never produced the sub-agent’s own model hop, so the child’s prompt was never observed',
      })
      .toBe(true);

    const prompts = await systemPrompts(page);
    expect(
      prompts.filter((text) => text.includes(childSentinel) && text.includes(phrase)),
      'the project context reached the sub-agent’s own prompt — a sub-agent must not inherit it',
    ).toEqual([]);
  } finally {
    await setProjectContext(page, projectId, {
      content: prior.content ?? '',
      enabled: prior.enabled ?? false,
    });
    await deleteAgents(page, agents);
  }
});
