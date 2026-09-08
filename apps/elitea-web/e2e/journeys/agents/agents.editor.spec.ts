/**
 * Journey 14c: the agent EDITOR's own surface — the Information section, the
 * create form's required-field gate, the stored instructions, the two
 * character-counted fields, and delete.
 *
 * Ported BY USE CASE from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/`), not one-for-one. Each
 * test below names the legacy test it answers. The use cases already covered
 * elsewhere are NOT repeated here:
 *
 *  - `test_create_agent_via_ui`, `test_agent_detail_page_loads`,
 *    `test_edit_agent_name`, `test_edit_agent_description` — J14 in
 *    `agents.lifecycle.spec.ts` creates through the form, saves, reloads and
 *    reads name/description/welcome message back off the server.
 *  - `test_create_agent_via_api_visible_in_ui` — J14 already asserts a saved
 *    agent is listed back on `/app/agents/my`.
 *  - `test_agent_toolkits_section_visible` / `test_internal_tools_enum_usage`
 *    — the toolkit grid and its attach/detach round trip are J17's
 *    (`e2e/journeys/toolkits/`), driven through the real toolkit routes.
 *
 *  - `test_agents_dashboard_loads`, `test_agent_search`,
 *    `test_agent_search_no_results` and `test_view_toggle_table_and_card` —
 *    all four are `agents.list.spec.ts` (J14e). This file used to record them
 *    as product gaps: the list carried no search control and `ViewToggle` had
 *    no port. Both are real now (`widgets/page-header`'s `ListSearchField` and
 *    `ListViewToggle`, mounted by `pages/agents/Applications.tsx`), so the use
 *    cases are journeys rather than disclosures.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  clickCreateButton,
  createAgent,
  deleteAgent,
} from '../../fixtures/api';

import type { APIRequestContext, Page } from '@playwright/test';

/**
 * The platform's own limit, from `src/shared/lib/limits.ts`
 * (`MAX_WELCOME_MESSAGE_LENGTH` / `MAX_CONVERSATION_STARTER_LENGTH`). The
 * legacy suite hard-codes 768 for both out of EliteaUI's `common/constants.js`;
 * this app agrees, and the constant is repeated here so a future divergence
 * fails as a changed number rather than as a mystery.
 */
const MAX_TEXT_FIELD_CHARS = 768;

/**
 * The counter's rendered sentence AT the limit.
 *
 * `shared/ui/CharacterCounter` appends a second bundle key once `remaining`
 * hits zero (`shared.ui.characterCounter.atLimit`, ". You have reached the
 * MAXIMUM character limit") on top of the running
 * `shared.ui.characterCounter.remaining` ("characters left") — the WCAG 1.4.1
 * half of the legacy acceptance criteria, so the limit is not signalled by
 * colour alone. Below the limit the element carries the count alone, which is
 * why only the zero-remaining assertions use this constant.
 */
const AT_LIMIT_COUNTER_TEXT = '0 characters left. You have reached the MAXIMUM character limit';

/** A name unique per run, so two runs on one stack cannot collide. */
function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
}

/**
 * Creates an agent that already HOLDS one chat starter.
 *
 * The shared `createAgent` seeds `conversation_starters: []`, and the journey
 * below needs a starter row on screen at load. It cannot click `+ Starter` to
 * get one — see that test's own comment for the click-swallowing defect this
 * routes around — so the row is seeded server-side instead.
 *
 * A local helper rather than a new parameter on `e2e/fixtures/api.ts`:
 * that file is edited by every journey unit in this wave at once, and one
 * caller's convenience is not worth the merge.
 */
async function createAgentWithStarter(
  request: APIRequestContext,
  name: string,
  starter: string,
): Promise<{ readonly id: string; readonly versionId: string }> {
  const path = `${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}`;
  const response = await request.post(path, {
    data: {
      name,
      description: `${AUTOTEST_PREFIX}e2e character-limit agent`,
      type: 'agent',
      versions: [
        {
          name: 'base',
          agent_type: 'openai',
          instructions: 'You are a helpful assistant.',
          conversation_starters: [starter],
        },
      ],
    },
  });
  expect(response.ok(), `seeding the agent returned ${response.status()}`).toBe(true);
  const body = await response.json();
  // Asserted, not assumed: a server that silently dropped the starter would
  // otherwise surface as an unexplained missing row in the browser.
  expect(body?.version_details?.conversation_starters, 'the server must store the seeded starter').toEqual([starter]);
  return { id: String(body.id), versionId: String(body.version_details.id) };
}

/** Opens an existing agent's editor and waits for the real configuration panel. */
async function openAgentEditor(page: Page, agentId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/agents/all/${agentId}`);
  await expect(page.getByTestId('edit-application-configuration-tab-panel')).toBeVisible({ timeout: 20_000 });
}

/*
 * Legacy: `agents/test_agent_management.py::TestAgentConfiguration::
 * test_agent_information_section` — "Information section should display Agent
 * ID and Version ID".
 *
 * This is issue #846. `features/agents/ui/ApplicationInformation.tsx` carried
 * `agent-information-section` and was mounted by the PIPELINE editor alone, so
 * 17 tests in the legacy suite timed out on that id while the component itself
 * was correct and unit-tested. The fix is one mount point in
 * `pages/agents/ui/EditApplicationConfigurationPanel.tsx`.
 *
 * Asserted against the ids the SERVER answered with, not against whatever the
 * page happens to show: the agent id is already in the URL the browser holds,
 * so a panel that echoed the URL back would satisfy a self-consistent check.
 * The version id can only come from the detail read.
 */
test('J14c: the agent editor shows an Information section carrying the ids the server reports', async ({
  page,
  request,
}) => {
  const name = uniqueName('information');
  const agent = await createAgent(request, name);
  try {
    await openAgentEditor(page, agent.id);

    const panel = page.getByTestId('edit-application-configuration-tab-panel');
    const information = panel.getByTestId('agent-information-section');
    // Inside THIS panel — the defect was a missing mount point, and a section
    // rendered anywhere else on the page would satisfy a bare presence check.
    await expect(information).toBeVisible({ timeout: 20_000 });

    await expect(information.getByTestId('copy-id')).toHaveText(agent.id);
    await expect(information).toContainText('Version ID:');
    await expect(information).toContainText(agent.versionId);

    // An agent is not a pipeline: the trigger and "Show pipeline" rows belong
    // to `/pipelines/:tab/:agentId` and must stay off here.
    await expect(information).not.toContainText('Trigger:');
    await expect(information).not.toContainText('Pipeline:');
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/*
 * Legacy: `agents/test_agent_management.py::TestCreateAgent::
 * test_create_agent_required_fields_validation` — "Save button should be
 * disabled when required fields are empty".
 *
 * J14 asserts only the LAST of the three states (Save enabled once both
 * fields are filled, through its `fillAgentForm`). The gate itself — Save
 * refused on an empty form, and refused again with the name alone — was
 * untested, so a form that enabled Save immediately passed every journey.
 */
test('J14c: the create form keeps Save disabled until BOTH required fields carry a value', async ({ page }) => {
  await page.goto(BASE_URL + '/app/agents/my');
  await page.waitForURL('**/agents**', { timeout: 15_000 });

  await clickCreateButton(page);
  const nameInput = page.getByTestId('agent-name-input');
  await expect(nameInput).toBeVisible({ timeout: 10_000 });

  const save = page.getByTestId('agent-save-button');
  await expect(save).toBeDisabled();

  // Name alone is not enough: `applicationCreationSchema`
  // (entities/application-form/model/validation.ts) requires a description too.
  await nameInput.fill(uniqueName('validation'));
  await expect(save).toBeDisabled();

  await page.getByTestId('agent-description-input').fill('Filled by the required-fields journey.');
  await expect(save).toBeEnabled({ timeout: 5_000 });

  // Nothing is saved, and the form is emptied rather than navigated away from:
  // leaving a dirty form arms the unsaved-changes guard J25 owns.
  await nameInput.fill('');
  await page.getByTestId('agent-description-input').fill('');
  await expect(save).toBeDisabled();
});

/*
 * Legacy: `agents/test_agent_management.py::TestAgentConfiguration::
 * test_agent_instructions_field` — "Instructions field should be visible and
 * editable", asserting the fixture's own instructions are read back.
 *
 * READ-ONLY on purpose, and narrower than the legacy test's "editable" claim.
 * The instructions box is a CodeMirror document
 * (`features/agents/ui/InstructionsInput.tsx` over `shared/ui/
 * CodeMirrorEditor`), not an `<input>`; driving keystrokes into it across
 * chromium AND webkit is a different piece of work from proving the stored
 * instructions reach the screen, and the persistence half of the use case is
 * already covered — for name, description and welcome message — by J14. What
 * was missing is that this field shows the server's value at all.
 */
test('J14c: the editor shows the instructions the agent was stored with', async ({ page, request }) => {
  const name = uniqueName('instructions');
  const agent = await createAgent(request, name);
  try {
    await openAgentEditor(page, agent.id);

    // `createAgent` seeds exactly this sentence.
    const instructions = page.getByRole('textbox', { name: 'Instructions' });
    await expect(instructions).toBeVisible({ timeout: 20_000 });
    await expect(instructions).toContainText('You are a helpful assistant.', { timeout: 20_000 });
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/*
 * Legacy: all nine tests of `agents/test_agent_character_limits.py` — the
 * welcome-message and conversation-starter counters (issue #4963 in the legacy
 * tracker).
 *
 * ONE journey, not nine, and deliberately narrower than the legacy suite:
 *
 *  - COVERED — the counter appears while the field is focused, counts down as
 *    text is added, reaches "0 characters left. You have reached the MAXIMUM
 *    character limit" at the limit, and the field
 *    refuses the character after it. Both fields, since both carry their own
 *    counter.
 *  - COVERED NOW — the legacy acceptance criteria 2-4: the counter changes
 *    colour at zero and says "You have reached the MAXIMUM character limit".
 *    This journey used to record both as an unimplemented parity gap. Five
 *    files each rendered their own bare count, and `shared/ui/CharacterCounter`
 *    — which had always carried both states — had no callers at all. All five
 *    call it now.
 *  - COVERED NOW — the FULLSCREEN mode. The legacy fullscreen cases open the
 *    field on a large surface; this app's expand affordance is on the
 *    INSTRUCTIONS editor (`InstructionsFullscreenButton`), which is the field
 *    the reference gives one to and the longest field on the page. Asserted
 *    below.
 *
 * The counter's copy comes from `shared/ui/CharacterCounter`'s own two bundle
 * keys, which is why the strings below are the rendered sentence rather than a
 * template literal this file rebuilds.
 */
test('J14c: the welcome message and chat starters count down to 768 and refuse the 769th character', async ({
  page,
  request,
}) => {
  const name = uniqueName('limits');
  const seededStarter = 'Seeded by the character-limit journey.';
  const agent = await createAgentWithStarter(request, name, seededStarter);
  const atLimit = 'X'.repeat(MAX_TEXT_FIELD_CHARS);
  try {
    await openAgentEditor(page, agent.id);
    const panel = page.getByTestId('edit-application-configuration-tab-panel');

    /*
     * The STARTER half runs first, and on a row the API seeded rather than one
     * clicked into existence.
     *
     * The first version of this journey clicked `+ Starter`, and it failed on
     * CI in both browsers with the row never appearing. That is not a flake and
     * not a missing control — it is a real defect, reproduced against a live
     * stack and measured:
     *
     *   the counter under the welcome message is focus-gated, so BLURRING that
     *   field unmounts a 24px line, and every control below it — `+ Starter`
     *   among them — jumps up by exactly that much. A real mouse click sends
     *   mousedown (which blurs), the layout shifts 24px, and mouseup lands off
     *   the ~24px-tall button, so no click event is ever produced. Clicking a
     *   SECOND time, with the field already blurred, adds the row normally.
     *
     * Filed as #848, fixed there by reserving the counter's line
     * (`visibility: hidden` instead of unmounting) in both
     * `WelcomeMessageInput.tsx` and `ConversationStartersEditor.tsx`, so
     * blur no longer shifts a control sitting below either counter. This
     * journey still seeds the starter through the API rather than clicking
     * `+ Starter` into existence: its subject is the 768-character contract,
     * not the add button, and `ConversationStartersEditor.test.tsx` /
     * `CreateAgentForm.test.tsx` already cover a single click on `+ Starter`
     * while the welcome field is focused. A dedicated journey for the add
     * interaction itself is still open work (see #848's own "Test impact" note).
     */
    const starter = panel.getByTestId('agent-conversation-starter-input').first();
    await expect(starter).toBeVisible({ timeout: 20_000 });
    await expect(starter).toHaveValue(seededStarter);
    await expect(starter).toHaveAttribute('maxlength', String(MAX_TEXT_FIELD_CHARS));

    await starter.fill(atLimit);
    // The counter is focus-gated (`useFieldFocus`), so it shows only while the
    // field is being edited — `fill` leaves the field focused.
    await expect(panel.getByTestId('agent-conversation-starter-counter').first()).toHaveText(AT_LIMIT_COUNTER_TEXT);

    // A REAL keystroke, not another `fill`: `fill` assigns the value and walks
    // straight past the `maxLength` the browser enforces on typed input, so it
    // would prove nothing about the refusal.
    await starter.press('End');
    await starter.pressSequentially('Z');
    expect((await starter.inputValue()).length).toBe(MAX_TEXT_FIELD_CHARS);

    // The same contract on the welcome message, which carries its own counter.
    const welcome = panel.getByTestId('agent-welcome-message-input');
    await expect(welcome).toBeVisible();
    // The contract the browser itself enforces, before any counter is read.
    await expect(welcome).toHaveAttribute('maxlength', String(MAX_TEXT_FIELD_CHARS));

    await welcome.fill('Hello');
    const welcomeCounter = panel.getByTestId('agent-welcome-message-counter');
    await expect(welcomeCounter).toHaveText(`${MAX_TEXT_FIELD_CHARS - 5} characters left`);

    await welcome.fill(atLimit);
    await expect(welcomeCounter).toHaveText(AT_LIMIT_COUNTER_TEXT);

    await welcome.press('End');
    await welcome.pressSequentially('Z');
    expect((await welcome.inputValue()).length).toBe(MAX_TEXT_FIELD_CHARS);
    await expect(welcomeCounter).toHaveText(AT_LIMIT_COUNTER_TEXT);

    /*
     * The colour, measured rather than assumed. `toHaveCSS` reads the computed
     * value, so a rule that never applied — the exact failure a `class`
     * assertion would miss — shows up as the neutral colour still being there.
     *
     * The two states are compared against each other rather than against a
     * literal `rgb(...)`: the token is the theme's (`text.warningText`), and
     * pinning its hex here would fail the next time the brand pack moves,
     * for a reason that is not a regression.
     */
    const atLimitColour = await welcomeCounter.evaluate((node) => getComputedStyle(node).color);
    await welcome.fill('Hello');
    await expect(welcomeCounter).toHaveText(`${MAX_TEXT_FIELD_CHARS - 5} characters left`);
    const belowLimitColour = await welcomeCounter.evaluate((node) => getComputedStyle(node).color);
    expect(atLimitColour, 'the counter must change colour at the limit').not.toBe(belowLimitColour);
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/*
 * Legacy: the FULLSCREEN half of `agents/test_agent_character_limits.py`
 * (`test_welcome_message_fullscreen_counter_and_warning` and its siblings),
 * which open a field on a large surface and edit it there.
 *
 * This app puts the expand affordance on the INSTRUCTIONS editor, which is the
 * field the reference gives one to and the longest field on the page. The
 * legacy tests self-skip when the expand button is absent — the shape this
 * suite refuses — so this asserts the dialog and the round trip instead.
 *
 * The text typed in the dialog is read back from the SERVER, not from the
 * inline editor: a dialog that edited a copy of the value would still show the
 * new text on screen and save the old one.
 */
test('J14c: the instructions editor opens full screen, and what is typed there is what gets saved', async ({
  page,
  request,
}) => {
  const name = uniqueName('fullscreen');
  const agent = await createAgent(request, name);
  const typed = 'Answer only in haiku.';
  try {
    await openAgentEditor(page, agent.id);

    await page.getByTestId('agent-instructions-fullscreen-button').click();
    const dialog = page.getByTestId('agent-instructions-fullscreen-dialog');
    await expect(dialog, 'the instructions must open on a fullscreen surface').toBeVisible({ timeout: 20_000 });

    // The dialog carries the CURRENT instructions, not an empty editor.
    await expect(dialog).toContainText('You are a helpful assistant.', { timeout: 20_000 });

    const editor = dialog.locator('.cm-content');
    await editor.click();
    await page.keyboard.press('ControlOrMeta+a');
    await page.keyboard.type(typed);
    await expect(dialog).toContainText(typed);

    await page.keyboard.press('Escape');
    await expect(dialog).toHaveCount(0, { timeout: 10_000 });

    await page.getByTestId('agent-save-button').click();

    await expect
      .poll(
        async () => {
          const detail = await request.get(
            `${API_BASE}/elitea_core/application/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.id}`,
          );
          const body = await detail.json();
          return String(body?.version_details?.instructions ?? '');
        },
        { timeout: 30_000, message: 'the instructions typed in the fullscreen editor never reached the server' },
      )
      .toContain(typed);
  } finally {
    await deleteAgent(request, agent.id);
  }
});

/*
 * Legacy: `agents/test_agent_management.py::TestAgentActions::
 * test_delete_agent_via_ui_menu`, and the read-back half of
 * `test_delete_agent_via_api` ("verify it's gone from the UI").
 *
 * DISCLOSED DEVIATION: the legacy test opens a three-dot menu. This app puts
 * the delete affordance directly on the editor's action bar
 * (`pages/agents/ui/EditApplicationActions.tsx`, mounting `features/agents`'
 * `DeleteApplicationButton`) behind a type-the-name confirmation. The use
 * case — a person deletes an agent from its own page and it leaves the list —
 * is the same; the control is not.
 *
 * The server is asked directly at the end, not only the list: a list that
 * merely dropped a cached row would satisfy a UI-only check.
 */
test('J14c: deleting an agent from its editor removes it from the list and from the server', async ({
  page,
  request,
}) => {
  const name = uniqueName('delete-ui');
  const agent = await createAgent(request, name);
  let deleted = false;
  try {
    await openAgentEditor(page, agent.id);
    await expect(page.getByTestId('agent-name-input')).toHaveValue(name, { timeout: 20_000 });

    await page.getByRole('button', { name: 'delete entity' }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog).toBeVisible({ timeout: 10_000 });

    // Type-to-confirm: `DeleteEntityModal`'s `shouldRequestInputName` keeps
    // Confirm disabled until the typed name matches exactly.
    const confirm = dialog.getByRole('button', { name: 'Delete', exact: true });
    await expect(confirm).toBeDisabled();
    await dialog.getByLabel('Name').fill(name);
    await expect(confirm).toBeEnabled();

    const deleteResponse = page.waitForResponse(
      (response) =>
        response.request().method() === 'DELETE' && /\/elitea_core\/application\/prompt_lib\//.test(response.url()),
    );
    await confirm.click();
    const answered = await deleteResponse;
    /*
     * STATUS ONLY — never `await answered.text()` here. A successful delete
     * answers 204 with no body at all, and webkit then fails the whole test on
     * the read itself ("Protocol error (Network.getResponseBody): Missing
     * content of resource") before any assertion runs. The status IS the
     * contract; the server read below is what proves the row is gone.
     */
    expect(answered.status(), `DELETE ${answered.url()}`).toBeLessThan(400);
    deleted = true;

    // The page leaves the deleted agent behind, for the list it came from.
    await page.waitForURL(/\/agents\/[^/]+$/, { timeout: 15_000 });

    // The server's own read is the proof; the list is the screen the person
    // looks at. Polled, because the delete invalidates the list cache
    // asynchronously.
    await expect
      .poll(
        async () => {
          const detail = await request.get(
            `${API_BASE}/elitea_core/application/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.id}`,
          );
          return detail.status();
        },
        { timeout: 20_000 },
      )
      .toBeGreaterThanOrEqual(400);

    await page.goto(BASE_URL + '/app/agents/my');
    await expect(page.getByText(name)).toHaveCount(0, { timeout: 20_000 });
  } finally {
    if (!deleted) await deleteAgent(request, agent.id);
  }
});
