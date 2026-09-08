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
 * And three legacy use cases have NO port target in this app, which is a
 * disclosed product gap rather than a missing test — `pages/agents/
 * Applications.tsx`'s own module comment records both:
 *
 *  - `test_agents_dashboard_loads` (the list's search box),
 *    `test_agent_search`, `test_agent_search_no_results` — the agents list
 *    carries no search control. The baseline's `StatusFilterSelect` and the
 *    search field beside it were never ported.
 *  - `test_view_toggle_table_and_card` — `ViewToggle` (`@/components/
 *    ViewToggle` in the baseline) has no port anywhere in `shared/ui` or
 *    `widgets`, so there is no table view to switch to.
 *
 * A journey for any of those would have to assert on a control that does not
 * exist, which is the `if (await x.isVisible())` shape this suite exists to
 * refuse.
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

import type { Page } from '@playwright/test';

/**
 * The platform's own limit, from `src/shared/lib/limits.ts`
 * (`MAX_WELCOME_MESSAGE_LENGTH` / `MAX_CONVERSATION_STARTER_LENGTH`). The
 * legacy suite hard-codes 768 for both out of EliteaUI's `common/constants.js`;
 * this app agrees, and the constant is repeated here so a future divergence
 * fails as a changed number rather than as a mystery.
 */
const MAX_TEXT_FIELD_CHARS = 768;

/** A name unique per run, so two runs on one stack cannot collide. */
function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
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
 *    text is added, reaches "0 characters left" at the limit, and the field
 *    refuses the character after it. Both fields, since both carry their own
 *    counter.
 *  - NOT COVERED, because this app does not implement it: the legacy tests
 *    also assert a RED counter and a "You have reached the MAXIMUM character
 *    limit" sentence at zero (their acceptance criteria 2-4), and a FULLSCREEN
 *    dialog carrying the same counter. `WelcomeMessageInput.tsx` and
 *    `ConversationStartersEditor.tsx` render the count alone, in one style, in
 *    one mode. Asserting the warning here would fail for a reason this journey
 *    cannot fix; it is a real parity gap, recorded here rather than smuggled in
 *    behind an `if (visible)`. The pattern EXISTS in this app —
 *    `features/settings/ui/project-context/EditorSection.tsx` shows both the
 *    red state and that exact sentence — so the gap is a missing application of
 *    it, not a missing capability.
 *
 * The counter text is a template literal in both components, NOT an `en.json`
 * key, which is why this journey asserts the rendered string rather than a
 * bundle entry.
 */
test('J14c: the welcome message and chat starters count down to 768 and refuse the 769th character', async ({
  page,
  request,
}) => {
  const name = uniqueName('limits');
  const agent = await createAgent(request, name);
  const atLimit = 'X'.repeat(MAX_TEXT_FIELD_CHARS);
  try {
    await openAgentEditor(page, agent.id);
    const panel = page.getByTestId('edit-application-configuration-tab-panel');

    const welcome = panel.getByTestId('agent-welcome-message-input');
    await expect(welcome).toBeVisible({ timeout: 20_000 });
    // The contract the browser itself enforces, before any counter is read.
    await expect(welcome).toHaveAttribute('maxlength', String(MAX_TEXT_FIELD_CHARS));

    // The counter is focus-gated (`useFieldFocus`), so it shows only while the
    // field is being edited — `fill` leaves the field focused.
    await welcome.fill('Hello');
    const welcomeCounter = panel.getByTestId('agent-welcome-message-counter');
    await expect(welcomeCounter).toHaveText(`${MAX_TEXT_FIELD_CHARS - 5} characters left`);

    await welcome.fill(atLimit);
    await expect(welcomeCounter).toHaveText('0 characters left');

    // A REAL keystroke, not another `fill`: `fill` assigns the value and walks
    // straight past the `maxLength` the browser enforces on typed input, so it
    // would prove nothing about the refusal.
    await welcome.press('End');
    await welcome.pressSequentially('Z');
    expect((await welcome.inputValue()).length).toBe(MAX_TEXT_FIELD_CHARS);
    await expect(welcomeCounter).toHaveText('0 characters left');

    // The same contract on a chat starter. The agent is created with none, so
    // the row has to be added first.
    await panel.getByTestId('agent-conversation-starter-add').click();
    const starter = panel.getByTestId('agent-conversation-starter-input').first();
    await expect(starter).toBeVisible({ timeout: 10_000 });
    await expect(starter).toHaveAttribute('maxlength', String(MAX_TEXT_FIELD_CHARS));

    await starter.fill(atLimit);
    await expect(panel.getByTestId('agent-conversation-starter-counter').first()).toHaveText('0 characters left');
    await starter.press('End');
    await starter.pressSequentially('Z');
    expect((await starter.inputValue()).length).toBe(MAX_TEXT_FIELD_CHARS);
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
    expect(answered.status(), await answered.text()).toBeLessThan(400);
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
