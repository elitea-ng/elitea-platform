/**
 * Onetest wave-1 tail (T1b, folder `pipelines`) — the three node-card
 * mechanisms ELITEA-0853/0863 name that no existing journey drives:
 *
 *  - a double-click on a node's own name label opens an inline rename
 *    (`NodeCardHeader.tsx`'s `onDoubleClickName`/`onBlur`), which rewrites
 *    every reference to the node (`renameYamlDocument`/`renameFlowNode`/
 *    `renameFlowEdge`) — `pipelines.node-actions.spec.ts` only drives this
 *    card's OTHER menu ("Make entrypoint"); nothing here clicks the name;
 *  - two nodes of the SAME type mint sequential ids (`getNormalInitialNodeId`,
 *    `LLM_1`, `LLM_2`, …) — `pipelines.lifecycle.spec.ts`'s own minting test
 *    adds exactly ONE of each of the nine admitted types, so it can never
 *    observe a SECOND node of the same type;
 *  - deleting a node through its own three-dot menu ("Delete") removes it
 *    from the canvas and the stored document while `END` and the rest of
 *    the graph survive — `pipelines.node-actions.spec.ts` only exercises
 *    "Make entrypoint" on that same menu, never "Delete".
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
  resolveLatestPipelineVersionId,
  storedNodeIds,
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
  await expect(page.locator(`.react-flow__node[data-id="${PIPELINE_STARTER_ENTRY_NODE_ID}"]`)).toBeVisible({
    timeout: 20_000,
  });
}

async function saveAndAwaitPersist(page: Page): Promise<void> {
  const persisted = page.waitForResponse(
    (response) =>
      response.request().method() === 'PUT' && response.url().includes('/version/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

async function readGraph(page: Page, pipeline: CreatedPipeline) {
  const versionId = await resolveLatestPipelineVersionId(page.request, pipeline.projectId, pipeline.id);
  const stored = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, versionId);
  return parseStoredGraph(stored.instructions);
}

/* onetest: ELITEA-0853 (naming half) — a second node of the same type mints the next sequential id (`LLM_1`, then `LLM_2`), not a collision and not a repeat of the first. */
test('a second node of the same type is named sequentially after the first', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}seq-name-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  await openEditor(page, pipeline);

  // The starter already ships `LLM_1` — the second LLM node added through
  // the menu must mint `LLM_2`, not collide with or repeat the first.
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: 'LLM', exact: true }).click();
  const second = page.locator('.react-flow__node[data-id="LLM_2"]');
  await expect(second, 'the second LLM node must mint LLM_2, sequentially after LLM_1').toBeVisible({ timeout: 15_000 });
  await expect(page.locator(`.react-flow__node[data-id="${PIPELINE_STARTER_ENTRY_NODE_ID}"]`)).toBeVisible();
});

/* onetest: ELITEA-0853 (rename half) — double-clicking a node's name label opens an inline edit; the new name reaches the STORED document (the node's id is renamed everywhere it is referenced), not only the canvas echo. */
test('double-clicking a node name renames it, and the rename reaches the stored document', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}rename-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  await openEditor(page, pipeline);

  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: 'Printer', exact: true }).click();
  const added = page.locator('.react-flow__node[data-id="Printer_1"]');
  await expect(added).toBeVisible({ timeout: 15_000 });

  const nameLabel = added.getByText('Printer_1', { exact: true });
  await nameLabel.dblclick();
  const nameInput = added.getByRole('textbox').first();
  await expect(nameInput).toBeVisible({ timeout: 5_000 });
  await nameInput.fill('MyCustomPrinter');
  await nameInput.blur();

  const renamed = page.locator('.react-flow__node[data-id="MyCustomPrinter"]');
  await expect(renamed, 'the canvas must show the new id after the rename commits').toBeVisible({ timeout: 5_000 });
  await expect(added).toHaveCount(0);

  await saveAndAwaitPersist(page);
  const graph = await readGraph(page, pipeline);
  const ids = storedNodeIds(graph);
  expect(ids, 'the OLD id must not survive the rename in the stored document').not.toContain('Printer_1');
  expect(ids, 'the NEW id must reach the stored document, not just the canvas').toContain('MyCustomPrinter');
  expect(ids).toContain(PIPELINE_STARTER_ENTRY_NODE_ID);
});

/* onetest: ELITEA-0853, ELITEA-0863 (delete half) — deleting a node from its own three-dot menu removes it from the canvas and the stored document; END and the rest of the graph survive. */
test('deleting a node from its own menu removes it from the canvas and the stored document; END survives', async ({
  page,
}) => {
  const name = `${AUTOTEST_PREFIX}del-node-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  await openEditor(page, pipeline);

  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: 'Printer', exact: true }).click();
  const added = page.locator('.react-flow__node[data-id="Printer_1"]');
  await expect(added).toBeVisible({ timeout: 15_000 });

  await added.getByRole('button', { name: 'Node actions' }).click();
  await page.getByRole('menuitem', { name: 'Delete' }).click();
  // `useDeleteItems` gates every node delete behind a confirm dialog
  // ("Delete node?" / "Remove") — the menu click alone only opens it.
  await page.getByRole('button', { name: 'Remove' }).click();

  await expect(added).toHaveCount(0);
  await expect(page.locator('.react-flow__node[data-id="END"]'), 'END must survive deleting an unrelated node').toBeVisible();
  await expect(page.locator(`.react-flow__node[data-id="${PIPELINE_STARTER_ENTRY_NODE_ID}"]`)).toBeVisible();

  await saveAndAwaitPersist(page);
  const graph = await readGraph(page, pipeline);
  const ids = storedNodeIds(graph);
  expect(ids, 'the deleted node must not reach the stored document').not.toContain('Printer_1');
  expect(ids).toContain(PIPELINE_STARTER_ENTRY_NODE_ID);
});
