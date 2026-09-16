/**
 * The pipeline configuration form's "Attachments" internal-tool switch
 * (`internal-tool-attachments`, `AgentInternalToolSwitch` rendered by
 * `ApplicationTools.tsx`'s `isPipeline`-narrowed MODULES grid) and its
 * `input_attachments` state-variable sync
 * (`usePipelineAttachmentYamlSync.hooks.ts`, wired from `ConfigurationTab.tsx`'s
 * `hasAttachments`).
 *
 * ELITEA-0890's own repro path opens this switch through a Chat-embedded
 * "conversation canvas editor" for a pipeline participant. That surface does
 * not exist in this build — `PlusChatButton`'s `onCreatePipeline` callback is
 * never supplied by any production caller (`grep -rn 'onCreatePipeline='
 * src/` — zero hits outside the component's own prop declaration/tests), so
 * a pipeline can never be created or opened as a chat participant at all; see
 * `not-applicable.md`. The switch and the sync hook it drives are the SAME
 * component/hook the standalone pipeline editor (`/app/pipelines/latest/{id}`)
 * mounts, so this file exercises the identical behaviour there instead.
 *
 * The other half of `AttachmentSwitch.tsx` (a differently-named, SEPARATE
 * component) really is dead code with no mount point anywhere, agents or
 * pipelines alike — not this case's concern, and not touched here.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';
import {
  createPipelineThroughApi,
  deletePipeline,
  parseStoredGraph,
  readStoredPipelineVersion,
  resolveLatestPipelineVersionId,
  type CreatedPipeline,
} from '../../fixtures/pipelines';

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline !== undefined) await deletePipeline(page.request, pipeline);
  }
});

async function openEditor(page: Page, pipeline: CreatedPipeline): Promise<void> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('agent-toolkits-section')).toBeVisible({ timeout: 15_000 });
}

async function saveAndAwaitPersist(page: Page): Promise<void> {
  const persisted = page.waitForResponse(
    (response) => response.request().method() === 'PUT' && response.url().includes('/version/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

async function openStateDrawer(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'State', exact: true }).click();
  // Plain text, NOT `getByText` — the starter template's LLM_1 node also
  // shows "messages" as an Output chip (a `role="button"` MUI Chip), and
  // `StateVariableItem` renders the state row's own name as a plain
  // `<Typography variant="body1">` (`role="paragraph"`), so only the role
  // distinguishes the drawer's own row from the node card behind it.
  await expect(page.getByRole('paragraph').filter({ hasText: /^messages$/ })).toBeVisible({ timeout: 10_000 });
}

/* onetest: ELITEA-0890 — enabling Attachments adds `input_attachments` to States; disabling it removes it again. Both reach the stored YAML, not only the drawer's own echo. */
test('toggling Attachments adds and removes the input_attachments state variable', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}attach-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  await openEditor(page, pipeline);

  await openStateDrawer(page);
  await expect(page.getByText('input_attachments', { exact: true })).toHaveCount(0);

  const attachmentsSwitch = page.getByTestId('internal-tool-attachments').getByRole('switch');
  await expect(attachmentsSwitch).not.toBeChecked();
  await attachmentsSwitch.click();
  await expect(attachmentsSwitch).toBeChecked();

  await expect(page.getByText('input_attachments', { exact: true })).toBeVisible({ timeout: 10_000 });

  await saveAndAwaitPersist(page);
  const versionIdOn = await resolveLatestPipelineVersionId(page.request, pipeline.projectId, pipeline.id);
  const storedOn = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, versionIdOn);
  expect(parseStoredGraph(storedOn.instructions).state, 'enabling Attachments must add input_attachments to the stored state').toHaveProperty(
    'input_attachments',
  );
  expect(storedOn.meta['internal_tools']).toContain('attachments');

  // Disable it again — the state key must come back out.
  await attachmentsSwitch.click();
  await expect(attachmentsSwitch).not.toBeChecked();
  await expect(page.getByText('input_attachments', { exact: true })).toHaveCount(0, { timeout: 10_000 });

  await saveAndAwaitPersist(page);
  const versionIdOff = await resolveLatestPipelineVersionId(page.request, pipeline.projectId, pipeline.id);
  const storedOff = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, versionIdOff);
  expect(
    parseStoredGraph(storedOff.instructions).state,
    'disabling Attachments must remove input_attachments from the stored state',
  ).not.toHaveProperty('input_attachments');
});
