/**
 * The Create-vs-Edit split for a pipeline: `pages/pipelines/CreatePipeline.tsx`
 * (`/app/pipelines/create`) and `pages/pipelines/EditPipeline.tsx`
 * (`/app/pipelines/latest/{id}`) are two SEPARATE route components, not one
 * form that toggles sections on and off. `CreatePipeline.tsx` renders only
 * `CreateAgentForm` (name/description/tags-omitted/welcome
 * message/conversation starters/step limit) — it mounts no Flow/YAML editor
 * and no `EditPipelineToolsPanel` at all, so "hidden before the first save"
 * is true by construction, not by a feature flag that could regress
 * independently of the route split itself.
 *
 * `pipelines.lifecycle.spec.ts`'s own create journey already proves the Flow
 * editor renders immediately after that first save; this file adds what it
 * does not check: the create form's OWN absence of Flow/YAML/Toolkit, the
 * YAML tab and the Toolkit add buttons together on the post-save page, and
 * that a pipeline opened COLD (already saved before this test ever touched
 * it) needs no second save to show any of it.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, clickCreateButton } from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline, type CreatedPipeline } from '../../fixtures/pipelines';

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline !== undefined) await deletePipeline(page.request, pipeline);
  }
});

async function assertEditorSectionsActive(page: Page): Promise<void> {
  await expect(page.getByRole('button', { name: 'Flow', exact: true })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole('button', { name: 'Yaml', exact: true })).toBeVisible();
  const toolkitsSection = page.getByTestId('agent-toolkits-section');
  await expect(toolkitsSection).toBeVisible({ timeout: 15_000 });
  await expect(toolkitsSection.getByRole('button', { name: 'Toolkit', exact: true })).toBeEnabled();
  await expect(toolkitsSection.getByRole('button', { name: 'MCP', exact: true })).toBeEnabled();
  await expect(toolkitsSection.getByRole('button', { name: 'Agent', exact: true })).toBeEnabled();
  await expect(toolkitsSection.getByRole('button', { name: 'Pipeline', exact: true })).toBeEnabled();
}

/* onetest: ELITEA-0913, ELITEA-0914, ELITEA-0916, ELITEA-0929 — before the first save the create form shows no Flow/YAML tab and no Toolkit section at all; a single save transitions straight to all three active at once. */
test('the create form has no Flow/YAML/Toolkit section — one save activates all three together', async ({ page }) => {
  await page.goto(BASE_URL + '/app/pipelines/my');
  await page.waitForURL('**/pipelines**', { timeout: 15_000 });
  await clickCreateButton(page);
  await page.waitForURL('**/app/pipelines/create**', { timeout: 15_000 });

  const panel = page.getByTestId('create-pipeline-form-panel');
  await expect(panel.getByTestId('agent-name-input')).toBeVisible({ timeout: 10_000 });

  // Pre-save: none of the three exist on the page at all.
  await expect(page.getByRole('button', { name: 'Flow', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Yaml', exact: true })).toHaveCount(0);
  await expect(page.getByTestId('agent-toolkits-section')).toHaveCount(0);

  const name = `${AUTOTEST_PREFIX}cux-${Date.now() % 1e9}`;
  await panel.getByTestId('agent-name-input').fill(name);
  await panel.getByTestId('agent-description-input').fill(`${AUTOTEST_PREFIX}creation-ux description`);
  await page.getByTestId('pipeline-save-button').click();

  await page.waitForURL(/\/app\/pipelines\/latest\/\d+/, { timeout: 20_000 });
  const id = /\/app\/pipelines\/latest\/(\d+)/.exec(page.url())?.[1];
  expect(id).toBeTruthy();
  created.push({ id: id as string, versionId: '', projectId: '1' });

  // Post-save, in the same single transition: all three active.
  await assertEditorSectionsActive(page);
});

/* onetest: ELITEA-0917 — an already-saved pipeline opened cold shows Flow/YAML/Toolkit immediately, with no second save needed; a minor edit still saves cleanly afterwards. */
test('a cold-opened, already-saved pipeline shows every section immediately', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}cux-cold-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);

  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  await assertEditorSectionsActive(page);

  // A minor edit still saves without anything becoming inactive afterwards.
  await page.getByTestId('agent-description-input').fill(`${AUTOTEST_PREFIX}edited description`);
  const persisted = page.waitForResponse(
    (response) => response.request().method() === 'PUT' && response.url().includes('/application/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
  await assertEditorSectionsActive(page);
});
