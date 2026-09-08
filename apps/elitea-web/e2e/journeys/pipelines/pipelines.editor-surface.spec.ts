/**
 * Journey 16e: the pipeline EDITOR's own chrome — the two panels, the `⋮`
 * menu, the create form's refusing states, and Discard.
 *
 * Ported BY USE CASE from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/pipelines/
 * test_pipeline_management.py` and `test_pipeline_advanced.py`); each test
 * names the legacy test it answers. What the existing pipelines journeys
 * cover is the GRAPH (`pipelines.graph-authoring.spec.ts`), the VERSIONS
 * (`pipelines.versioning.spec.ts`), the configuration FORM's round trip
 * (`pipelines.configuration-form.spec.ts`) and the save/admission gate
 * (`pipelines.validation.spec.ts`). The controls AROUND them had none.
 *
 * ── Two legacy use cases in this file's area are NOT ported ──────────────
 *
 *  - `TestActionsMenu::test_export_pipeline_if_available` — there is no
 *    Export control on a pipeline. `pages/pipelines/ui/EditPipelineActions.tsx`
 *    discloses it ("export/delete have no pipeline-side mount point yet"),
 *    and the menu asserted below is the evidence: it holds Share and Fork and
 *    nothing else. The legacy test self-skips when the item is absent, which
 *    is precisely the shape that reports green against an app that has
 *    nothing; the menu's real contents are asserted instead.
 *  - `TestMultiNodeTopology::test_three_node_chain` — it builds LLM → Code →
 *    END, and `Code` is not a node type this platform admits: the Rust
 *    compiler has no `parse_pipeline_node` arm for it
 *    (`services/elitea-worker-rust/src/agents/graph/compiler.rs:1236`), so
 *    the Add-node menu withholds it and `pipelines.lifecycle.spec.ts` asserts
 *    that withholding. Porting the test would mean authoring a pipeline that
 *    cannot load. The claim underneath it — a chain of connections reaching
 *    the stored document — is `pipelines.graph-authoring.spec.ts`'s, which
 *    drags three different connection shapes on the real canvas and reads
 *    each one back out of the stored YAML.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, clickCreateButton } from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline, type CreatedPipeline } from '../../fixtures/pipelines';

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline === undefined) continue;
    await deletePipeline(page.request, pipeline);
  }
});

/** A name unique per run, inside the create form's own 32-character `maxLength` on the same column. */
function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
}

/**
 * Open a pipeline's editor and wait for it to be POPULATED, not merely
 * mounted.
 *
 * The whole form renders disabled while the detail fetch is in flight and is
 * re-seeded when it lands, so a value typed into the window between the two
 * is discarded — the same measurement `agents.lifecycle.spec.ts`'s J14
 * records for the agent editor. A non-empty name is the page's own "loaded"
 * signal.
 */
async function openPipelineEditor(page: Page, pipeline: CreatedPipeline): Promise<void> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  await expect(page.getByTestId('edit-pipeline-configuration-panel')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('agent-name-input')).not.toHaveValue('', { timeout: 30_000 });
}

/*
 * Legacy: `TestEditPipeline::test_pipeline_has_configuration_and_history_tabs`
 * — "Release 2.0.1: tab-based navigation was replaced with an always-visible
 * left configuration panel and a 'view run history' icon button."
 *
 * Both controls, in one screen, named by what they are rather than by a tab
 * index. The configuration panel is `GeneralFormPanel`'s `pipeline-config-tab`
 * and the history control is `shared/ui/ViewRunHistoryButton`'s
 * `pipeline-history-tab` — the two testids that survived the redesign the
 * legacy docstring describes.
 *
 * DISCLOSED, because a reader will reasonably ask why this does not CLICK the
 * history button: `ConfigurationTab.tsx` renders the history view from a
 * `renderRunHistory` slot, and `pages/pipelines/EditPipeline.tsx` fills every
 * other slot but not that one (`buildPipelineConfigurationTabSlots` names
 * `renderConfigurationForm`, `renderChat`, `renderClearChatButton` and
 * `renderContextBudget` only). `ConfigurationTab`'s own guard is
 * `if (showHistory && slots.renderRunHistory)`, so a click today sets a flag
 * and falls through to the same two-panel layout — there is no
 * `entities/run-history` in this app to render. Asserting a click would
 * either assert nothing or assert the fall-through. The legacy test asserts
 * visibility, and visibility is what is asserted here too.
 */
test('J16e: the pipeline editor shows its configuration panel and its run-history control', async ({ page }) => {
  const pipeline = await createPipelineThroughApi(page.request, uniqueName('panels'));
  created.push(pipeline);
  await openPipelineEditor(page, pipeline);

  await expect(page.getByTestId('pipeline-config-tab'), 'the always-visible configuration panel').toBeVisible({
    timeout: 20_000,
  });
  await expect(page.getByTestId('pipeline-history-tab'), 'the "view run history" control').toBeVisible();
  // Named, not just present: the button is icon-only, so its `aria-label` is
  // the only thing that tells a keyboard or screen-reader user what it does —
  // and an icon-only button with no name is the exact WCAG 4.1.2 violation
  // #135 had to fix on this screen's two collapse toggles.
  await expect(page.getByTestId('pipeline-history-tab')).toHaveAccessibleName('View run history');
});

/*
 * Legacy: `TestActionsMenu::test_three_dot_menu_lists_items` — "the menu holds
 * at least two items, including Delete and Export".
 *
 * Ported as the menu's REAL contract, because this platform's menu is a
 * different menu. `features/agent-lifecycle/ui/EntityLifecycleMenu.tsx` states
 * the rule it is built on: production's menu holds nine items, four of them
 * already exist in this app as their own toolbar buttons, and this menu
 * carries the ones that were missing rather than a second path to the same
 * write. For a PIPELINE that is Share (version), Fork and Share (entity) —
 * publish is omitted because the server refuses a pipeline outright with 400
 * `pipeline_not_publishable`.
 *
 * The count is exact and the absences are named. "At least two items" would
 * pass against a menu that had grown a second Delete, and the two publish
 * items reappearing here is a real regression this asserts against.
 */
test('J16e: the pipeline editor’s ⋮ menu offers exactly the lifecycle actions a pipeline has', async ({ page }) => {
  const pipeline = await createPipelineThroughApi(page.request, uniqueName('menu'));
  created.push(pipeline);
  await openPipelineEditor(page, pipeline);

  const trigger = page.getByTestId('pipeline-lifecycle-menu-button');
  await expect(trigger, 'the editor must offer its lifecycle menu').toBeEnabled({ timeout: 20_000 });
  await trigger.click();

  const menu = page.getByRole('menu', { name: 'Lifecycle actions' });
  await expect(menu).toBeVisible({ timeout: 10_000 });
  await expect(menu.getByTestId('pipeline-share-version-menuitem')).toBeVisible();
  await expect(menu.getByTestId('pipeline-fork-menuitem')).toBeVisible();
  await expect(menu.getByTestId('pipeline-share-entity-menuitem')).toBeVisible();
  await expect(menu.getByRole('menuitem'), 'the menu is exactly those three items').toHaveCount(3);

  // Publish and Unpublish are withheld for a pipeline on purpose — the route
  // answers 400 `pipeline_not_publishable`, so offering them would be a
  // control whose only outcome is a refusal.
  await expect(menu.getByTestId('pipeline-publish-menuitem')).toHaveCount(0);
  await expect(menu.getByTestId('pipeline-unpublish-menuitem')).toHaveCount(0);
});

/*
 * Legacy: `TestCreatePipeline::test_create_pipeline_required_fields_validation`
 * — "Save is disabled with empty fields, and still disabled with a name but no
 * description".
 *
 * J16 (`pipelines.lifecycle.spec.ts`) fills BOTH fields and then clears the
 * name, so it proves the name half only. This proves the states the legacy
 * test names — the untouched form and the name-only form — and the
 * description half, with the sentence the form shows for it.
 */
test('J16e: the pipeline create form refuses to save until both required fields are filled', async ({ page }) => {
  await page.goto(`${BASE_URL}/app/pipelines/my`);
  await page.waitForURL('**/pipelines**', { timeout: 15_000 });
  await clickCreateButton(page);
  await page.waitForURL('**/app/pipelines/create**', { timeout: 15_000 });

  const panel = page.getByTestId('create-pipeline-form-panel');
  const nameInput = panel.getByTestId('agent-name-input');
  const descriptionInput = panel.getByTestId('agent-description-input');
  const saveButton = page.getByTestId('pipeline-save-button');
  await expect(nameInput).toBeVisible({ timeout: 20_000 });

  // 1. The untouched form.
  await expect(saveButton, 'an untouched create form must not be saveable').toBeDisabled();

  // 2. A name and nothing else. `validation.ts` requires a non-blank
  //    description too, so this is still refused.
  await nameInput.fill(uniqueName('req'));
  await expect(saveButton, 'a name alone must not be enough — description is required').toBeDisabled();

  // 3. The reason, in the form's own words. Reached by filling and clearing
  //    rather than by leaving the field untouched: react-hook-form publishes
  //    a field error once the field has been interacted with, so an assertion
  //    on the pristine form would be asserting the resolver's silence.
  await descriptionInput.fill('temporary');
  await descriptionInput.fill('');
  await expect(panel.getByText('Description is required')).toBeVisible({ timeout: 10_000 });
  await expect(saveButton).toBeDisabled();

  // 4. Both filled — the gate opens. Without this the three assertions above
  //    would also hold for a button that is disabled unconditionally.
  await descriptionInput.fill('Created by the legacy-suite port');
  await expect(saveButton).toBeEnabled({ timeout: 10_000 });

  // Nothing was saved: this journey never clicks Save, so it creates no row
  // and needs no cleanup. Asserted so a future edit that adds a click cannot
  // leave one behind silently.
  expect(page.url()).toContain('/app/pipelines/create');
});

/*
 * Legacy: `TestDiscardChanges::test_discard_reverts_name_change` — "edit the
 * name, click Discard, and the original name is restored".
 *
 * The USE CASE is "an unsaved rename does not survive Discard". The BEHAVIOUR
 * differs from the legacy app's and the difference is deliberate:
 * `EditPipeline.tsx`'s `handleDiscarded` resets the form, drops the
 * flow-editor draft and the model pick, disarms the unsaved-changes blocker
 * and LEAVES for the list. That file records why — Discard used to be
 * `form.reset()` alone while the save path kept reading the live graph
 * through `usePipelineGraphDraft()`, so a later Save silently persisted the
 * discarded canvas edits and the user was stranded on the edit page.
 *
 * So the assertion is not "the field reverts on the page it was typed on" —
 * that page is gone by then — it is the claim underneath it, checked at the
 * two places that can carry it: the SERVER never saw the rename, and
 * re-opening the editor shows the original.
 *
 * J25 (`pipelines.lifecycle.spec.ts`) covers the neighbouring control, the
 * navigation blocker; nothing covered the Discard button itself.
 */
test('J16e: Discard drops an unsaved rename — the server never sees it and the editor reopens on the original', async ({
  page,
}) => {
  const name = uniqueName('discard');
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  await openPipelineEditor(page, pipeline);

  const nameInput = page.getByTestId('agent-name-input');
  await expect(nameInput).toHaveValue(name);
  await nameInput.fill(`${AUTOTEST_PREFIX}discarded`);
  await expect(nameInput).toHaveValue(`${AUTOTEST_PREFIX}discarded`);

  // The editor's Discard control is labelled "Cancel"
  // (`CreateApplicationTabBar`), and it confirms through a warning modal
  // before it fires — clicking it alone changes nothing.
  await page.getByRole('button', { name: 'Cancel', exact: true }).click();
  const confirm = page.getByRole('dialog');
  await expect(confirm).toBeVisible({ timeout: 10_000 });
  await confirm.getByRole('button', { name: 'Discard', exact: true }).click();

  // Discard leaves the editor for the list it came from, and it must do that
  // WITHOUT the unsaved-changes dialog: the blocker is disarmed as part of
  // the discard, and a second prompt here would be the regression #133's own
  // disarm contract exists to prevent.
  await page.waitForURL(/\/app\/pipelines\/[^/]+$/, { timeout: 20_000 });
  await expect(page.getByRole('dialog')).toHaveCount(0);

  // 1. The server never saw it. This is the half the screen cannot give: a
  //    page that reverted its input while the PUT had already landed would
  //    look identical.
  const stored = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${pipeline.projectId}/${pipeline.id}`,
  );
  expect(stored.ok(), `the pipeline must still be readable: ${(await stored.text()).slice(0, 300)}`).toBe(true);
  const storedApplication = (await stored.json()) as { name?: unknown };
  expect(storedApplication.name, 'the discarded rename must not have reached the applications row').toBe(name);

  // 2. …and the editor reopens on the original value, which is the claim the
  //    legacy test made on the page it stayed on.
  await openPipelineEditor(page, pipeline);
  await expect(page.getByTestId('agent-name-input')).toHaveValue(name);
});
