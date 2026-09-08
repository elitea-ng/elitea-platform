/**
 * The pipeline editor's CONFIGURATION FORM — the left pane of
 * `/app/pipelines/{tab}/{id}`.
 *
 * ── The gap this closes ──────────────────────────────────────────────────
 *
 * Until this unit the pane rendered a notice: "Configuration form is not
 * available yet." A pipeline's name, description, tags, welcome message and
 * chat starters could be set once, on the create form, and never edited
 * again. The form is real now, and this journey is the proof that its fields
 * survive a round trip through the backend rather than through the page's own
 * component state.
 *
 * ── Why every assertion is a RELOAD, not a screen read ───────────────────
 *
 * The form keeps its own copies of every field (`useEditPipelineVersionFields`
 * for the version-level ones, react-hook-form for name/description). Reading
 * the input back after Save would prove only that the page kept what the user
 * typed — which it did before this change too, all the way up to the point
 * where Save discarded it. So each field below is asserted TWICE and neither
 * time off the pre-save screen: once against what the server stored (the
 * version GET and the application GET, read through `page.request` so the
 * browser's cookies apply), and once against the freshly reloaded editor.
 *
 * ── Why name/description need the application read ───────────────────────
 *
 * They are APPLICATION-level columns, written only by
 * `PUT /elitea_core/application/prompt_lib/{projectId}/{applicationId}` — a
 * SECOND call the version PUT knows nothing about. The page used to issue the
 * version PUT alone, so a rename answered 200 and vanished. Reading the
 * application row back is the only assertion that can tell those two apart.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';
import { readStoredPipelineVersion } from '../../fixtures/pipelines';

/** The create POST carries the project id; nothing here assumes project 1 (the chat persona owns its own — #290). */
const APPLICATIONS_RE = /\/elitea_core\/applications\/prompt_lib\/(\d+)/;

interface CreatedPipeline {
  readonly projectId: string;
  readonly pipelineId: string;
  readonly versionId: string;
}

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline === undefined) continue;
    await page.request.delete(
      `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${pipeline.projectId}/${pipeline.pipelineId}`,
    );
  }
});

/** Create a pipeline through its own form and read the ids off the RESPONSE — same shape `pipelines.graph-authoring.spec.ts` establishes. */
async function createPipeline(page: Page, name: string): Promise<CreatedPipeline> {
  const response = page.waitForResponse(
    request => APPLICATIONS_RE.test(new URL(request.url()).pathname) && request.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await page.goto(`${BASE_URL}/app/pipelines/create`);
  await expect(page.getByTestId('agent-name-input')).toBeVisible({ timeout: 30_000 });
  await page.getByTestId('agent-name-input').fill(name);
  await page.getByTestId('agent-description-input').fill(`${name} description`);
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('pipeline-save-button').click();

  const resolved = await response;
  expect(resolved.status(), `the pipeline must be created: ${(await resolved.text()).slice(0, 300)}`).toBe(201);
  const body = (await resolved.json()) as { id?: string; version_details?: { id?: string } };
  const pipeline: CreatedPipeline = {
    projectId: APPLICATIONS_RE.exec(new URL(resolved.url()).pathname)?.[1] ?? '',
    pipelineId: String(body.id ?? ''),
    versionId: String(body.version_details?.id ?? ''),
  };
  expect(pipeline.projectId, 'the pipeline must belong to a project').not.toBe('');
  expect(pipeline.pipelineId, 'the created pipeline must carry an id').toMatch(/^\d+$/);
  expect(pipeline.versionId, 'the created pipeline must carry a version').not.toBe('');
  created.push(pipeline);
  return pipeline;
}

/** The application row itself — where `name`/`description` live. */
async function readStoredApplication(page: Page, pipeline: CreatedPipeline): Promise<{ name: string; description: string }> {
  const url = `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${pipeline.projectId}/${pipeline.pipelineId}`;
  const resp = await page.request.get(url);
  expect(resp.ok(), `GET ${url} -> ${resp.status()} ${(await resp.text()).slice(0, 300)}`).toBe(true);
  const body = (await resp.json()) as { name?: unknown; description?: unknown };
  return {
    name: typeof body.name === 'string' ? body.name : '',
    description: typeof body.description === 'string' ? body.description : '',
  };
}

/** Open the editor and wait for the configuration form to be on screen and populated. */
async function openConfigurationForm(page: Page, pipeline: CreatedPipeline): Promise<void> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.pipelineId}`);
  await expect(page.getByTestId('edit-pipeline-configuration-panel')).toBeVisible({ timeout: 30_000 });
  // Populated, not merely mounted: the detail fetch resolves after the first
  // render, and typing into a field the page is about to re-seed loses the
  // edit for a reason that has nothing to do with saving.
  await expect(page.getByTestId('agent-name-input')).not.toHaveValue('', { timeout: 30_000 });
}

test.describe('pipeline configuration form', () => {
  test('every field the form owns survives a save and a reload', async ({ page }) => {
    const name = `${AUTOTEST_PREFIX}cfg-form`;
    const pipeline = await createPipeline(page, name);
    await openConfigurationForm(page, pipeline);

    const renamed = `${name}-renamed`;
    await page.getByTestId('agent-name-input').fill(renamed);
    await page.getByTestId('agent-description-input').fill('Edited from the configuration form');
    await page.getByTestId('agent-welcome-message-input').fill('Welcome to this pipeline');

    const versionPut = page.waitForResponse(
      response =>
        /\/elitea_core\/version\/prompt_lib\/\d+\/\d+\/\d+/.test(new URL(response.url()).pathname) &&
        response.request().method() === 'PUT',
      { timeout: 30_000 },
    );
    const applicationPut = page.waitForResponse(
      response =>
        /\/elitea_core\/application\/prompt_lib\/\d+\/\d+$/.test(new URL(response.url()).pathname) &&
        response.request().method() === 'PUT',
      { timeout: 30_000 },
    );
    await page.getByTestId('pipeline-save-button').click();

    // BOTH calls, not one. The version PUT alone was the whole defect.
    expect((await versionPut).status(), 'the version PUT must succeed').toBeLessThan(300);
    expect((await applicationPut).status(), 'the application PUT must fire and succeed').toBeLessThan(300);

    // 1. What the SERVER stored.
    const application = await readStoredApplication(page, pipeline);
    expect(application.name, 'a rename must reach the applications row').toBe(renamed);
    expect(application.description).toBe('Edited from the configuration form');

    const version = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.pipelineId, pipeline.versionId);
    expect(version.welcomeMessage, 'the welcome message must reach application_versions').toBe('Welcome to this pipeline');
    // The executor is pinned on every write: an empty `agent_type` is
    // substituted with the literal `"openai"` by `insertVersion`.
    expect(version.agentType).toBe('pipeline');

    // 2. What the RELOADED editor shows — the page's own state is gone by now.
    await openConfigurationForm(page, pipeline);
    await expect(page.getByTestId('agent-name-input')).toHaveValue(renamed);
    await expect(page.getByTestId('agent-description-input')).toHaveValue('Edited from the configuration form');
    await expect(page.getByTestId('agent-welcome-message-input')).toHaveValue('Welcome to this pipeline');
  });

  /**
   * The Information panel is read-only and needs no save — but it was one of
   * the components this repository had built, unit-tested and mounted
   * NOWHERE, so "it renders on the real route" is the only claim that has
   * ever been checked.
   */
  test('the Information panel shows the ids an external caller needs', async ({ page }) => {
    const pipeline = await createPipeline(page, `${AUTOTEST_PREFIX}cfg-info`);
    await openConfigurationForm(page, pipeline);

    const information = page.getByTestId('agent-information-section');
    await expect(information).toBeVisible({ timeout: 30_000 });
    await expect(information).toContainText(pipeline.pipelineId);
    await expect(information).toContainText(pipeline.versionId);
  });
});
