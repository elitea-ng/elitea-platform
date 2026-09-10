/**
 * The pipeline configuration form's TAGS field (`AgentTagEditor`,
 * `src/features/agents/ui/AgentTagEditor.tsx`) — a multi-select MUI
 * `Autocomplete` (`multiple`, `freeSolo`) wired into the pipeline editor's
 * `tagsSlot` (`src/pages/pipelines/ui/EditPipelineConfigurationPanel.tsx`).
 *
 * `pipelines.configuration-form.spec.ts` proves name/description/welcome
 * message survive a save+reload; it does not touch tags at all. This file
 * closes that gap for the "add several tags in one save" family
 * (onetest ELITEA-0779/0781/0783): every assertion reads the STORED
 * `tagNames` (`e2e/fixtures/pipelines.ts::readStoredPipelineVersion`), not
 * the chips left on screen — the same "screen echo" trap this suite's other
 * pipeline files already guard against.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createTag, deleteAutotestTags, deleteTag, type StoredTag } from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline, readStoredPipelineVersion, resolveLatestPipelineVersionId, type CreatedPipeline } from '../../fixtures/pipelines';

const created: CreatedPipeline[] = [];
const createdTags: StoredTag[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline !== undefined) await deletePipeline(page.request, pipeline);
  }
  while (createdTags.length > 0) {
    const tag = createdTags.pop();
    if (tag !== undefined) await deleteTag(page.request, tag.id);
  }
  // Freeform-created ("system-new") tags leave a row with no explicit
  // cleanup handle above — sweep by name prefix instead.
  await deleteAutotestTags(page.request);
});

/** Open the pipeline editor and wait for the tags Autocomplete to be mounted. */
async function openConfigurationForm(page: Page, pipeline: CreatedPipeline): Promise<void> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  await expect(page.getByTestId('edit-pipeline-configuration-panel')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByRole('combobox', { name: 'Tags' })).toBeVisible({ timeout: 15_000 });
}

/** Type one tag name into the Tags Autocomplete and commit it as a chip. */
async function addTagChip(page: Page, name: string): Promise<void> {
  const input = page.getByRole('combobox', { name: 'Tags' });
  await input.click();
  await input.fill(name);
  await page.keyboard.press('Enter');
  await expect(page.getByRole('button', { name: new RegExp(`^${name}`) })).toBeVisible({ timeout: 10_000 });
}

/** Click Save and wait for the version PUT (tags travel on the version write) to land. */
async function saveAndAwaitPersist(page: Page): Promise<void> {
  const persisted = page.waitForResponse(
    (response) => response.request().method() === 'PUT' && response.url().includes('/version/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

/* onetest: ELITEA-0779 — 3 never-before-seen ("system-new") tags added in one save all persist after a hard reload. */
test('adding several brand-new tags in one save persists all of them', async ({ page }) => {
  const suffix = Date.now() % 1e9;
  const pipeline = await createPipelineThroughApi(page.request, `${AUTOTEST_PREFIX}tags-new-${suffix}`);
  created.push(pipeline);

  await openConfigurationForm(page, pipeline);

  const tagNames = [`${AUTOTEST_PREFIX}TagAlpha${suffix}`, `${AUTOTEST_PREFIX}TagBeta${suffix}`, `${AUTOTEST_PREFIX}TagGamma${suffix}`];
  for (const name of tagNames) await addTagChip(page, name);

  await saveAndAwaitPersist(page);

  const versionId = await resolveLatestPipelineVersionId(page.request, pipeline.projectId, pipeline.id);
  const stored = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, versionId);
  expect(new Set(stored.tagNames), 'all 3 system-new tags must reach the stored version row').toEqual(new Set(tagNames));

  // The reloaded editor must show the same 3 chips, not merely the server row.
  await openConfigurationForm(page, pipeline);
  for (const name of tagNames) {
    await expect(page.getByRole('button', { name: new RegExp(`^${name}`) })).toBeVisible({ timeout: 15_000 });
  }
});

/* onetest: ELITEA-0781 — a mixed save (system-existing tags + one brand-new tag) persists every tag, not only the new or only the existing ones. */
test('a mixed save of existing and brand-new tags persists all of them', async ({ page }) => {
  const suffix = Date.now() % 1e9;
  const existingA = await createTag(page.request, `${AUTOTEST_PREFIX}ExistingA${suffix}`);
  const existingB = await createTag(page.request, `${AUTOTEST_PREFIX}ExistingB${suffix}`);
  createdTags.push(existingA, existingB);

  const pipeline = await createPipelineThroughApi(page.request, `${AUTOTEST_PREFIX}tags-mixed-${suffix}`);
  created.push(pipeline);

  await openConfigurationForm(page, pipeline);

  const newTagName = `${AUTOTEST_PREFIX}TagMix${suffix}`;
  await addTagChip(page, existingA.name);
  await addTagChip(page, existingB.name);
  await addTagChip(page, newTagName);

  await saveAndAwaitPersist(page);

  const versionId = await resolveLatestPipelineVersionId(page.request, pipeline.projectId, pipeline.id);
  const stored = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, versionId);
  expect(new Set(stored.tagNames)).toEqual(new Set([existingA.name, existingB.name, newTagName]));
});

/* onetest: ELITEA-0783 — a pipeline that already carries 1 saved tag keeps it after 2 more system-new tags are added in the same save. */
test('a pipeline with one saved tag keeps it after adding two more in one save', async ({ page }) => {
  const suffix = Date.now() % 1e9;
  const savedTagName = `${AUTOTEST_PREFIX}Saved${suffix}`;
  const pipeline = await createPipelineThroughApi(page.request, `${AUTOTEST_PREFIX}tags-keep-${suffix}`);
  created.push(pipeline);

  // Seed the one pre-existing tag through the form itself (not the API): the
  // claim under test is "a save that ADDS tags does not drop the one already
  // there", which only means something if that first tag went through the
  // exact same write path the second save will use.
  await openConfigurationForm(page, pipeline);
  await addTagChip(page, savedTagName);
  await saveAndAwaitPersist(page);

  await openConfigurationForm(page, pipeline);
  await expect(page.getByRole('button', { name: new RegExp(`^${savedTagName}`) })).toBeVisible({ timeout: 15_000 });

  const newTagNames = [`${AUTOTEST_PREFIX}New1_${suffix}`, `${AUTOTEST_PREFIX}New2_${suffix}`];
  for (const name of newTagNames) await addTagChip(page, name);
  await saveAndAwaitPersist(page);

  const versionId = await resolveLatestPipelineVersionId(page.request, pipeline.projectId, pipeline.id);
  const stored = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, versionId);
  expect(new Set(stored.tagNames)).toEqual(new Set([savedTagName, ...newTagNames]));
});
