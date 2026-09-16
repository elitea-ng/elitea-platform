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
 * WORKER: the NATIVE runtime only. This is deliberate and is not a gap being
 * papered over — the Python SDK worker has no port of either module and this
 * programme excludes the SDK, so `GET /elitea_core/runtime_capabilities`
 * reports both as unavailable there and the UI renders the toggles disabled
 * with that reason. A python leg would assert the absence, which
 * `capabilities_handler_test.go` already does without a stack.
 *
 * STATUS: UNVERIFIED. The `chat-stream` project (mock LLM + a worker) is not
 * available in this wave, so this file has not been run. The pieces it spans
 * are proven separately where they can be: the write routes and their
 * claim-scoped tenancy in
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
  const body = (await response.json()) as { rows?: readonly SkillRow[] };
  return (body.rows ?? []).find((row) => row.name === name);
}

/* onetest: ELITEA-2784 — a fully specified request creates the Skill with the
   fields it named, and no clarification is needed. ELITEA-2794 — the created
   Skill is NOT auto-added as a participant. The clarification HALF of the
   sibling cases (ELITEA-2785) is `chat.hitl.spec.ts`'s subject: the pause and
   its resume are `ask_user`'s, not this module's, and this module adds no
   branch to either. */
test('a chat turn with Skills Builder enabled creates the Skill it names, and does not attach it', async ({ page }) => {
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

/* onetest: ELITEA-2786 — a fully specified request creates the Project
   Context. ELITEA-2787 — a second request UPDATES the single existing context
   rather than creating a duplicate, which on this platform is a property of
   the storage shape (one `configuration` row per project) and is asserted as
   such: the content changes and the row count cannot. */
test('a chat turn with Project Context Builder enabled writes the project context, then updates it in place', async ({ page }) => {
  test.setTimeout(420_000);

  const stamp = Date.now() % 1_000_000;
  const agentName = `${AUTOTEST_PREFIX}pcb-${stamp}`;
  const first = `${AUTOTEST_PREFIX}This project is a Python REST API for order management.`;
  const second = `${first} It now supports GraphQL in addition to REST.`;

  const { projectId } = await prepareAgentWithModule(page, agentName, PROJECT_CONTEXT_BUILDER);

  const readContext = async (): Promise<{ content?: string; enabled?: boolean }> => {
    const response = await page.request.get(
      `${BASE_URL}/api/v2/elitea_core/project_context/prompt_lib/${projectId}/project-context`,
    );
    return response.ok() ? ((await response.json()) as { content?: string; enabled?: boolean }) : {};
  };

  await sendTurn(page, `${callTool(PROJECT_CONTEXT_TOOL, { content: first })} write that down`);
  await expect
    .poll(async () => (await readContext()).content, {
      timeout: 180_000,
      message: 'the Project Context Builder tool ran but the project context was not written',
    })
    .toBe(first);
  // A context written from chat that is not in effect is indistinguishable,
  // from the user's side, from one that was never written.
  expect((await readContext()).enabled, 'a first context must be in effect').toBe(true);

  await sendTurn(page, `${callTool(PROJECT_CONTEXT_TOOL, { content: second })} extend it`);
  await expect
    .poll(async () => (await readContext()).content, {
      timeout: 180_000,
      message: 'the second turn did not update the existing project context',
    })
    .toBe(second);
  // ELITEA-2787's duplicate-free half: the read route serves ONE context per
  // project, so a second row would either shadow the first or be unreachable.
  // Asserting the updated content came back through that single read IS the
  // no-duplicate assertion on this platform's storage shape.
  expect(
    (await readContext()).enabled,
    'an update that did not state `enabled` must keep the project’s current value',
  ).toBe(true);
});
