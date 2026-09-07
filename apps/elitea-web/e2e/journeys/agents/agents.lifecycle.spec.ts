/**
 * Journey 14: Create agent → save → publish → unpublish (JRNY-014)
 * Journey 15: Create a new version → set default → delete old (JRNY-015)
 * Journey 25: Unsaved-changes nav block (JRNY-025)
 *
 * Spec §8.5 acceptance (from parity/manifest/agents.json JRNY-014/015/025).
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, clickCreateButton, createAgent, deleteAgent } from '../../fixtures/api';

import type { Page } from '@playwright/test';

/**
 * Opens the create-agent form and asserts the REAL form is on screen.
 *
 * Deliberately no `.or(getByRole('heading', …))` fallback: a heading is exactly
 * what a stub route renders, so accepting one means the journey passes against
 * an unimplemented screen. Three copies of that fallback lived in this file and
 * were the reason J14/J15/J25 could not tell a working page from a placeholder.
 */
async function openCreateAgentForm(page: Page): Promise<void> {
  await clickCreateButton(page);
  await expect(page.getByTestId('agent-name-input')).toBeVisible({ timeout: 10_000 });
  await expect(page.getByTestId('agent-description-input')).toBeVisible();
}

/**
 * Fills both fields the create form requires. `applicationCreationSchema`
 * (entities/application-form/model/validation.ts) requires name AND
 * description; filling only the name leaves Save correctly disabled, which is
 * why J14/J15 used to time out clicking it.
 */
async function fillAgentForm(page: Page, name: string): Promise<void> {
  await page.getByTestId('agent-name-input').fill(name);
  await page.getByTestId('agent-description-input').fill(`${name} description`);
  await expect(page.getByTestId('agent-save-button')).toBeEnabled({ timeout: 5_000 });
}

// ─────────────────────────────────────────────────────────────────────────────
// Journey 14: Create agent, save, publish, unpublish
// ─────────────────────────────────────────────────────────────────────────────
/*
 * NOT COVERED — publish / unpublish (#120). JRNY-014's §8.5 text names them;
 * this journey is PERMANENTLY NARROWER than that text, and a green run of it
 * must never be read as covering them. It used to "cover" them with
 * `if (await publishButton.isVisible().catch(() => false))`, which silently
 * passed because no such control exists.
 *
 * RE-TRIAGED 2026-08-09, and #120's original premise ("designed in Figma,
 * no UI") is now only half true. The backend is NOT the gap — it is fully
 * implemented and routed:
 *
 *   POST /api/v2/elitea_core/publish/prompt_lib/{projectID}/{versionID}
 *        services/elitea-main/internal/api/router.go:854 -> eliteacore
 *        handler.go:482 (real: version-name validation, pipeline rejection,
 *        409 on already-published, category validation, then clones the
 *        version to `status='published'` and embeds sub-agents)
 *   POST /api/v2/elitea_core/unpublish/prompt_lib/{projectID}/{versionID}
 *        router.go:855 -> handler.go:835 (reverts published/embedded clones
 *        to `draft`, 409 when not published)
 *   GET|POST /api/v2/elitea_core/publish_validate/prompt_lib/{p}/{v}
 *        router.go:856-857 -> handler.go:1227
 *
 * The generated web client already wraps all three —
 * `src/shared/api/generated/applications/applications.ts`'s
 * `usePublishApplication` (:2144), `useUnpublishApplication` (:2428),
 * `useValidateForPublish` (:2703) — and `src/shared/api/endpoints.manifest.json`
 * records `"usedBy": []` for both publish entries (:451-458, :511-518). So
 * what is missing is exactly one layer: no component, hook or menu item in
 * elitea-web calls them. The legacy UI's equivalents
 * (`usePublishVersionMenu.hooks.jsx` / `useUnpublishVersionMenu.hooks.jsx` /
 * `PublishWizardModal.jsx` under EliteaUI's `[fsd]/entities/version`) were
 * never ported.
 *
 * NOT built in this change, and deliberately not stubbed: publishing runs a
 * multi-step wizard (validation step, terms, category selection) against
 * `publish_validate`, and a half-wired "Publish" button that skipped it would
 * look finished while being wrong. Tracked in #120 with the evidence above.
 */
test('J14: create agent, save, and persist it', async ({ page }) => {
  await page.goto(BASE_URL + '/app/agents/my');
  await page.waitForURL('**/agents**', { timeout: 15_000 });

  await checkA11y(page);

  await openCreateAgentForm(page);
  await fillAgentForm(page, `${AUTOTEST_PREFIX}e2e-agent`);

  await page.getByTestId('agent-save-button').click();

  // A real server round trip: the created agent must be addressable. This is
  // the assertion a heading-only route cannot satisfy — it requires the POST to
  // have succeeded and the router to have navigated to the new agent's id.
  await page.waitForURL(/\/agents\/[^/]+\/[^/]+/, { timeout: 15_000 });

  // Assert on the EDIT page's own panel, not just on a populated name input.
  // Proven necessary by mutation: with this route reverted to a heading-only
  // stub, `expect(agent-name-input).toHaveValue(...)` still PASSED — the create
  // page's input stays mounted under the parent route long enough to satisfy it,
  // so the journey went green against a stub. This testid exists only on
  // EditApplication, and (since the panel is no longer a self-closing Box) is
  // only non-empty when the real form is inside it.
  const editPanel = page.getByTestId('edit-application-configuration-tab-panel');
  await expect(editPanel).toBeVisible({ timeout: 10_000 });
  await expect(editPanel.getByTestId('agent-name-input')).toHaveValue(`${AUTOTEST_PREFIX}e2e-agent`);

  // And it must be listed back on the agents index — proving it was persisted,
  // not merely held in client state.
  await page.goto(BASE_URL + '/app/agents/my');
  await expect(page.getByText(`${AUTOTEST_PREFIX}e2e-agent`).first()).toBeVisible({ timeout: 15_000 });

  await checkA11y(page);
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 15: Create a new version, set default, delete old
// ─────────────────────────────────────────────────────────────────────────────
/*
 * COVERED IN FULL as of #147. JRNY-015 names "create a new version -> set
 * default -> delete old", and all three steps were once wrapped in
 * `if (await x.isVisible().catch(() => false))`, so the test passed whether
 * or not those controls existed. Every assertion below is unconditional:
 *
 *  - create a new version: #134 mounted `AgentVersionControls`; the Save-As-
 *    Version round trip is asserted end to end (dialog -> POST -> navigate
 *    onto the new version -> both versions listed).
 *  - set default: #147 put the command item in the version menu. Asserted
 *    through the real PATCH and through the menu's own "Default" marker
 *    afterwards, NOT merely as an item on screen — an item wired to nothing
 *    would satisfy a presence-only check.
 *  - delete old: #147 moved the delete affordance INTO the same menu, where
 *    the baseline puts it, and gave it a type-the-name confirm. The whole
 *    round trip is asserted here — the item, the typed name, the real DELETE
 *    and the version leaving the list.
 *
 * The one thing NOT asserted here is the server's 400 refusal
 * ("Unpublish first. Cannot delete a published version."). Reaching it needs
 * a PUBLISHED version, and publishing is not an affordance this app mounts
 * yet. That branch is proved where it is produced and where it is shown:
 * `services/elitea-main/internal/api/v2/applications/
 * delete_version_postgres_integration_test.go` pins the status and the exact
 * text, and `features/agents/ui/AgentVersionControls.test.tsx` pins that the
 * same text reaches the dialog and leaves it open.
 */
test('J15: a saved agent exposes a version selector listing its versions', async ({ page }) => {
  /*
   * Was EXPECTED-FAIL until #134. The agent edit page mounted no
   * version-selection control at all: it fetched the version list and spent it
   * solely on `useIsVersionNotFound`'s 404 check, while both halves of the
   * baseline's version bar sat in the tree unreachable —
   * `AgentPipelineVersionSelector` importable only from `ToolCardBody`
   * (a TOOL card, not the agent's own page) and `SaveNewVersionButton` with no
   * production importer whatsoever. `pages/agents/EditApplication.tsx` now
   * mounts `features/agents`' `AgentVersionControls`, which composes both.
   *
   * The control asserted on below is the REAL production selector: nothing in
   * this file stubs, injects or rewrites the served bundle.
   */
  await page.goto(BASE_URL + '/app/agents/my');
  await page.waitForURL('**/agents**', { timeout: 15_000 });

  await openCreateAgentForm(page);
  await fillAgentForm(page, `${AUTOTEST_PREFIX}version-test-agent`);

  await page.getByTestId('agent-save-button').click();

  // Hard: the save must land on a real agent URL carrying an id.
  await page.waitForURL(/\/agents\/[^/]+\/[^/]+/, { timeout: 15_000 });

  // The version selector is the subject of this journey. It must exist — a
  // `test.skip('Version selector not found in this build')` here turned a
  // missing feature into a green run.
  const versionTrigger = page.getByTestId('version-selector-trigger');
  await expect(versionTrigger).toBeVisible({ timeout: 10_000 });

  // The saved agent starts at exactly one version, and the selector must NAME
  // it. `count > 0` alone does not prove that: with an empty version list the
  // menu still renders one `menuitem` — its own disabled "No versions
  // available" placeholder — so a selector wired to nothing satisfied the old
  // assertion. Mutation-verified both ways (see the PR): forcing
  // `versions={[]}` into `AgentVersionControls` keeps the count assertion
  // green and fails only the two below.
  await versionTrigger.click();
  const versionOptions = page.getByRole('menuitem');
  await expect(versionOptions.first()).toBeVisible({ timeout: 5_000 });
  await expect(page.getByRole('menuitem', { name: /no versions available/i })).toHaveCount(0);
  const optionNames = (await versionOptions.allInnerTexts()).map((text) => text.trim()).filter((text) => text !== '');
  expect(optionNames.length).toBeGreaterThan(0);

  await page.keyboard.press('Escape');

  // The other half of #134: `SaveNewVersionButton` had no production importer
  // at all. Assert it end to end — the button, its dialog, the POST, and the
  // resulting version appearing in the selector — rather than merely that a
  // button is on screen, which a disconnected render would also satisfy.
  await page.getByRole('button', { name: /save as version/i }).click();
  const versionDialog = page.getByRole('dialog');
  await expect(versionDialog).toBeVisible({ timeout: 5_000 });
  await versionDialog.getByRole('textbox').fill('v2');
  await versionDialog.getByRole('button', { name: /^save$/i }).click();

  // A real server round trip: the created version must be addressable, i.e.
  // the page navigates onto `/agents/:tab/:id/:version` with the NEW id.
  await page.waitForURL(/\/agents\/[^/]+\/[^/]+\/\d+/, { timeout: 15_000 });

  // …and the dropdown must now list both versions, which only holds if the
  // detail query was invalidated after the POST.
  await expect(versionTrigger).toBeVisible({ timeout: 10_000 });
  await versionTrigger.click();
  await expect(page.getByRole('menuitem', { name: /v2/i })).toBeVisible({ timeout: 10_000 });
  expect(await page.getByRole('menuitem').count()).toBeGreaterThan(1);

  /*
   * JRNY-015 step 2 — SET DEFAULT (#147). Unconditional, and deliberately
   * not `if (await item.isVisible().catch(() => false))`: that shape is what
   * hid this gap for three rounds, and the endpoint it reaches
   * (PATCH /api/v2/elitea_core/default_version/prompt_lib/{p}/{app}/{ver},
   * router.go:1778 -> applications/handler.go:1202 -> repos/applications.go:650)
   * had a full backend and no caller anywhere in the app.
   *
   * The page is on v2 (the version just created), so the item is enabled:
   * the baseline's eligibility rule only refuses the version that already IS
   * the default, the "base" fallback while no default is recorded, and a
   * published version.
   */
  const setDefaultItem = page.getByTestId('agent-version-set-default');
  await expect(setDefaultItem).toBeVisible({ timeout: 10_000 });
  await expect(setDefaultItem).not.toHaveAttribute('aria-disabled', 'true');

  // The REQUEST is the assertion. An item that opens a dialog and sends
  // nothing would satisfy every DOM check above and change no default.
  const setDefaultStatuses: number[] = [];
  const onDefaultResponse = (response: import('@playwright/test').Response): void => {
    if (response.request().method() === 'PATCH' && response.url().includes('/elitea_core/default_version/')) {
      setDefaultStatuses.push(response.status());
    }
  };
  page.on('response', onDefaultResponse);

  await setDefaultItem.click();
  const setDefaultDialog = page.getByTestId('agent-version-set-default-dialog');
  await expect(setDefaultDialog).toBeVisible({ timeout: 5_000 });
  await expect(setDefaultDialog.getByTestId('agent-version-set-default-name')).toHaveText('v2');
  await setDefaultDialog.getByRole('button', { name: /set as a default/i }).click();

  // The LAST response, not the first, and not an exact count: the app's query
  // client retries once on a non-final error (app/providers/queryClient.ts:157),
  // so a transient 5xx legitimately produces two responses for one click.
  await expect(() => expect(setDefaultStatuses.length).toBeGreaterThanOrEqual(1)).toPass({ timeout: 15_000 });
  page.off('response', onDefaultResponse);
  expect(
    setDefaultStatuses.at(-1),
    `set-default PATCH must succeed, got ${setDefaultStatuses.join(',')}`,
  ).toBeLessThan(400);

  // A successful PATCH closes the dialog, and the menu then marks the version
  // it just pinned. Both are the app's own read-back: the server reports the
  // default on no documented response (see `AgentVersionControls`' disclosed
  // gap), so this is what a user actually sees.
  await expect(setDefaultDialog).toBeHidden({ timeout: 10_000 });
  await versionTrigger.click();
  await expect(page.getByTestId('agent-version-default-marker')).toBeVisible({ timeout: 5_000 });
  await expect(page.getByTestId('agent-version-set-default')).toHaveAttribute('aria-disabled', 'true');

  await page.keyboard.press('Escape');

  /*
   * JRNY-015 step 3 — DELETE OLD VERSION (#147).
   *
   * A THIRD version is created first, and that is not padding. Both other
   * versions are now undeletable by design: "base" is the agent's live
   * version, and v2 is the default this journey just pinned. The step the
   * journey names — delete the version that is no longer wanted — is only
   * reachable on a version that is neither, so the journey has to make one.
   */
  await page.getByRole('button', { name: /save as version/i }).click();
  const thirdDialog = page.getByRole('dialog');
  await expect(thirdDialog).toBeVisible({ timeout: 5_000 });
  await thirdDialog.getByRole('textbox').fill('v3');
  await thirdDialog.getByRole('button', { name: /^save$/i }).click();
  await page.waitForURL(/\/agents\/[^/]+\/[^/]+\/\d+/, { timeout: 15_000 });

  await versionTrigger.click();
  const deleteItem = page.getByTestId('agent-version-delete');
  await expect(deleteItem).toBeVisible({ timeout: 10_000 });
  // Enabled on v3, and the two rules that hold it back elsewhere are the
  // reason v3 exists.
  await expect(deleteItem).not.toHaveAttribute('aria-disabled', 'true');

  const deleteStatuses: number[] = [];
  const onDeleteResponse = (response: import('@playwright/test').Response): void => {
    if (response.request().method() === 'DELETE' && response.url().includes('/elitea_core/version/prompt_lib/')) {
      deleteStatuses.push(response.status());
    }
  };
  page.on('response', onDeleteResponse);

  await deleteItem.click();
  const deleteDialog = page.getByTestId('agent-version-delete-dialog');
  await expect(deleteDialog).toBeVisible({ timeout: 5_000 });

  // TYPE-TO-CONFIRM. Confirm is disabled until the version name matches, so
  // the request cannot be sent by a stray click on an open dialog.
  const deleteConfirm = deleteDialog.getByRole('button', { name: /^delete$/i });
  await expect(deleteConfirm).toBeDisabled();
  expect(deleteStatuses).toHaveLength(0);

  await deleteDialog.getByRole('textbox').fill('v3');
  await expect(deleteConfirm).toBeEnabled();
  await deleteConfirm.click();

  // The REQUEST is the assertion. A dialog that closed and sent nothing would
  // satisfy every DOM check above.
  await expect(() => expect(deleteStatuses.length).toBeGreaterThanOrEqual(1)).toPass({ timeout: 15_000 });
  page.off('response', onDeleteResponse);
  expect(
    deleteStatuses.at(-1),
    `version DELETE must succeed, got ${deleteStatuses.join(',')}`,
  ).toBeLessThan(400);

  // The escape: the URL pointed at the version that is now gone, so the page
  // navigates back to the agent's default version rather than 404ing.
  await expect(deleteDialog).toBeHidden({ timeout: 10_000 });
  await page.waitForURL(/\/agents\/[^/]+\/[^/]+$/, { timeout: 15_000 });

  // …and the menu stops offering it. This is the read-back, so a DELETE the
  // server accepted and the list still shows would fail here.
  await versionTrigger.click();
  await expect(page.getByRole('menuitem', { name: /v3/i })).toHaveCount(0);
  await expect(page.getByRole('menuitem', { name: /v2/i })).toBeVisible({ timeout: 10_000 });
  await page.keyboard.press('Escape');

  await checkA11y(page);
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 25: Unsaved-changes navigation block
// ─────────────────────────────────────────────────────────────────────────────
test('J25: unsaved-changes nav block: navigate away from dirty agent → dialog → cancel → stay', async ({
  page,
}) => {
  /*
   * Was EXPECTED-FAIL until #133. The guard was half built: `NavBlockerDialog`
   * held a real TanStack `useBlocker` and `AppShell` mounted it under every
   * page, but `setBlockNav` had only three production call sites — all in
   * `processes/chat/ui/ChatWithEditors.hooks.ts`, the CHAT-embedded editors —
   * so the standalone `/agents` pages never armed it and a typed edit was
   * discarded silently on any nav-link click. `pages/agents/
   * CreateApplication.tsx` and `EditApplication.tsx` now arm it from their own
   * dirty state via `widgets/app-shell`'s `useUnsavedChangesNavBlocker`.
   *
   * The dialog asserted on below is the REAL production `NavBlockerDialog`,
   * not a fixture: nothing in this file stubs or rewrites the served bundle.
   */
  await page.goto(BASE_URL + '/app/agents/my');
  await page.waitForURL('**/agents**', { timeout: 15_000 });

  await openCreateAgentForm(page);

  // Dirty the form.
  await page.getByTestId('agent-name-input').fill(`${AUTOTEST_PREFIX}dirty-agent`);

  // Navigate away via a real in-app link (no goto() fallback: a hard navigation
  // bypasses the router's blocker entirely, so falling back to one would test
  // nothing while still passing).
  await page.getByRole('link', { name: /chat/i }).first().click();

  // The blocker dialog must appear. It used to be wrapped in
  // `if (dialogVisible)`, so an app with NO nav blocker passed this journey.
  const navBlockerDialog = page.getByRole('dialog');
  await expect(navBlockerDialog).toBeVisible({ timeout: 10_000 });
  await checkA11y(page);

  // Cancelling must keep us on the agent form with the typed value intact —
  // that is the actual guarantee JRNY-025 is about.
  await navBlockerDialog.getByRole('button', { name: /cancel|stay|no/i }).first().click();
  await expect(page.getByTestId('agent-name-input')).toHaveValue(`${AUTOTEST_PREFIX}dirty-agent`);
  expect(page.url()).toContain('/agents');

  await checkA11y(page);
});

/*
 * #307 — the agent edit page persists what a user types.
 *
 * Was EXPECTED-FAIL before #307. The page rendered the full editing form and
 * saved nothing a user could change: `useEditApplicationEditorBridge` routed
 * only `name`/`description` into the form and dropped every other field on
 * the floor, while `useEditApplicationForm` submitted `conversation_starters`
 * alone — and then called `form.reset()`, so the Save button reported success
 * and the edit vanished.
 *
 * Deliberately asserted after a FULL RELOAD, not against the live form: every
 * field on this page keeps a local mirror of what the user typed
 * (`WelcomeMessageInput`'s `inputValue`, `useCreateAgentFormState`'s `name`),
 * so re-reading the input after clicking Save proves only that React kept the
 * string in memory — exactly the false green the old page would have produced.
 * Only a reload re-reads the server.
 *
 * NOT COVERED, and deliberately not smuggled in with an `if (visible)`:
 *  - `variables` — sent by the page, but the Go `UpdateVersion` handler has
 *    no `variables` branch (applications/handler.go:807-836), so it is a
 *    silent server-side no-op on update. Asserting it would fail for a
 *    reason this page cannot fix.
 *  - `conversation_starters` — persists, but has no input mounted anywhere:
 *    the baseline's `ConversationStarters` editor has no port in this app.
 */
test('J14: the agent edit page persists an edited name, description and welcome message across a reload', async ({
  page,
}) => {
  // The name input carries maxlength="32" (entities/application-form). The
  // edited value below appends "-ed", so the BASE name must leave room for it
  // or the browser silently truncates and the reload assertion compares a
  // 33-character expectation against the 32 characters the field could hold.
  // Measured: `autotest_persist-<9 digits>` is 25, plus "-ed" is 28.
  const name = `${AUTOTEST_PREFIX}persist-${Date.now() % 1e9}`;
  const { id } = await createAgent(page.request, name);
  try {
    await page.goto(BASE_URL + `/app/agents/latest/${id}`);

    const editPanel = page.getByTestId('edit-application-configuration-tab-panel');
    await expect(editPanel).toBeVisible({ timeout: 15_000 });
    const nameInput = editPanel.getByTestId('agent-name-input');
    // The whole form renders `disabled` while the detail fetch is in flight,
    // and the panel mounts before it settles — typing during that window is
    // dropped silently. A populated name is the page's own "loaded" signal.
    await expect(nameInput).toHaveValue(name, { timeout: 15_000 });

    const editedName = `${name}-ed`;
    const editedDescription = `${AUTOTEST_PREFIX}edited description`;
    const editedWelcome = `${AUTOTEST_PREFIX}welcome aboard`;

    await nameInput.fill(editedName);
    await editPanel.getByTestId('agent-description-input').fill(editedDescription);

    // `toBeVisible`, not `toBeInTheDocument`: this input lives in an
    // accordion, and a collapsed panel still has the node in the DOM.
    const welcomeInput = editPanel.getByTestId('agent-welcome-message-input');
    await expect(welcomeInput).toBeVisible();
    await welcomeInput.fill(editedWelcome);

    // Both PUTs must LAND before the reload. #307 made this page issue two —
    // the application PUT carrying name/description and the version PUT
    // carrying the welcome message — and reloading while either is in flight
    // discards it. Measured: without this wait the run is engine-dependent,
    // chromium winning the race and webkit losing it, so the reload read back
    // the ORIGINAL name and the journey failed for a reason that had nothing
    // to do with persistence.
    const saved: number[] = [];
    const onResponse = (response: import('@playwright/test').Response): void => {
      if (response.request().method() === 'PUT' && response.url().includes('/elitea_core/')) {
        saved.push(response.status());
      }
    };
    page.on('response', onResponse);

    await page.getByTestId('agent-save-button').click();

    await expect(() => expect(saved.length).toBeGreaterThanOrEqual(2)).toPass({ timeout: 15_000 });
    page.off('response', onResponse);
    expect(saved.every((status) => status < 400), `save PUTs must succeed, got ${saved.join(',')}`).toBe(true);

    // A successful save clears the dirty flag, so this reload must NOT be
    // intercepted by the unsaved-changes guard. If the page ever regresses to
    // "reports success without sending", this is where it shows up.
    await page.reload();

    await expect(editPanel).toBeVisible({ timeout: 15_000 });
    // Application-level fields: these travel on a DIFFERENT endpoint (the
    // application PUT) that this page did not call at all before #307.
    await expect(nameInput).toHaveValue(editedName, { timeout: 15_000 });
    await expect(editPanel.getByTestId('agent-description-input')).toHaveValue(editedDescription);
    // Version-level field: the version PUT, whose body previously carried
    // conversation starters and nothing else.
    await expect(welcomeInput).toHaveValue(editedWelcome);
  } finally {
    await deleteAgent(page.request, id);
  }
});

/*
 * The issue's literal repro (#133): "/agents/<id>", the EDIT page, not the
 * create form the journey above drives. Both pages had the same hole and both
 * are now armed, but only the create page was measured — this closes that.
 * The agent is minted through the API so the test is about the guard, not
 * about the create flow J14 already covers.
 */
test('J25: unsaved-changes nav block also guards the standalone agent EDIT page', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}edit-${Date.now() % 1e9}`;
  const { id } = await createAgent(page.request, name);
  try {
    await page.goto(BASE_URL + `/app/agents/latest/${id}`);

    // The real edit page, not the create form still mounted under the parent
    // route — this testid exists only on `EditApplication`.
    const editPanel = page.getByTestId('edit-application-configuration-tab-panel');
    await expect(editPanel).toBeVisible({ timeout: 15_000 });
    const nameInput = editPanel.getByTestId('agent-name-input');
    await expect(nameInput).toHaveValue(name, { timeout: 15_000 });

    const edited = `${name}-x`;
    await nameInput.fill(edited);

    await page.getByRole('link', { name: /chat/i }).first().click();

    const navBlockerDialog = page.getByRole('dialog');
    await expect(navBlockerDialog).toBeVisible({ timeout: 10_000 });

    await navBlockerDialog.getByRole('button', { name: /cancel|stay|no/i }).first().click();
    await expect(nameInput).toHaveValue(edited);
    expect(page.url()).toContain(`/agents/latest/${id}`);
  } finally {
    await deleteAgent(page.request, id);
  }
});
