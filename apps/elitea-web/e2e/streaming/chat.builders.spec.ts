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
 *  - PROJECT CONTEXT BUILDER WRITES — and that write used to DISABLE the
 *    project's chat: the second turn was refused 422
 *    `unsupported_agent_execution`, because the current-path resolver excluded
 *    any project carrying an ENABLED project context (project-context
 *    injection into the system prompt was unported). #946 removed that gate
 *    and implemented the injection, so ELITEA-2787's update half is a passing
 *    assertion below rather than a stated gap. Every Project Context test here
 *    still clears the row in a `finally`: a row left behind is now INJECTED
 *    into every later chat-stream spec's turn in the same project instead of
 *    refusing it, which is quieter and no less wrong.
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
import { AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createAgentThroughForm } from '../fixtures/api';

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

/**
 * The SECOND project the isolation assertions at the end of this file read.
 *
 * The seeded shared project, which the chat persona can READ (it holds
 * `models.project_context.view` there) and whose rows no builder turn in its
 * own personal project may touch.
 */
const OTHER_PROJECT_ID = DEFAULT_PROJECT_ID;

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
 * spec.ts`'s header for the same rule stated from the other side. It is not
 * merely tidiness: since #946 a project carrying an ENABLED context has that
 * text spliced into the system prompt of every agent turn in it.
 */
async function clearProjectContext(page: import('@playwright/test').Page, projectId: string): Promise<void> {
  const restored = await page.request.put(projectContextUrl(projectId), {
    // EMPTY CONTENT, TOGGLE LEFT ON — and the second half is not an oversight.
    //
    // Emptying the CONTENT is what makes the row inert: the injection requires
    // non-blank content as well as the toggle (`CurrentProjectContext.
    // InjectableText`, services/elitea-main/internal/application/
    // agentexecution/projectcontext.go), and an empty context injects nothing.
    // Emptying it is therefore the whole of the cleanup.
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
   a duplicate. This was fail-marked until #946: the SECOND turn could not run at all while the
   first context was in effect. */
test('a second Project Context Builder turn updates the context in place', async ({ page }) => {
  test.skip(
    !IS_NATIVE_RUNTIME,
    'the Python SDK worker has no port of `project_context_builder`; the python leg asserts that ' +
      'absence through the capability route instead (see the last test in this file)',
  );
  // ── WHY THIS TEST WAS FAIL-MARKED, AND WHAT FIXED IT (#946) ─────────────
  //
  // Measured on the standalone stack, 2026-09-16: the first turn wrote the
  // context and enabled it, and the SECOND turn was then refused before it
  // reached the worker —
  //
  //   POST …/elitea_core/messages/prompt_lib/{project}/{conversation}
  //   422 {"error":"unsupported_agent_execution",
  //        "message":"This agent turn requires the current execution path."}
  //
  // — by one of the ~25 conditions in the current-path resolver
  // (`services/elitea-main/internal/db/queries/agent_chat.sql`, the
  // `NOT EXISTS (SELECT 1 FROM configuration AS project_context …)` clause,
  // present in every one of its resolve variants). Project-context INJECTION
  // was unported, so rather than run a turn that would silently ignore the
  // project's context, the resolver returned no rows.
  //
  // The consequence was larger than this case: the module's own successful
  // write made EVERY later agent turn in that project impossible, including
  // the one that would update it. #946 removed the clause from all four
  // variants and implemented the injection it was standing in for
  // (`internal/application/agentexecution/projectcontext.go`), so a second
  // turn is admitted and this case measures what it was written to measure.
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

    // THE assertion this test exists for. The start below was the 422 until
    // #946; a regression of that gate fails here first.
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
    // Still MANDATORY rather than tidy. A context row left enabled no longer
    // REFUSES another spec's turn (#946), but it is now INJECTED into every
    // later turn in this project — so leaving one behind would quietly put
    // this test's text into other specs' system prompts.
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

/* ────────────────────────────────────────────────────────────────────────────
 * CROSS-PROJECT ISOLATION (#939 group 2)
 *
 * ELITEA-3304/3305 were written against the legacy mechanism: a
 * `runtime_context` XML blob carrying `<user_id>`/`<project_id>`, injected into
 * the builder tool so the model would not have to be told which project to
 * write into. THAT MECHANISM DOES NOT EXIST HERE, and the cases' own
 * pass criteria are what survive the difference:
 *
 *   - "created without asking for project_id or user_id" — the tools' own
 *     `parameters_schema` (`services/elitea-worker-rust/src/agents/
 *     internal_tools.rs`) carries neither field, so the model CANNOT name a
 *     project even if it tried. The project comes from
 *     `ClaimBoundRuntimeContextAuthority` and is re-resolved by main on the
 *     claim-bound listener (`internal/infra/storage/content_server.go`);
 *   - "the context appears only in the current project" and "other projects are
 *     unaffected" — that is a claim about two projects, and it is what the two
 *     tests below measure. The tests above prove the write LANDS in the
 *     conversation's project; nothing yet proved it landed NOWHERE ELSE.
 *
 * ELITEA-3308 (the XML's exact shape) is therefore not portable and not a
 * defect: there is no wire format to assert, and the claim binding it stood in
 * for is a stronger guarantee than the blob was. ELITEA-3306/3307/3311/3312 ask
 * the same of an "Agent & Pipeline Builder" chat module, which this platform
 * does not have — `internal_mcp` is in the catalogue and is SKIPPED by the
 * native runtime, and the Settings switch of that name is the per-USER
 * `default_internal_mcp_enabled` flag, not a builder.
 *
 * THE SECOND PROJECT is the seeded shared project (`1`). The chat persona holds
 * `models.project_context.view` there (`scripts/e2e-stack.sh`), so it can READ
 * it — which is exactly the shape the assertion needs: a project the caller can
 * see and the tool must not have touched. Snapshot-and-compare rather than
 * expect-empty, because other specs write there too.
 * ──────────────────────────────────────────────────────────────────────────── */

/** Names of the skills project `projectId` holds. */
async function skillNamesIn(request: APIRequestContext, projectId: string): Promise<readonly string[]> {
  const response = await request.get(`${BASE_URL}/api/v2/elitea_core/skills/prompt_lib/${projectId}`);
  if (!response.ok()) return [];
  const body = (await response.json()) as { items?: readonly SkillRow[] };
  return (body.items ?? []).map((row) => String(row.name ?? ''));
}

/* onetest: ELITEA-3305 — Skill Builder creates the skill in the CURRENT project
   only: the model names no project, the row appears in the conversation's
   project, and the other project the caller can see is untouched. */
test('Skills Builder writes into the conversation’s own project and nowhere else', async ({ page }) => {
  test.skip(
    !IS_NATIVE_RUNTIME,
    'the Python SDK worker has no port of `skills_builder`; the python leg asserts that absence ' +
      'through the capability route instead',
  );
  test.setTimeout(360_000);

  const stamp = Date.now() % 1_000_000;
  const agentName = `${AUTOTEST_PREFIX}sbiso-${stamp}`;
  const skillName = `${AUTOTEST_PREFIX}skiso-${stamp}`;

  const { projectId } = await prepareAgentWithModule(page, agentName, SKILLS_BUILDER);
  expect(
    projectId,
    'this assertion needs two DIFFERENT projects — the chat persona must own a personal project (#290)',
  ).not.toBe(OTHER_PROJECT_ID);
  const otherBefore = await skillNamesIn(page.request, OTHER_PROJECT_ID);

  // The request names NO project and NO user. It cannot: the tool's schema has
  // no such argument, which is the platform's answer to "was the user asked to
  // specify project_id?" — they were never able to be.
  await sendTurn(
    page,
    `${callTool(SKILL_TOOL, {
      name: skillName,
      instructions: 'Summarize the thread in three bullets.',
    })} make me that skill`,
  );

  await expect
    .poll(async () => (await findSkillByName(page.request, projectId, skillName))?.name, {
      timeout: 180_000,
      message: 'the Skills Builder tool ran but no skill row exists in the conversation’s project',
    })
    .toBe(skillName);

  // THE ISOLATION ASSERTION. Read AFTER the row is known to exist in the right
  // project, so "absent from the other one" cannot be "not written yet".
  const otherAfter = await skillNamesIn(page.request, OTHER_PROJECT_ID);
  expect(otherAfter, 'the skill must not appear in a project the conversation is not in').not.toContain(skillName);
  expect(
    otherAfter.filter((name) => !otherBefore.includes(name)),
    'the other project gained no skill from this turn',
  ).toEqual([]);
});

/* onetest: ELITEA-3304 — Project Context Builder creates the context in the
   CURRENT project only; the other project's context is unchanged. */
test('Project Context Builder writes the conversation’s own project context and leaves another project’s alone', async ({
  page,
}) => {
  test.skip(
    !IS_NATIVE_RUNTIME,
    'the Python SDK worker has no port of `project_context_builder`; the python leg asserts that ' +
      'absence through the capability route instead',
  );
  test.setTimeout(360_000);

  const stamp = Date.now() % 1_000_000;
  const agentName = `${AUTOTEST_PREFIX}pcbiso-${stamp}`;
  const content = `${AUTOTEST_PREFIX}Isolation probe ${stamp}: this text belongs to one project only.`;

  const { projectId } = await prepareAgentWithModule(page, agentName, PROJECT_CONTEXT_BUILDER);
  expect(projectId, 'this assertion needs two DIFFERENT projects').not.toBe(OTHER_PROJECT_ID);
  await clearProjectContext(page, projectId);
  // Snapshot rather than expect-empty: other specs write to project 1 too, and
  // this test must not own that row.
  const otherBefore = await readProjectContext(page, OTHER_PROJECT_ID);

  try {
    await sendTurn(page, `${callTool(PROJECT_CONTEXT_TOOL, { content })} write that down`);
    await expect
      .poll(async () => (await readProjectContext(page, projectId)).content, {
        timeout: 180_000,
        message: 'the Project Context Builder tool ran but the project context was not written',
      })
      .toBe(content);

    const otherAfter = await readProjectContext(page, OTHER_PROJECT_ID);
    expect(otherAfter.content ?? '', 'the other project’s context must not carry this turn’s text').not.toContain(
      content,
    );
    expect(
      otherAfter.content ?? '',
      'the other project’s context must be exactly what it was before this turn',
    ).toBe(otherBefore.content ?? '');
    expect(otherAfter.enabled, 'the other project’s toggle must not have moved').toBe(otherBefore.enabled);
  } finally {
    await clearProjectContext(page, projectId);
  }
});
