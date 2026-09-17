/**
 * THE CHAT-EMBEDDED ENTITY EDITOR (#940 A12).
 *
 * Creating or editing a Pipeline (or an Agent) as a chat participant opens the
 * real editor INSIDE the chat page, beside the conversation, and saves through
 * the ordinary routes.
 *
 * ── WHAT WAS ACTUALLY BROKEN, measured before this ──────────────────────────
 *
 * Three separate gaps, each of which looked like a working screen:
 *
 *  1. `PipelineEditor`'s `deps.renderCreateForm` slot reached the chat
 *     composition root as `undefined`, and `PipelineEditorParts.tsx` renders
 *     it with `?.` — so "Create new" under the composer's Pipelines submenu
 *     opened an editor with a title bar, a Save button and NO FORM at all.
 *     The Agents submenu next to it worked, which is what made it easy to
 *     miss.
 *  2. `deps.renderConfigurationPanels` and `deps.onSaveVersion` were likewise
 *     absent, so opening an EXISTING pipeline participant for editing showed
 *     an empty Configuration tab behind a permanently-disabled Save.
 *  3. The editors rendered as siblings of the chat ROW, each a `height: 100%`
 *     block BELOW it inside a `main` that is `display: block; height: 100vh`
 *     with no overflow of its own. `EditorShell` is a fixed header (Close,
 *     Discard, Save) over one `overflow: auto` body and that layout only works
 *     inside a parent with a bounded height — given none, the header sat a
 *     viewport below the fold and the whole document scrolled instead of the
 *     form.
 *
 * ── AND THE FOURTH, WHICH NO LAYOUT FIX WOULD HAVE FOUND ────────────────────
 *
 * Both create paths sent `conversationStarters: []` on the wire whatever the
 * form collected (`useAgentEditorCreate` / `usePipelineEditorCreate`), while
 * `ConversationStartersEditor` wrote `version_details.conversation_starters`
 * and the form state carried the setter for it. Four starters typed before
 * Save produced an entity with none, with no error anywhere. That is why the
 * starter assertions below are read back from the SERVER rather than off the
 * rows still on screen.
 *
 * Ported BY USE CASE: the legacy cases describe a "canvas" with Save/Discard/
 * Close controls and a configuration page that must not scroll into the
 * toolkit section. This app's equivalent is the editor column
 * (`chat-editor-panel`) with `EditorShell`'s header — same three controls,
 * same requirement that the form scrolls inside its own panel — and its
 * Configuration tab carries no toolkit section at all (see
 * `useChatPipelineConfig`'s own doc comment for why the Tools panel is
 * deliberately not offered there), so "does not overflow into the toolkit
 * section" is satisfied by the panel being the scroll container.
 */
import { expect, test } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createConversation, deleteConversation } from '../../fixtures/api';

const CONVERSATION_PATH = `/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;

interface ParticipantRow {
  readonly entity_name?: string;
  readonly entity_meta?: { readonly id?: string | number };
  readonly entity_settings?: { readonly version_id?: string | number };
}

function uniqueName(tag: string): string {
  // `MAX_NAME_LENGTH` is 32 and the field TRUNCATES on type, so a longer name
  // silently saves a different one than the by-name lookup then searches for
  // (the #882 CI failure `chat.composerCreate.spec.ts` records).
  return `${AUTOTEST_PREFIX}${tag}-${Date.now() % 1_000_000}`;
}

async function readParticipants(request: APIRequestContext, conversationId: string): Promise<readonly ParticipantRow[]> {
  const response = await request.get(`${API_BASE}${CONVERSATION_PATH}/${conversationId}`);
  expect(response.status()).toBe(200);
  const body = (await response.json()) as { participants?: readonly ParticipantRow[] };
  return body.participants ?? [];
}

/** Opens the composer "+" menu's Pipelines submenu and clicks its "Create new" row. */
async function openPipelineCreate(page: Page): Promise<void> {
  const plus = page.getByTestId('plus-menu-button');
  await expect(plus).toBeEnabled({ timeout: 20_000 });
  await plus.click();
  const row = page.getByTestId('plus-menu-pipelines');
  await expect(row).toBeVisible({ timeout: 10_000 });
  await row.click();
  const createNew = page.getByTestId('plus-submenu-create-new');
  await expect(
    createNew,
    'the Pipelines submenu must offer "Create new" — the row is hidden when no onCreatePipeline handler reaches it',
  ).toBeVisible({ timeout: 10_000 });
  await createNew.click();
}

/**
 * Opens the participants rail if it is collapsed.
 *
 * The rail's toggle is one button whose accessible name STATES the direction
 * ("Expand participants" / "Collapse participants"), so the expand name being
 * absent is exactly "already expanded" — there is no state to read separately
 * and no risk of collapsing an open rail by clicking blind.
 */
async function expandParticipants(page: Page): Promise<void> {
  const expand = page.getByRole('button', { name: 'Expand participants' });
  if ((await expand.count()) > 0) await expand.click();
  await expect(page.getByTestId('participants-container')).toBeVisible({ timeout: 15_000 });
}

/**
 * Opens the editor for one participant by name, from the rail.
 *
 * Two things had to be measured rather than assumed, and both cost a run:
 * the rail starts COLLAPSED, and a row's action bar is `showButtons=
 * {isHovering}` — so the edit control does not exist in the DOM until the row
 * is hovered. The accessible name is type-specific ("Edit pipeline X", "Edit
 * agent X", `resolveEditTooltip`), so it is matched by pattern rather than by
 * one literal: a test that pins the literal breaks the day the participant's
 * stored `agent_type` changes shape, which is not what it is testing.
 */
async function openParticipantEditor(page: Page, participantName: string): Promise<void> {
  await expandParticipants(page);
  const row = page.getByTestId('participants-container').getByText(participantName, { exact: false }).first();
  await expect(row, 'the attached participant must be listed in the rail').toBeVisible({ timeout: 20_000 });
  await row.hover();
  const edit = page.getByRole('button', { name: new RegExp(`^Edit .*${participantName}$`) });
  await expect(
    edit,
    'hovering the participant row must reveal its edit control',
  ).toBeVisible({ timeout: 15_000 });
  await edit.click();
}

async function deletePipeline(request: APIRequestContext, pipelineId: string): Promise<void> {
  await request.delete(`${API_BASE}/elitea_core/application/prompt_lib/${DEFAULT_PROJECT_ID}/${pipelineId}`);
}

/* onetest: ELITEA-0927 — creating a Pipeline as a chat participant: the form
   opens INSIDE the chat context, Save stores it, and it is attached as a
   participant. (The Agent half of the same case is already proven by
   `chat.composerCreate.spec.ts`; this is the half that opened an empty
   editor.)
   ELITEA-0919/0920 — 1 to 4 conversation starters, each appearing exactly
   once, with the 5th blocked — and, unlike the case's screen-level wording,
   read back from the SERVER because the wire dropped them regardless of what
   the rows showed.
   ELITEA-0922 — Save / Discard / Close stay reachable with all four starters
   added. */
test('creating a pipeline from the chat composer opens a real form, keeps its controls reachable, and attaches what it saved', async ({ page }) => {
  const conversationId = await createConversation(page.request, uniqueName('embed-create'));
  const pipelineName = uniqueName('pl');
  let pipelineId: string | undefined;

  try {
    // Nothing is attached yet, so whatever the flow below attaches is the only
    // thing that could have.
    expect(await readParticipants(page.request, conversationId)).toEqual([]);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    await openPipelineCreate(page);

    // ── 1. The panel, and the form inside it ────────────────────────────────
    //
    // The panel is the assertion for gap 3: an editor rendered as a sibling of
    // the chat row has no bounded-height parent and no scroll container of its
    // own.
    const panel = page.getByTestId('chat-editor-panel');
    await expect(panel, 'the editor must open in the chat page’s own editor column').toBeVisible({ timeout: 20_000 });

    // And THIS is gap 1: before the create-form slot was wired, the editor
    // opened with a title and a Save and nothing to fill in.
    const nameInput = page.getByTestId('agent-name-input');
    await expect(nameInput, 'the pipeline create form must render a name field').toBeVisible({ timeout: 15_000 });
    await nameInput.fill(pipelineName);
    await page.getByTestId('agent-description-input').fill(`${AUTOTEST_PREFIX}created from the chat composer`);

    // A pipeline's `instructions` IS its YAML graph, so the shared form must
    // NOT offer it as free text here (`showInstructions={false}`). A prose
    // graph stores a pipeline no runtime can compile.
    await expect(
      page.getByTestId('agent-instructions-input'),
      'the pipeline create form must not expose the YAML graph as a free-text instructions field',
    ).toHaveCount(0);

    // ── 2. Welcome message and four starters ───────────────────────────────
    await page.getByTestId('agent-welcome-message-input').fill('Welcome to test pipeline');

    const addStarter = page.getByTestId('agent-conversation-starter-add');
    const starters = ['Starter 1', 'Starter 2', 'Starter 3', 'Starter 4'];
    for (const [index, starter] of starters.entries()) {
      await expect(addStarter, `adding starter ${index + 1} must be offered`).toBeEnabled();
      await addStarter.click();
      const rows = page.getByTestId('agent-conversation-starter-input');
      await expect(rows).toHaveCount(index + 1);
      await rows.nth(index).fill(starter);
    }

    // ELITEA-0919 step 10 — four is the maximum.
    await expect(addStarter, 'a fifth conversation starter must be blocked at the cap of four').toBeDisabled();

    // ELITEA-0920 — each starter text appears exactly ONCE. Read off the rows
    // themselves rather than counted by text, because the duplication the case
    // is written about renders the same list twice: four rows holding the four
    // values in order is the only shape that satisfies both halves.
    const starterRows = page.getByTestId('agent-conversation-starter-input');
    await expect(starterRows).toHaveCount(4);
    expect(
      await starterRows.evaluateAll((nodes) => nodes.map((node) => (node as HTMLInputElement).value)),
      'each conversation starter must appear exactly once, in the order it was added',
    ).toEqual(starters);

    // …and the welcome message field is likewise ONE field, not two.
    await expect(page.getByTestId('agent-welcome-message-input')).toHaveCount(1);

    // ── 3. ELITEA-0918/0922 — the panel scrolls, the header does not ───────
    //
    // The form is now taller than the panel. Scrolling it to the bottom and
    // back must leave the header's three controls exactly where they were: the
    // header is OUTSIDE the scrolling body, which is the property the previous
    // layout did not have.
    const save = page.getByTestId('pipeline-save-button');
    const discard = page.getByRole('button', { name: 'Discard' });
    const close = page.getByRole('button', { name: 'Close editor' });
    await expect(save, 'Save must be visible before the form is scrolled').toBeVisible();

    const scrollBox = panel.locator('div').filter({ has: page.getByTestId('agent-name-input') }).last();
    await scrollBox.evaluate((node) => node.scrollTo(0, node.scrollHeight));
    await expect(save, 'Save must stay reachable at the bottom of the form').toBeVisible();
    await expect(discard, 'Discard must stay reachable at the bottom of the form').toBeVisible();
    await expect(close, 'Close must stay reachable at the bottom of the form').toBeVisible();

    await scrollBox.evaluate((node) => node.scrollTo(0, 0));
    await expect(
      page.getByTestId('agent-name-input'),
      'scrolling back to the top must bring the name field back',
    ).toBeVisible();

    // ── 4. Save, and what the SERVER holds afterwards ──────────────────────
    await expect(save).toBeEnabled({ timeout: 10_000 });
    await save.click();

    await expect
      .poll(async () => (await readParticipants(page.request, conversationId)).map((row) => row.entity_name), {
        timeout: 30_000,
        message: 'saving a pipeline created from the "+" menu must attach it to the conversation',
      })
      .toEqual(['application']);

    const attached = await readParticipants(page.request, conversationId);
    pipelineId = attached[0]?.entity_meta?.id === undefined ? undefined : String(attached[0].entity_meta.id);
    expect(pipelineId, 'the attached participant must name the pipeline that was created').toBeDefined();

    // The dead-wiring assertion. Every control above was on screen before this
    // change too; what was not stored is what it collected.
    const stored = await page.request.get(
      `${API_BASE}/elitea_core/application/prompt_lib/${DEFAULT_PROJECT_ID}/${String(pipelineId)}`,
    );
    expect(stored.ok(), 'the created pipeline must be readable').toBe(true);
    const detail = (await stored.json()) as {
      name?: string;
      version_details?: { welcome_message?: string; conversation_starters?: readonly string[] };
    };
    expect(detail.name, 'the pipeline must be stored under the name that was typed').toBe(pipelineName);
    expect(
      detail.version_details?.welcome_message,
      'the welcome message the form collected must reach the stored version',
    ).toBe('Welcome to test pipeline');
    expect(
      [...(detail.version_details?.conversation_starters ?? [])],
      'the four chat starters the form collected must reach the stored version — this is the field the create path sent as [] regardless',
    ).toEqual(starters);
  } finally {
    await deleteConversation(page.request, conversationId);
    if (pipelineId !== undefined) await deletePipeline(page.request, pipelineId);
  }
});

/* onetest: ELITEA-0928 — editing an existing pipeline participant in chat: the
   configuration sections load, the panel scrolls both ways, a change to the
   Welcome Message saves, and re-opening shows the saved value.
   ELITEA-0925 — the same create-then-edit workflow for an AGENT, exercised
   through the chat surface rather than the standalone Agents page the case
   describes (`agents.editor.spec.ts` covers that page; this is the half the
   pipeline UX refactor could regress). */
test('editing an existing pipeline participant loads its configuration, saves it, and shows the saved value on re-open', async ({ page }) => {
  const conversationId = await createConversation(page.request, uniqueName('embed-edit'));
  const pipelineName = uniqueName('ple');
  let pipelineId: string | undefined;

  try {
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    // Create one through the same flow — the participant this case needs as a
    // precondition, made the way a user makes it rather than by API, so the
    // row under test is the row the product produces.
    await openPipelineCreate(page);
    await expect(page.getByTestId('agent-name-input')).toBeVisible({ timeout: 15_000 });
    await page.getByTestId('agent-name-input').fill(pipelineName);
    await page.getByTestId('agent-description-input').fill(`${AUTOTEST_PREFIX}edit target`);
    await page.getByTestId('pipeline-save-button').click();

    await expect
      .poll(async () => (await readParticipants(page.request, conversationId)).length, {
        timeout: 30_000,
        message: 'the created pipeline must attach before it can be edited',
      })
      .toBe(1);
    const created = (await readParticipants(page.request, conversationId))[0];
    pipelineId = created?.entity_meta?.id === undefined ? undefined : String(created.entity_meta.id);
    expect(pipelineId).toBeDefined();

    // Close the create editor, then re-open the SAME participant for editing
    // through the participant rail — the gesture the case describes.
    await page.getByRole('button', { name: 'Close editor' }).click();
    await expect(page.getByTestId('chat-editor-panel')).toBeHidden({ timeout: 15_000 });

    await openParticipantEditor(page, pipelineName);

    // ── The sections the case lists ────────────────────────────────────────
    //
    // This is gap 2: with `renderConfigurationPanels` unwired the Configuration
    // tab rendered NOTHING, and Save was disabled because `onSaveVersion` was
    // absent too.
    const panel = page.getByTestId('chat-editor-panel');
    await expect(panel).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId('agent-name-input'), 'Name must load').toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId('agent-description-input'), 'Description must load').toBeVisible();
    await expect(page.getByTestId('agent-welcome-message-input'), 'Welcome Message must load').toBeVisible();
    await expect(
      page.getByTestId('agent-conversation-starters-section'),
      'Conversation Starters must load',
    ).toBeVisible();

    // The stored name must be PRE-POPULATED, not blank: a form that renders
    // its controls but seeds none of them saves an empty name over a real one.
    await expect(page.getByTestId('agent-name-input')).toHaveValue(pipelineName);

    // ── Change the welcome message and save ────────────────────────────────
    const updated = 'Updated welcome message for TC17';
    await page.getByTestId('agent-welcome-message-input').fill(updated);
    const save = page.getByTestId('pipeline-save-button');
    await expect(save, 'Save must be armed once the edit form has a version to write').toBeEnabled({ timeout: 15_000 });
    await save.click();

    // Server-side first — the screen clears its dirty flag either way.
    await expect
      .poll(
        async () => {
          const response = await page.request.get(
            `${API_BASE}/elitea_core/application/prompt_lib/${DEFAULT_PROJECT_ID}/${String(pipelineId)}`,
          );
          const body = (await response.json()) as { version_details?: { welcome_message?: string } };
          return body.version_details?.welcome_message;
        },
        { timeout: 30_000, message: 'the edited welcome message must reach the stored version' },
      )
      .toBe(updated);

    // ── Re-open: the panel shows what was saved ────────────────────────────
    await page.getByRole('button', { name: 'Close editor' }).click();
    await expect(page.getByTestId('chat-editor-panel')).toBeHidden({ timeout: 15_000 });
    await openParticipantEditor(page, pipelineName);
    await expect(page.getByTestId('agent-welcome-message-input')).toHaveValue(updated, { timeout: 20_000 });
  } finally {
    await deleteConversation(page.request, conversationId);
    if (pipelineId !== undefined) await deletePipeline(page.request, pipelineId);
  }
});
