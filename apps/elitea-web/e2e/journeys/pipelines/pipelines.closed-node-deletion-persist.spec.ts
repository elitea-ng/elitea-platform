/**
 * elitea_issues #5829 — "Pipeline node deletion fails to persist despite
 * success notification in conversation canvas mode": delete a node, Save,
 * see the success toast, reopen the pipeline — the deleted node was still
 * there (a false-positive save).
 *
 * That exact repro path (a chat-embedded "conversation canvas" pipeline
 * editor) does not exist in this app — see
 * `pipelines.attachments-config.spec.ts`'s doc comment: no production
 * caller ever supplies `PlusChatButton.onCreatePipeline`, so a pipeline is
 * never created or opened as a chat participant's own editor. The
 * STANDALONE pipeline editor (`/app/pipelines/latest/{id}`) is the one real
 * surface `NodeCardHeader.tsx`'s "Delete" menu item and the Save button both
 * live on, so this pins the same use case there: delete a node, Save, wait
 * for the success toast, reload, and read the STORED document (not the
 * screen) for the deleted node's id.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';
import {
  PIPELINE_STARTER_ENTRY_NODE_ID,
  createPipelineThroughApi,
  deletePipeline,
  parseStoredGraph,
  readStoredPipelineVersion,
  storedNodeIds,
  type CreatedPipeline,
} from '../../fixtures/pipelines';

/** The id `getInitialNodeId` mints for the first Printer node on this graph — same convention `pipelines.node-actions.spec.ts` relies on. */
const ADDED_NODE_ID = 'Printer_1';

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline === undefined) continue;
    await deletePipeline(page.request, pipeline);
  }
});

async function openCanvas(page: Page, pipeline: CreatedPipeline): Promise<void> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 20_000 });
  await page.getByRole('button', { name: 'Collapse the configuration panel' }).click();
  await expect(page.getByRole('button', { name: 'Expand the configuration panel' })).toBeVisible({ timeout: 10_000 });
  await page.getByRole('button', { name: 'Collapse the chat panel' }).click();
  await expect(page.getByRole('button', { name: 'Expand the chat panel' })).toBeVisible({ timeout: 10_000 });
}

async function saveAndAwaitPersist(page: Page): Promise<void> {
  const persisted = page.waitForResponse(
    response => response.request().method() === 'PUT' && response.url().includes('/version/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

/* elitea_issues: #5829 — a node deleted from the canvas and Saved must be absent from the STORED document, and stay absent across a reload. */
test('deleting a node and saving removes it from the stored pipeline, not just the screen', async ({ page }) => {
  test.slow();
  const name = `${AUTOTEST_PREFIX}del-persist-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);

  await openCanvas(page, pipeline);

  // Add a second node and save it in — the one this test will delete.
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: 'Printer', exact: true }).click();
  const addedNode = page.locator(`.react-flow__node[data-id="${ADDED_NODE_ID}"]`);
  await expect(addedNode).toBeVisible({ timeout: 20_000 });
  await saveAndAwaitPersist(page);

  const afterAdd = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, pipeline.versionId);
  expect(storedNodeIds(parseStoredGraph(afterAdd.instructions)), 'setup: the added node must be stored before this test deletes it').toContain(
    ADDED_NODE_ID,
  );

  // Delete it through its own "Delete" menu item — a confirm dialog gates it.
  // Fit View first: a node added at the viewport centre can land under the
  // (reopened, post-save) configuration panel otherwise (the same trap
  // `pipelines.node-actions.spec.ts`'s `fitAndSettle` exists for).
  await page.getByRole('button', { name: 'Fit View' }).click();
  await expect(addedNode.getByRole('button', { name: 'Node actions' })).toBeVisible({ timeout: 10_000 });
  await addedNode.getByRole('button', { name: 'Node actions' }).click();
  await page.getByRole('menuitem', { name: 'Delete' }).click();
  await expect(page.getByRole('heading', { name: 'Delete node?', exact: true })).toBeVisible({ timeout: 10_000 });
  await page.getByRole('button', { name: 'Remove' }).click();
  await expect(addedNode).toHaveCount(0);

  await saveAndAwaitPersist(page);

  // THE ASSERTION — the stored document, not the screen (the bug's own
  // "success notification but nothing persisted" trap).
  const afterDelete = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, pipeline.versionId);
  const graph = parseStoredGraph(afterDelete.instructions);
  expect(storedNodeIds(graph), 'the deleted node must be gone from the stored document, not merely the canvas').not.toContain(ADDED_NODE_ID);
  expect(storedNodeIds(graph)).toContain(PIPELINE_STARTER_ENTRY_NODE_ID);

  // And it stays gone across a full reload of the editor.
  await page.reload();
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  await expect(page.locator(`.react-flow__node[data-id="${ADDED_NODE_ID}"]`)).toHaveCount(0);
});
