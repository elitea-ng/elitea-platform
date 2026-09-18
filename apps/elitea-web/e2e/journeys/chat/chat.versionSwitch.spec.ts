/**
 * Switching a non-published agent's VERSION from inside a chat conversation
 * — the version selector persists the switch server-side, and does not
 * produce the spurious "LLM settings override is only allowed for published
 * agents from agent studio" error while doing so, and the selector's own
 * button label tracks the switch with no reload. Previously `test.fail()`-
 * marked as #907 ("label goes blank after the switch"): that turned out to be
 * `AgentEditorPanel`'s responsive icon-only mode, entered because the test
 * left the participants rail expanded — see the in-test comment at the
 * `Collapse participants` click for the measurements.
 *
 * Ported by use case from `w1-chat-interface.md` (ELITEA-0387 (#907) (#907)).
 *
 * ELITEA-0386 ("open the LLM settings panel for the agent in chat, edit a
 * setting after a version switch, save") is NOW PORTED (A14/F5) — see the
 * test below. It was previously recorded NA: `updateParticipantLlmSettings`
 * had zero callers anywhere in `src/`, and its own doc comment disclosed
 * that even a future caller would 400 outright (it PATCHed a bare
 * `{llm_settings}` object where the route it resolves to,
 * `BatchUpdateEntitySettings`, decodes a JSON ARRAY of
 * `{participant_id, ...settings}` rows). Both are fixed: the client now
 * sends the array shape (spreading the participant's CURRENT entity_settings
 * first — `BatchUpdateEntitySettings`'s repo half is a full REPLACE, not a
 * merge, same as the single-participant PUT), and `AgentEditorPanel` grew a
 * real trigger (`EditLlmSettingsButton`, gear-adjacent `Tune` icon) that
 * opens `widgets/llm-model-selector`'s `LLMSettingsDialog` for the active
 * agent participant, wired at the composition root
 * (`widgets/chat-box/ui/ChatBoxLlmSettingsDialog.tsx`).
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

/* onetest: ELITEA-0387 — switching a not-published agent's version in chat persists server-side with no spurious "LLM settings override" error, and the version selector's own label shows the newly-active version without a reload */
test('ELITEA-0387: switching a not-published agent\'s version has no spurious error and persists server-side', async ({
  page,
}) => {

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

    // Collapse the rail again once the pick has landed. `AgentEditorPanel` is
    // responsive (`useAgentEditorPanelFit`): it measures the composer's
    // controls ROW and drops every label — the version selector's included —
    // for an icon-only view below 430px. An expanded participants rail takes
    // the chat column down to ~716px and the controls row to ~352px, so the
    // panel is legitimately icon-only for as long as the rail is open, and
    // the selector's own button renders `VersionIcon` instead of ANY name.
    // That — not the switch — is what the original `test.fail()` here was
    // measuring: the label was already blank ~400ms after the panel mounted,
    // before any version was switched (the earlier `toHaveText('base')` only
    // passed inside the fit hook's own 100ms debounce window). Measured on
    // this stack: rail open -> row 352px, icon; rail closed -> row 548px,
    // label. See #907's ledger row.
    await page.getByRole('button', { name: 'Collapse participants' }).click();

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

    // "Version selector shows V1 [the switched-to version] as active" — the
    // case's own literal final assertion, and it holds: the selector's label
    // tracks the switch with no reload, in the layout where the panel renders
    // labels at all.
    await expect(versionButton).toHaveText('v2', { timeout: 5_000 });
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteAgent(page.request, agent.id);
  }
});

/* onetest: ELITEA-0386 — editing a not-published agent participant's LLM settings from inside a chat conversation succeeds (the batch PATCH now sends the array shape the handler decodes), with no spurious "LLM settings override" error */
test('ELITEA-0386: editing a not-published agent participant\'s LLM settings from chat persists, with no spurious error', async ({
  page,
}) => {
  // More steps than ELITEA-0387 above (agent+version create, dialog
  // open/apply/close, a 20s PATCH wait, a 15s poll) — same
  // `test.setTimeout` precedent `settings.users.spec.ts` already uses for
  // multi-step journeys past the 30s default.
  test.setTimeout(60_000);

  const catalogue = await page.request.get(`${API_BASE}/configurations/models/${DEFAULT_PROJECT_ID}?include_shared=true`);
  expect(catalogue.status()).toBe(200);
  const models = ((await catalogue.json()) as { items?: readonly { name: string }[] }).items ?? [];
  expect(models.length, 'this stack seeds at least one llm-section model row').toBeGreaterThan(0);
  const modelName = models[0]!.name;

  const agentName = uniqueName('llmag');
  const agent = await createAgentWithVersion(page.request, agentName, {
    agentType: 'openai',
    model: { modelName },
  });

  const conversationId = await createConversation(page.request, uniqueName('llmconv'));
  try {
    await attachParticipants(page.request, conversationId, [
      {
        entity_name: 'application',
        entity_meta: { id: agent.id, project_id: DEFAULT_PROJECT_ID, name: agentName },
        entity_settings: { version_id: agent.versionId },
      },
    ]);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    const expandParticipants = page.getByRole('button', { name: 'Expand participants' });
    await expect(expandParticipants).toBeVisible({ timeout: 15_000 });
    await expandParticipants.click();
    const agentsSection = page.getByTestId('participants-section-Agents');
    await expect(agentsSection).toBeVisible({ timeout: 15_000 });
    await agentsSection.getByText(agentName, { exact: true }).click();

    const editLlmSettings = page.getByTestId('chat-agent-editor-llm-settings-button');
    await expect(editLlmSettings, 'the per-participant LLM-settings trigger must be offered for an agent participant this admin can edit').toBeVisible({
      timeout: 15_000,
    });
    await editLlmSettings.click();

    const dialog = page.getByText('Model settings');
    await expect(dialog).toBeVisible({ timeout: 10_000 });
    await checkA11y(page);

    const patched = page.waitForResponse(
      (r) => r.url().includes(`/entity_settings/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`) && r.request().method() === 'PATCH',
      { timeout: 20_000 },
    );
    // Apply with no field edits — this proves the WIRE SHAPE/participant
    // scoping succeeds (the case this test is about), not any one field's
    // round trip; `entities/participant/api/participantApi.test.ts` already
    // pins the exact request body shape unit-level.
    await page.getByRole('button', { name: 'Apply' }).click();
    const patchResponse = await patched;
    expect(
      patchResponse.status(),
      `the batch PATCH must succeed, not 400 the array-vs-object mismatch this case is about: ${(await patchResponse.text()).slice(0, 300)}`,
    ).toBe(200);

    await expect(dialog).not.toBeVisible({ timeout: 10_000 });

    // No spurious override error anywhere on screen after the save.
    const texts = await toastTexts(page);
    expect(texts.join(' | ')).not.toContain('LLM settings override is only allowed for published agents');

    // The write reaches the server, not a void: re-read the participant and
    // confirm entity_settings still carries this agent's version_id (proving
    // the REPLACE-not-merge fix — `currentEntitySettings` spread — actually
    // preserved it rather than wiping it out with a bare `{llm_settings}`).
    await expect
      .poll(
        async () => {
          const rows = await readParticipants(page.request, conversationId);
          return String(rows[0]?.entity_settings?.version_id);
        },
        { timeout: 15_000, message: 'the LLM-settings save must not clobber the participant\'s other entity_settings fields' },
      )
      .toBe(String(agent.versionId));
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteAgent(page.request, agent.id);
  }
});
