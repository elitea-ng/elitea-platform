/**
 * The agent TOOLS surface: internal-tool toggles, the agent-as-tool relation,
 * and the edit page's own Chat button — all driven through the UI, all
 * asserted against what the server stored and what the runtime then does.
 *
 * Three defect classes this journey pins, each measured in a live browser
 * before it was fixed:
 *
 *  1. INTERNAL TOOLS BRICKED THE AGENT. The version-level admission gates
 *     matched `meta.internal_tools` against literal arrays, so toggling ANY
 *     internal tool made the resolver return zero rows and every send answer
 *     422 — a message naming neither the toggle nor the runtime. The gates
 *     now admit the platform catalogue, and BOTH runtimes skip what they
 *     cannot serve with a logged `agent_internal_tool_skipped`. The Python
 *     worker was believed to serve the whole set and did not: `planner` died
 *     on a plan directory it could not create and `pyodide` on a Deno runtime
 *     its image does not ship, and either one killed the turn — POST 200,
 *     then a stored assistant row flagged `is_error` with EMPTY content, which
 *     is exactly what step 5 below rules out. The observable contract asserted
 *     here: every toggle ON, saved, and the agent STILL ANSWERS.
 *
 *  2. THE AGENT-AS-TOOL ATTACH WAS A SILENT 200 NO-OP. The router bound
 *     PATCH `application_relation` to the READ handler, and the unreachable
 *     write handler targeted a table that exists in no schema — so the "+
 *     Agent" picker answered 200 and wrote nothing. Asserted here by
 *     attaching, reading the relation back, and then DETACHING: a detach that
 *     removes no row answers 404, so only a write that really happened lets
 *     step 3 finish.
 *
 *  3. NO WAY TO TALK TO THE AGENT. The edit page's Chat button (this branch)
 *     creates a conversation carrying BOTH participants — the user (nothing
 *     server-side creates that mapping on the REST path, and the resolver's
 *     author join refuses a conversation without it) and the agent with its
 *     load-bearing `entity_settings.version_id`.
 *
 * Lives in `streaming/` because an agent turn needs the full standalone stack
 * (`chat-stream` project, `scripts/chat-stream-e2e.sh`); the plain journeys
 * stack has no worker and no model.
 */
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  AUTOTEST_PREFIX,
  createAgentThroughForm,
  expectStoredAssistantAnswer,
  fillComposer,
} from '../fixtures/api';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/**
 * The canonical internal-tool names — `INTERNAL_TOOLS_LIST[].name` in
 * `src/features/agents/lib/internalTools.ts`, minus `image_generation`, which
 * the panel hides unless the project offers an image-generation toolkit.
 *
 * ONE list, and the testids are derived from it rather than written out beside
 * it. `AgentInternalToolSwitch` builds `data-testid` from this same name, so a
 * switch is addressed by exactly the string it writes into
 * `meta.internal_tools` and the toggle and the read-back below cannot drift
 * apart. They could before: the testid was built from the DISPLAY TITLE
 * ("Swarm Mode" → `internal-tool-swarm-mode`), so this file carried two
 * parallel arrays whose rows had to be kept in the same order by hand, and
 * rewording or translating a label would have moved a handle this journey
 * depends on.
 */
const INTERNAL_TOOL_NAMES = [
  'attachments',
  'internal_mcp',
  'pyodide',
  'data_analysis',
  'planner',
  'swarm',
  'lazy_tools_mode',
] as const;

const testIdFor = (toolName: string): string => `internal-tool-${toolName}`;

test('internal tools all on, agent-as-tool attached — and the agent still answers', async ({ page }) => {
  test.setTimeout(300_000);
  const name = `${AUTOTEST_PREFIX}tools-${Date.now() % 1_000_000}`;

  // ── 1. Author the agent through the form ────────────────────────────────
  const { projectId, agentId, versionId } = await createAgentThroughForm(page, name);

  // The save lands on the agent's edit page, where the Tools panel lives.
  await expect(page.getByTestId(testIdFor('attachments'))).toBeVisible({ timeout: 30_000 });

  // The grid folds to four rows behind "Show all"; the other three switches
  // do not exist in the DOM until it is expanded.
  await page.getByText('Show all', { exact: true }).click();
  await expect(page.getByTestId(testIdFor('pyodide'))).toBeVisible({ timeout: 10_000 });

  // ── 2. Every internal tool ON, then Save ───────────────────────────────
  for (const toolName of INTERNAL_TOOL_NAMES) {
    const row = page.getByTestId(testIdFor(toolName));
    await row.scrollIntoViewIfNeeded();
    await row.getByRole('switch').click();
  }
  const versionSaved = page.waitForResponse(
    (r) => r.url().includes('/elitea_core/version/prompt_lib/') && r.request().method() === 'PUT',
    { timeout: 30_000 },
  );
  await page.getByTestId('agent-save-button').click();
  expect((await versionSaved).status(), 'the toggles must reach the version row').toBeLessThan(300);

  // What the server now STORES is the half a UI assertion cannot fake.
  const stored = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`,
  );
  const detail = (await stored.json()) as { version_details?: { meta?: { internal_tools?: readonly string[] } } };
  const storedTools = detail.version_details?.meta?.internal_tools ?? [];
  for (const toolName of INTERNAL_TOOL_NAMES) {
    expect(storedTools, `${toolName} must survive the save`).toContain(toolName);
  }

  // ── 3. Attach an agent as a tool, read it back, and detach it ──────────
  //
  // Through the API rather than the picker: the pick emits exactly these two
  // PATCHes (measured — `useAgentPipelineAssociation` sends `has_relation:
  // true` to attach, `useDisassociateToolkit` sends `false` to detach), and
  // the defect being pinned was server-side, a 200 that wrote nothing.
  //
  // THE DETACH IS THE DISCRIMINATING HALF, not the list read below it. A
  // zero-row detach answers 404 ("no such relation on this version"), so a
  // detach that succeeds proves the attach actually inserted the row — which
  // is precisely what the old handler did not do. This journey used to send
  // the detach ALONE and require a 2xx; that passed against the handler that
  // answered 201 whatever it deleted, so it asserted nothing at all.
  //
  // AND THE DETACH IS LOAD-BEARING FOR STEP 4. This spec attaches the agent
  // to ITSELF (one agent, exercised as both ends of the relation), and a
  // SELF-referencing nested tool is refused at admission — the send answers
  // 422 because the nesting walk repeats the agent's own key at hop two
  // ("circular reference", `walkCurrentApplicationNesting` in
  // services/elitea-main/internal/infra/db/repos/agent_nesting.go — NOT the
  // freeze, which admits the row's shape fine). So the relation is exercised
  // in both directions and removed before the turn. The agent-as-tool TURN
  // with a DISTINCT child — which the runtime serves — is
  // chat.nested-agent.spec.ts's job.
  const relation = await page.request.patch(
    `${BASE_URL}/api/v2/elitea_core/application_relation/prompt_lib/${projectId}/${agentId}/${versionId}`,
    { data: { application_id: Number(agentId), version_id: Number(versionId), has_relation: true } },
  );
  expect(relation.status(), 'the relation route must be writable').toBeLessThan(300);
  const relationList = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application_relation/prompt_lib/${projectId}/${agentId}/${versionId}`,
  );
  expect(relationList.ok()).toBe(true);
  const detached = await page.request.patch(
    `${BASE_URL}/api/v2/elitea_core/application_relation/prompt_lib/${projectId}/${agentId}/${versionId}`,
    { data: { application_id: Number(agentId), version_id: Number(versionId), has_relation: false } },
  );
  expect(
    detached.status(),
    'the detach must find the row the attach wrote — a 404 here means the attach was a no-op',
  ).toBeLessThan(300);

  // ── 4. Chat: both participants, and an admitted, answered turn ─────────
  const conversationCreated = page.waitForResponse(
    (r) => /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await page.getByTestId('chat-with-agent-button').click();
  const conversation = (await (await conversationCreated).json()) as { id?: string | number };
  await page.waitForURL(new RegExp(`/app/chat/${String(conversation.id ?? '')}$`), { timeout: 30_000 });

  const participants = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${String(conversation.id ?? '')}`,
  );
  const conversationBody = (await participants.json()) as {
    participants?: readonly { entity_name?: string; entity_settings?: { version_id?: string | number } }[];
  };
  const names = (conversationBody.participants ?? []).map((p) => p.entity_name);
  expect(names, 'the resolver joins the author through the user mapping').toContain('user');
  const agentParticipant = (conversationBody.participants ?? []).find((p) => p.entity_name === 'application');
  expect(String(agentParticipant?.entity_settings?.version_id ?? '')).toBe(String(versionId));

  // Composer first, budget second — the same shape `chat.pipeline.spec.ts`
  // failed on in run 33812152063. See `fillComposer`.
  const sendButton = await fillComposer(page, `autotest tools ${Date.now()}`);
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 45_000 },
  );
  await sendButton.click();

  const startResponse = await started;
  expect(
    startResponse.status(),
    `the turn was refused with every internal tool on: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);

  // The STORED reply, not the on-screen bubble — the `IS_ERROR` discrimination
  // and why nothing on screen can stand in for it are in
  // `expectStoredAssistantAnswer`. This spec's own first draft went green
  // against the error card it rules out.
  await expectStoredAssistantAnswer(page, projectId, conversation.id ?? '', {
    timeout: 90_000,
    message: 'no stored answer — the runtime refused a profile the admission gates let through',
  });

  await page.request.delete(`${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agentId}`);
});
