/**
 * THE TWO CHAT-AUTHORED BUILDER MODULES, end to end (#940 A8).
 *
 * `skills_builder` and `project_context_builder` are internal tools the
 * NATIVE runtime implements — not skips — so a turn that enables one gives the
 * model a tool that WRITES a product entity into the conversation's own
 * project: a Skill, or that project's Project Context.
 *
 * WHAT ONLY A TURN CAN SHOW. Five independently-correct pieces have to line up
 * and nothing below a real turn touches more than one of them:
 *
 *  1. the version's `meta.internal_tools` has to SURVIVE the save — the web
 *     catalogue, the Go forwarding map, the Rust catalogue and the nine SQL
 *     admission sites all name these two now, and any one of them missing the
 *     name answers 422 on every send with a message that names neither the
 *     tool nor the runtime (`internal_tools_catalogue_drift_test.go` pins the
 *     four against each other; this proves the chain at run time);
 *  2. the runtime has to BIND a tool rather than log
 *     `agent_internal_tool_skipped` — the fate of every other catalogue name
 *     on this worker;
 *  3. the tool has to reach main at all. It writes through the claim-bound
 *     private mTLS content listener, the runtime's only channel to tenant
 *     data, under the SAME durable claim that authorized the turn;
 *  4. main has to take the project from that CLAIM and from nothing in the
 *     request — the property that makes a write on that listener safe at all;
 *  5. the row has to be the one the product reads back, through the ordinary
 *     public route a user's browser uses.
 *
 * So every assertion below is taken SERVER-SIDE, through the public read
 * routes, after the turn: a screen that renders a confident answer is exactly
 * what a tool that was never bound also produces.
 *
 * WORKER: the turns run on the NATIVE runtime only. This is deliberate and is
 * not a gap being papered over — the Python SDK worker has no port of either
 * module and this programme excludes the SDK, so `GET
 * /elitea_core/runtime_capabilities` reports both as unavailable there and the
 * UI renders the toggles disabled with that reason. The python leg asserts
 * exactly that, through the last test in this file, rather than listing
 * nothing.
 *
 * STATUS: RUN, on the standalone stack with the native runtime, 2026-09-16.
 * What it measured, recorded here because each finding is a different KIND of
 * thing and only one of them is a product defect:
 *
 *  - SKILLS BUILDER WORKS, end to end: the module survives the save, the
 *    runtime binds `create_or_update_skill`, the write reaches main over the
 *    claim-bound listener, and the row (name, description, and a `base`
 *    version carrying the instructions) is readable through the public route.
 *    The first run failed anyway, on THIS FILE: the list route answers
 *    `{items: …}` and the helper read `rows`, so a correct write read back as
 *    "no such skill".
 *  - PROJECT CONTEXT BUILDER WRITES, and that write then disables the
 *    project's chat. The second turn is refused 422
 *    `unsupported_agent_execution`, because the current-path resolver excludes
 *    any project carrying an ENABLED project context (project-context
 *    injection into the system prompt is unported). ELITEA-2787's update half
 *    is therefore a stated product gap below, not a passing assertion — and
 *    every Project Context test here clears the row in a `finally`, because a
 *    row left behind refuses every LATER chat-stream spec's turn in the same
 *    project.
 *
 * The pieces this file spans are also proven separately where they can be: the
 * write routes and their claim-scoped tenancy in
 * `services/elitea-main/internal/infra/storage/runtime_entity_builder_test.go`,
 * the catalogue/binding rules in
 * `services/elitea-worker-rust/src/agents/internal_tools.rs`'s own tests, and
 * the admission chain in `internal_tools_catalogue_drift_test.go`.
 */
import { expect, test } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { AUTOTEST_PREFIX, createAgentThroughForm } from '../fixtures/api';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/**
 * The mock's argument-carrying tool-call marker
 * (`deploy/mock-llm/server.py`'s `[[mock:call_tool <toolName> {json}]]`).
 *
 * The ARGUMENT form is mandatory here, not a convenience: both builder tools
 * declare required fields, and the short form's empty object is refused by the
 * runtime before the tool runs — the same reason the delegation journeys name
 * the tool and the task together.
 */
function callTool(toolName: string, args: Record<string, unknown>): string {
  return `[[mock:call_tool ${toolName} ${JSON.stringify(args)}]]`;
}

/** The tool names the native runtime binds for the two modules (`internal_tools.rs`). */
const SKILL_TOOL = 'create_or_update_skill';
const PROJECT_CONTEXT_TOOL = 'write_project_context';

/** The module names the version stores (`INTERNAL_TOOLS_LIST`). */
const SKILLS_BUILDER = 'skills_builder';
const PROJECT_CONTEXT_BUILDER = 'project_context_builder';

/** Same pinning rule, and same reason, as `chat.hitl.spec.ts`: an unpinned version runs on the project default, which on a stack with a real provider is not the mock. */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

/**
 * Which runtime answers the turns. `scripts/chat-stream-e2e.sh` exports it;
 * the local default is the native dev stack, as everywhere else in this
 * directory (`chat.toolkit-hitl.spec.ts`).
 *
 * The header states the rule this gates: the two modules are a NATIVE-runtime
 * capability and the Python SDK worker has no port of either. So the python
 * leg does not drive the turns — it asserts the ABSENCE the product itself
 * reports, in the capability test below. A python run that silently listed
 * nothing at all would be the same "a gate that stopped gating" failure this
 * repository keeps paying for, which is why the absence is a TEST and not a
 * `test.describe` the leg skips past.
 */
const IS_NATIVE_RUNTIME = (process.env['E2E_WORKER'] ?? 'rust') === 'rust';

interface PreparedAgent {
  readonly projectId: string;
  readonly agentId: string;
  readonly conversationId: string;
}

/**
 * Authors an agent through the form, enables ONE builder module on its stored
 * version, pins the mock model, and opens a chat with it.
 *
 * `meta` is REPLACED wholesale by `UpdateVersion`, never merged, so what the
 * form stored is read back first and only `internal_tools` is changed —
 * writing a bare `{internal_tools: […]}` silently drops `step_limit`, which
 * the runtime reads.
 */
async function prepareAgentWithModule(
  page: import('@playwright/test').Page,
  name: string,
  moduleName: string,
): Promise<PreparedAgent> {
  const { projectId, agentId, versionId } = await createAgentThroughForm(page, name);

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
        meta: { ...storedMeta, internal_tools: [moduleName] },
        llm_settings: { model_name: MOCK_MODEL },
      },
    },
  );
  expect(
    saved.status(),
    `the version must accept ${moduleName}: ${(await saved.text()).slice(0, 300)}`,
  ).toBeLessThan(300);

  // What the SERVER stored, not what this test sent. An admission gate that
  // refuses the module answers 200 here and then refuses every send with a
  // 422 naming neither the module nor the runtime — the exact failure the
  // catalogue drift gate exists to prevent, measured here at run time.
  const readback = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
  );
  const detail = (await readback.json()) as {
    version_details?: { meta?: { internal_tools?: readonly string[] } };
  };
  expect(
    detail.version_details?.meta?.internal_tools ?? [],
    `${moduleName} must survive the save, or the agent has no builder tool to call`,
  ).toContain(moduleName);

  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversation = (await (await conversationCreated).json()) as { id?: string | number };
  const conversationId = String(conversation.id ?? '');
  expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
  await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 45_000 });

  return { projectId, agentId, conversationId };
}

/** Sends one message and asserts the START was accepted — a 422 here is the admission failure this journey is largely about. */
async function sendTurn(page: import('@playwright/test').Page, text: string): Promise<void> {
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 30_000 });
  await input.fill(text);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();
  const startResponse = await started;
  expect(
    startResponse.status(),
    `the builder turn was refused: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);
}

interface SkillRow {
  readonly id?: string | number;
  readonly name?: string;
  readonly description?: string;
}

async function findSkillByName(
  request: APIRequestContext,
  projectId: string,
  name: string,
): Promise<SkillRow | undefined> {
  const response = await request.get(`${BASE_URL}/api/v2/elitea_core/skills/prompt_lib/${projectId}`);
  if (!response.ok()) return undefined;
  // `items`, not `rows`. The skills list route answers
  // `{items, total, page, page_size, total_pages}`
  // (`services/elitea-main/internal/api/v2/skills/handler.go`'s `ListResponse`);
  // the first version of this helper read `rows`, which is the shape the
  // conversation and toolkit listings use. An absent key reads as an empty
  // list, so the helper answered "no such skill" for a skill the tool had
  // written correctly — the measured first failure of this file, and exactly
  // the "invisible data" shape that costs an investigation every time.
  const body = (await response.json()) as { items?: readonly SkillRow[] };
  return (body.items ?? []).find((row) => row.name === name);
}

/* onetest: ELITEA-2784 — a fully specified request creates the Skill with the
   fields it named, and no clarification is needed. ELITEA-2794 — the created
   Skill is NOT auto-added as a participant. The clarification HALF of the
   sibling cases (ELITEA-2785) is `chat.hitl.spec.ts`'s subject: the pause and
   its resume are `ask_user`'s, not this module's, and this module adds no
   branch to either. */
test('a chat turn with Skills Builder enabled creates the Skill it names, and does not attach it', async ({ page }) => {
  test.skip(
    !IS_NATIVE_RUNTIME,
    'the Python SDK worker has no port of `skills_builder`; the python leg asserts that absence ' +
      'through the capability route instead (see the last test in this file)',
  );
  // Two model round trips (the tool call and the answer quoting its result)
  // plus a create and a save. Every wait below is bounded well under this.
  test.setTimeout(360_000);

  const stamp = Date.now() % 1_000_000;
  const agentName = `${AUTOTEST_PREFIX}sb-${stamp}`;
  const skillName = `${AUTOTEST_PREFIX}skill-${stamp}`;
  const instructions = 'Extract blockers, decisions and action items as a bulleted list.';

  const { projectId, conversationId } = await prepareAgentWithModule(page, agentName, SKILLS_BUILDER);

  await sendTurn(
    page,
    `${callTool(SKILL_TOOL, {
      name: skillName,
      description: 'Summarizes daily standup notes into action items',
      instructions,
    })} make me that skill`,
  );

  // THE assertion, taken server-side. A turn whose tool was never bound
  // answers in the same shape and looks identical on screen.
  await expect
    .poll(async () => (await findSkillByName(page.request, projectId, skillName))?.name, {
      timeout: 180_000,
      message: 'the Skills Builder tool ran but no skill row exists in the conversation’s project',
    })
    .toBe(skillName);

  const skill = await findSkillByName(page.request, projectId, skillName);
  expect(skill?.description, 'the skill must carry the description the request gave').toBe(
    'Summarizes daily standup notes into action items',
  );

  // The CONTENT, read through the detail route the product uses — the list
  // row carries no instructions, and a skill created with an empty `base`
  // version renders blank everywhere with no error (the #611 shape).
  const detail = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/skill/prompt_lib/${projectId}/${String(skill?.id)}`,
  );
  expect(detail.ok(), 'the created skill must be readable').toBe(true);
  const detailBody = (await detail.json()) as {
    instructions?: string;
    version_details?: { instructions?: string };
  };
  expect(
    detailBody.version_details?.instructions ?? detailBody.instructions,
    'the skill’s base version must hold the instructions the request gave',
  ).toContain('blockers');

  // ELITEA-2794: creating an entity from chat must not silently join it to the
  // conversation. The tool writes a row and nothing else — asserted rather
  // than assumed, because "helpfully" attaching it is a plausible design and
  // the case explicitly forbids it.
  const conversation = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`,
  );
  const participants =
    ((await conversation.json()) as { participants?: readonly { entity_name?: string }[] }).participants ?? [];
  expect(
    participants.map((participant) => participant.entity_name),
    'a skill created from chat must not be added as a participant',
  ).not.toContain('skill');
});

/**
 * Both Project Context tests share this: the module's ONE action is a
 * project-wide write, so a run that leaves it in place changes what every
 * other `chat-stream` spec can do. See `chat.project-context-injection.
 * spec.ts`'s header for the same rule stated from the other side — and the
 * gap below for why it is not merely tidiness here: a project carrying an
 * ENABLED context cannot run an agent turn at all on this platform.
 */
async function clearProjectContext(page: import('@playwright/test').Page, projectId: string): Promise<void> {
  const restored = await page.request.put(projectContextUrl(projectId), {
    // EMPTY CONTENT, TOGGLE LEFT ON — and the second half is not an oversight.
    //
    // Emptying the CONTENT is what makes the row inert: every admission clause
    // that excludes a project for its context requires `content <> ''` as well
    // as `enabled = 'true'` (agent_chat.sql), and an empty context injects
    // nothing. Emptying it is therefore the whole of the cleanup.
    //
    // Writing `enabled: false` as well LOOKED tidier and quietly broke the
    // next run (measured): `WriteProjectContext` keeps a project's existing
    // toggle when a request does not state one, and only a project with NO row
    // at all gets `true`. A cleanup that turned the toggle off left the next
    // run's context written-but-disabled, and the `in effect` assertion below
    // failed for a reason that had nothing to do with the module. `true` with
    // empty content is the state a project that never had a row behaves as.
    data: { content: '', enabled: true },
  });
  expect(
    restored.ok(),
    'the project context must be cleared, or every later chat-stream turn in this project is refused 422',
  ).toBe(true);
}

function projectContextUrl(projectId: string): string {
  return `${BASE_URL}/api/v2/elitea_core/project_context/prompt_lib/${projectId}/project-context`;
}

async function readProjectContext(
  page: import('@playwright/test').Page,
  projectId: string,
): Promise<{ content?: string; enabled?: boolean }> {
  const response = await page.request.get(projectContextUrl(projectId));
  return response.ok() ? ((await response.json()) as { content?: string; enabled?: boolean }) : {};
}

/* onetest: ELITEA-2786 — a fully specified request creates the Project Context, in effect, from
   the turn itself. */
test('a chat turn with Project Context Builder enabled writes the project context', async ({ page }) => {
  test.skip(
    !IS_NATIVE_RUNTIME,
    'the Python SDK worker has no port of `project_context_builder`; the python leg asserts that ' +
      'absence through the capability route instead (see the last test in this file)',
  );
  test.setTimeout(360_000);

  const stamp = Date.now() % 1_000_000;
  const agentName = `${AUTOTEST_PREFIX}pcb-${stamp}`;
  const first = `${AUTOTEST_PREFIX}This project is a Python REST API for order management.`;

  const { projectId } = await prepareAgentWithModule(page, agentName, PROJECT_CONTEXT_BUILDER);
  // The pre-state this case describes, stated rather than inherited: a project
  // carrying no context of its own. `WriteProjectContext` keeps whatever
  // toggle the project already had (see `clearProjectContext`), so a leftover
  // disabled row from an earlier run would make the `in effect` assertion
  // below measure that leftover instead of this module.
  await clearProjectContext(page, projectId);

  try {
    await sendTurn(page, `${callTool(PROJECT_CONTEXT_TOOL, { content: first })} write that down`);
    await expect
      .poll(async () => (await readProjectContext(page, projectId)).content, {
        timeout: 180_000,
        message: 'the Project Context Builder tool ran but the project context was not written',
      })
      .toBe(first);
    // A context written from chat that is not in effect is indistinguishable,
    // from the user's side, from one that was never written.
    expect((await readProjectContext(page, projectId)).enabled, 'a first context must be in effect').toBe(true);
  } finally {
    // Unconditional, and not optional — see `clearProjectContext`.
    await clearProjectContext(page, projectId);
  }
});

/* onetest: ELITEA-2787 — a second request UPDATES the single existing context rather than creating
   a duplicate. Marked as a product gap below: the SECOND turn cannot run at all while the first
   context is in effect. */
test('a second Project Context Builder turn updates the context in place', async ({ page }) => {
  test.skip(
    !IS_NATIVE_RUNTIME,
    'the Python SDK worker has no port of `project_context_builder`; the python leg asserts that ' +
      'absence through the capability route instead (see the last test in this file)',
  );
  // ── MEASURED, on the standalone stack, 2026-09-16 ───────────────────────
  //
  // The first turn writes the context and enables it — that half is proven by
  // the test above, and by the row this one leaves behind. The SECOND turn is
  // then refused before it reaches the worker:
  //
  //   POST …/elitea_core/messages/prompt_lib/{project}/{conversation}
  //   422 {"error":"unsupported_agent_execution",
  //        "message":"This agent turn requires the current execution path."}
  //
  // The cause is one of the ~25 conditions in the current-path resolver
  // (`services/elitea-main/internal/db/queries/agent_chat.sql`, the
  // `NOT EXISTS (SELECT 1 FROM configuration AS project_context …
  //  WHERE type = 'project_context' AND enabled = 'true' AND content <> '')`
  // clause, present in every one of its resolve variants). Project-context
  // INJECTION into a turn's system prompt is not ported, so rather than run a
  // turn that would silently ignore the project's context, the resolver
  // returns no rows and the route answers 422.
  //
  // The consequence is larger than this case: the module's own successful
  // write makes EVERY later agent turn in that project impossible, including
  // the one that would update it. So this is not "the update is broken" — the
  // update route works (`chat.project-context-injection.spec.ts` proves the
  // round trip since #888) — it is that no second TURN can be admitted to ask
  // for it. `test.fail` rather than a skip, so the day injection lands this
  // test starts failing-as-passing and says so.
  test.fail(
    true,
    'ELITEA-2787: product gap — a project whose Project Context is enabled cannot run any agent ' +
      'turn: the current-path resolver excludes it (agent_chat.sql\'s project_context NOT EXISTS ' +
      'clause) and the start route answers 422 unsupported_agent_execution. The module writes a ' +
      'context that disables the very chat that would update it; project-context injection into ' +
      'the system prompt is unported.',
  );
  test.setTimeout(420_000);

  const stamp = Date.now() % 1_000_000;
  const agentName = `${AUTOTEST_PREFIX}pcb2-${stamp}`;
  const first = `${AUTOTEST_PREFIX}This project is a Python REST API for order management.`;
  const second = `${first} It now supports GraphQL in addition to REST.`;

  const { projectId } = await prepareAgentWithModule(page, agentName, PROJECT_CONTEXT_BUILDER);
  // The pre-state this case describes, stated rather than inherited: a project
  // carrying no context of its own. `WriteProjectContext` keeps whatever
  // toggle the project already had (see `clearProjectContext`), so a leftover
  // disabled row from an earlier run would make the `in effect` assertion
  // below measure that leftover instead of this module.
  await clearProjectContext(page, projectId);

  try {
    await sendTurn(page, `${callTool(PROJECT_CONTEXT_TOOL, { content: first })} write that down`);
    await expect
      .poll(async () => (await readProjectContext(page, projectId)).content, {
        timeout: 180_000,
        message: 'the Project Context Builder tool ran but the project context was not written',
      })
      .toBe(first);

    // THE assertion this test exists for, and the one the gap above stops:
    // the start below is the 422.
    await sendTurn(page, `${callTool(PROJECT_CONTEXT_TOOL, { content: second })} extend it`);
    await expect
      .poll(async () => (await readProjectContext(page, projectId)).content, {
        timeout: 180_000,
        message: 'the second turn did not update the existing project context',
      })
      .toBe(second);
    // ELITEA-2787's duplicate-free half: the read route serves ONE context per
    // project, so a second row would either shadow the first or be unreachable.
    // Asserting the updated content came back through that single read IS the
    // no-duplicate assertion on this platform's storage shape.
    expect(
      (await readProjectContext(page, projectId)).enabled,
      'an update that did not state `enabled` must keep the project’s current value',
    ).toBe(true);
  } finally {
    // The `test.fail` above makes this MANDATORY rather than tidy: an expected
    // failure still unwinds through here, and the row it would otherwise leave
    // behind refuses every subsequent chat-stream spec's turn in this project
    // (measured — it cost most of a full-project run).
    await clearProjectContext(page, projectId);
  }
});

/* onetest: ELITEA-2784/2786 — the precondition BOTH cases assume, and the only half of this file
   the python leg can assert: whether this deployment serves the two modules at all. The turn
   tests above run on the native leg; on the python leg this is the assertion, because the SDK
   worker has no port of either module and the product is required to SAY so (a toggle that is
   offered and then silently does nothing is the failure #866 existed to end). */
test('the deployment reports whether the builder modules are served by its worker', async ({ page }) => {
  test.setTimeout(60_000);

  const response = await page.request.get(`${BASE_URL}/api/v2/elitea_core/runtime_capabilities`);
  expect(
    response.ok(),
    `the runtime capability route must answer: ${response.status()} ${(await response.text()).slice(0, 200)}`,
  ).toBe(true);
  const body = (await response.json()) as {
    worker?: string;
    internal_tools?: Record<string, boolean>;
  };

  // Which worker the DEPLOYMENT says it runs, checked against which worker
  // this leg was started as. A mismatch means the leg proved nothing about
  // the runtime it named, whichever way the rest of the assertions go.
  expect(
    body.worker,
    'the deployment must report the worker this leg was started against',
  ).toBe(IS_NATIVE_RUNTIME ? 'rust' : 'python');

  expect(
    body.internal_tools?.[SKILLS_BUILDER],
    IS_NATIVE_RUNTIME
      ? '`skills_builder` must be reported AVAILABLE on the native runtime — the turn test above binds it'
      : '`skills_builder` must be reported UNAVAILABLE on the python worker, which has no port of it',
  ).toBe(IS_NATIVE_RUNTIME);
  expect(
    body.internal_tools?.[PROJECT_CONTEXT_BUILDER],
    IS_NATIVE_RUNTIME
      ? '`project_context_builder` must be reported AVAILABLE on the native runtime'
      : '`project_context_builder` must be reported UNAVAILABLE on the python worker',
  ).toBe(IS_NATIVE_RUNTIME);
});
