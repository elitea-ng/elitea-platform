/**
 * Switching a non-published agent's VERSION from inside a chat conversation
 * — the version selector persists the switch server-side, and does not
 * produce the spurious "LLM settings override is only allowed for published
 * agents from agent studio" error while doing so. `test.fail()`-marked: the
 * switch itself is correct, but the selector's own button label goes BLANK
 * instead of showing the version just switched to — see the test's own
 * `test.fail()` call for the diagnosis, and `S/port/defects.md`.
 *
 * Ported by use case from `w1-chat-interface.md` (ELITEA-0387).
 *
 * ELITEA-0386 ("open the LLM settings panel for the agent in chat, edit a
 * setting after a version switch, save") is NOT here — recorded NA instead.
 * `entities/participant/api/participantApi.ts`'s `updateParticipantLlmSettings`
 * is the only client function shaped for "edit an agent participant's
 * llm_settings from chat", and it has ZERO callers anywhere in `src/`
 * (grepped) — there is no LLM-settings-editing panel wired to any agent
 * participant in a chat conversation to open at all. Its own doc comment
 * additionally discloses that even a future caller would 400 outright: it
 * PATCHes a bare `{llm_settings}` object, but the route it resolves to
 * (`BatchUpdateEntitySettings`) decodes a JSON ARRAY of
 * `{participant_id, ...settings}` rows — a genuine, already-documented
 * backend/frontend contract mismatch, not something this package invented.
 * See `S/port/not-applicable.md`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT IS AND ISN'T DRIVEN THROUGH THE UI
 * ─────────────────────────────────────────────────────────────────────────────
 * The version SWITCH itself is real UI: `VersionSelector`
 * (`features/chat-input/ui/VersionSelector.tsx`) is reached by role
 * ("version selector menu"), and `useChatBoxVersioning`'s `handleSelectVersion`
 * is the composition-root write this test proves reaches the server — the
 * exact `PUT .../entity_settings/.../{participantId}` route
 * `services/elitea-main/internal/api/v2/conversations/handler.go`'s
 * `UpdateEntitySettings` serves.
 *
 * The manual case's final steps ("send a message", "the agent responds")
 * are NOT driven: sending in an ALREADY-EXISTING conversation needs the
 * runtime plane, which this stack does not mount
 * (`chat.management.spec.ts`'s module header, note 1) — a send here would
 * exercise a 405, not the version/LLM-settings contract this journey is
 * about. What IS asserted is the property both cases actually discriminate:
 * the version_id the SERVER now holds for the participant, and the absence
 * of the spurious override error across the write(s) the switch triggers.
 *
 * `UpdateEntitySettings`'s own override check
 * (`handler.go`'s `llmSettingsDiffer` branch) fires only when a write's body
 * carries BOTH `llm_settings` AND a `version_id`, and the settings differ
 * from that version's own baseline. `handleSelectVersion` always sends the
 * NEW version's own `llm_settings` alongside its `version_id` — matching by
 * construction — so a version switch alone can never trip it; this is
 * exercised directly to prove that stays true across repeated switches.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  agentVersionBody,
  createAgentWithVersion,
  createConversation,
  deleteAgent,
  deleteConversation,
  readApplicationVersions,
} from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-verswitch';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}${SUFFIX}`;
}

const PARTICIPANTS_PATH = `/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}`;
const CONVERSATION_PATH = `/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;

interface ParticipantRow {
  readonly id: number | string;
  readonly entity_name?: string;
  readonly entity_settings?: { readonly version_id?: string | number };
}

async function attachParticipants(
  request: APIRequestContext,
  conversationId: string,
  bodies: readonly Record<string, unknown>[],
): Promise<readonly ParticipantRow[]> {
  const response = await request.post(`${API_BASE}${PARTICIPANTS_PATH}/${conversationId}`, { data: bodies });
  expect(response.status(), `participants must attach: ${(await response.text()).slice(0, 300)}`).toBe(200);
  return (await response.json()) as readonly ParticipantRow[];
}

async function readParticipants(
  request: APIRequestContext,
  conversationId: string,
): Promise<readonly ParticipantRow[]> {
  const response = await request.get(`${API_BASE}${CONVERSATION_PATH}/${conversationId}`);
  expect(response.status()).toBe(200);
  const body = (await response.json()) as { participants?: readonly ParticipantRow[] };
  return body.participants ?? [];
}

/** Creates a SECOND version on an existing agent — `POST /versions/prompt_lib/{p}/{app}` (applications/handler.go's `CreateVersion`). */
async function createSecondVersion(
  request: APIRequestContext,
  applicationId: string,
  name: string,
  modelName: string,
): Promise<string> {
  const path = `/elitea_core/versions/prompt_lib/${DEFAULT_PROJECT_ID}/${applicationId}`;
  const body = agentVersionBody({ name, agentType: 'openai', model: { modelName } });
  const response = await request.post(`${API_BASE}${path}`, { data: body });
  expect(response.status(), `second version must be created: ${(await response.text()).slice(0, 300)}`).toBe(201);
  const created = (await response.json()) as { id?: unknown };
  expect(typeof created.id).toBe('string');
  return created.id as string;
}

/** Every toast notification currently on screen — the union of MUI Snackbar/Alert content this app's toast layer renders. */
async function toastTexts(page: import('@playwright/test').Page): Promise<readonly string[]> {
  return page.getByRole('alert').allTextContents();
}

/* onetest: ELITEA-0387 — switching a not-published agent's version in chat persists server-side with no spurious "LLM settings override" error, but the version selector's OWN label goes blank instead of showing the newly-active version */
test('ELITEA-0387: switching a not-published agent\'s version has no spurious error and persists server-side', async ({
  page,
}) => {
  test.fail(
    true,
    "ELITEA-0387: product gap — the version switch persists correctly server-side and produces no spurious override error (both verified below), but VersionSelector's own button goes BLANK instead of showing the newly-active version's name: useChatBoxVersioning.ts's mergeParticipantVersionSettings spreads a snake_case entity_settings key onto activeParticipant (already normalised to camelCase entitySettings elsewhere), so AgentEditorPanel's resolveSelectedVersion(participantDetails?.versions, participantForEditor?.entitySettings?.versionId) can no longer resolve a selected version from the corrupted local object — confirmed recoverable by a page reload, which re-fetches and re-normalises from the server's (correct) state",
  );

  const catalogue = await page.request.get(`${API_BASE}/configurations/models/${DEFAULT_PROJECT_ID}?include_shared=true`);
  expect(catalogue.status()).toBe(200);
  const models = ((await catalogue.json()) as { items?: readonly { name: string }[] }).items ?? [];
  expect(models.length, 'this stack seeds at least one llm-section model row').toBeGreaterThan(0);
  const modelName = models[0]!.name;

  const agentName = uniqueName('verag');
  const agent = await createAgentWithVersion(page.request, agentName, {
    agentType: 'openai',
    model: { modelName },
  });
  const v2Id = await createSecondVersion(page.request, agent.id, 'v2', modelName);
  const versions = await readApplicationVersions(page.request, agent.id);
  const v2 = versions.find((v) => v.id === v2Id);
  expect(v2, 'the created version must be readable back by the editor\'s own read').toBeDefined();

  const conversationId = await createConversation(page.request, uniqueName('verconv'));
  try {
    const attached = await attachParticipants(page.request, conversationId, [
      {
        entity_name: 'application',
        entity_meta: { id: agent.id, project_id: DEFAULT_PROJECT_ID, name: agentName },
        entity_settings: { version_id: agent.versionId },
      },
    ]);
    const participantId = String(attached[0]?.id);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });
    await checkA11y(page);

    // A cold deep link opens with NO active participant
    // (`useActiveParticipantSelection`'s own doc comment: the page's restore
    // effect finds nothing in local storage, so the composer starts on the
    // bare model view). `AgentEditorPanel` — and its `VersionSelector` — only
    // renders once this agent IS the active participant, so it is selected
    // from the rail first, exactly as a person would.
    const expandParticipants = page.getByRole('button', { name: 'Expand participants' });
    await expect(expandParticipants).toBeVisible({ timeout: 15_000 });
    await expandParticipants.click();
    const agentsSection = page.getByTestId('participants-section-Agents');
    await expect(agentsSection).toBeVisible({ timeout: 15_000 });
    await agentsSection.getByText(agentName, { exact: true }).click();

    const versionButton = page.getByRole('button', { name: 'version selector menu' });
    await expect(versionButton, 'the version selector must be offered for an agent with 2+ versions').toBeVisible({
      timeout: 15_000,
    });
    // Named "base" — see `createAgentWithVersion`'s own hardcoded first-version name.
    await expect(versionButton).toHaveText('base');

    await versionButton.click();
    const menu = page.getByRole('menu');
    await expect(menu).toBeVisible({ timeout: 10_000 });
    await expect(menu.getByRole('menuitem', { name: 'base' })).toBeVisible();
    await expect(menu.getByRole('menuitem', { name: 'v2' })).toBeVisible();

    const persisted = page.waitForResponse(
      (r) => r.url().includes(`/entity_settings/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}/${participantId}`) && r.request().method() === 'PUT',
      { timeout: 20_000 },
    );
    await menu.getByRole('menuitem', { name: 'v2' }).click();
    const putResponse = await persisted;
    expect(putResponse.status(), 'the switch must reach the server, or nothing below means anything').toBe(200);

    // The server now holds the NEW version for this participant. This half
    // of the case IS real: the switch is not a client-side illusion.
    await expect
      .poll(
        async () => {
          const rows = await readParticipants(page.request, conversationId);
          return String(rows[0]?.entity_settings?.version_id);
        },
        { timeout: 15_000, message: 'the switch must persist the new version_id server-side' },
      )
      .toBe(v2Id);

    // No spurious override error anywhere on screen after the switch — the
    // OTHER half of what the manual case's title asserts, and it also holds.
    const texts = await toastTexts(page);
    expect(texts.join(' | ')).not.toContain('LLM settings override is only allowed for published agents');

    // What does NOT hold: "Version selector shows V1 [the switched-to
    // version] as active" — the case's own literal final assertion. See this
    // test's own `test.fail()` call above for the diagnosis.
    await expect(versionButton).toHaveText('v2', { timeout: 5_000 });
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteAgent(page.request, agent.id);
  }
});
