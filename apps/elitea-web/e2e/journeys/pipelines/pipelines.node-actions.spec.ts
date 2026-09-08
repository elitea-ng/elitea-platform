/**
 * Journey 16g: a NODE's own three-dot menu — "Make entrypoint".
 *
 * Ported BY USE CASE from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/pipelines/
 * test_pipeline_advanced.py::TestMakeEntrypoint::test_make_node_entrypoint`).
 *
 * ── Why this one is worth a journey of its own ───────────────────────────
 *
 * `entry_point` is the single most load-bearing string in a pipeline
 * document. The compiler runs it through the same `valid_graph_id` check as
 * every node id and then refuses the whole graph if it names no node
 * (`services/elitea-worker-rust/src/agents/graph/compiler.rs:464`,
 * `document.entry-point` on the client side). Every existing pipelines
 * journey takes it as given — the editor sets it to the first node added and
 * nothing else ever moves it. This is the ONE control that moves it, and
 * `handleMakeEntrypoint` (`features/pipelines/ui/nodes/BaseNode/
 * NodeCardHeader.tsx:196`) is a one-line `setYamlJsonObject` with, until now,
 * no journey reaching it.
 *
 * ── Why the assertion is a stored read ───────────────────────────────────
 *
 * The card paints an entrypoint badge from `yamlJsonObject.entry_point ===
 * id`, which is the editor's own store — the same "screen echo" trap
 * `e2e/fixtures/pipelines.ts` was extracted for. A badge would appear whether
 * or not the change ever reached `yamlCode`, and `yamlCode` is what the save
 * actually sends (`model/usePipelineGraphDraft.ts` reads it from the store at
 * click time). So the badge is checked as a fast-failing diagnostic and the
 * evidence is the document the backend stored.
 *
 * The legacy test guards itself with `pytest.skip` when the menu item is not
 * offered. It is offered here, and the conditions are stated rather than
 * probed: `NodeCardHeader.tsx:251` withholds "Make entrypoint" from a node
 * that already IS the entrypoint and from Condition/Decision nodes, and
 * renders the menu at all only while the card is EXPANDED — which is the
 * editor's default (`FlowEditor.tsx:164`, `expandAll` starts `true`).
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

/** The id `getInitialNodeId` mints for the first Printer node on this graph. */
const ADDED_NODE_ID = 'Printer_1';

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline === undefined) continue;
    await deletePipeline(page.request, pipeline);
  }
});

/**
 * Open the editor and give the canvas the width, through the editor's own two
 * collapse affordances.
 *
 * Not defensive padding — the measurement is `pipelines.graph-authoring.
 * spec.ts`'s: with the configuration panel open, a node added at the viewport
 * centre lands UNDER it and Playwright reports
 * `<div data-testid="edit-pipeline-configuration-form-gap"> … intercepts
 * pointer events` on every retry. React Flow's canvas is a transformed
 * surface, so nothing about scrolling moves a node out from under an overlay.
 */
async function openCanvas(page: Page, pipeline: CreatedPipeline): Promise<void> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 20_000 });
  await page.getByRole('button', { name: 'Collapse the configuration panel' }).click();
  await expect(page.getByRole('button', { name: 'Expand the configuration panel' })).toBeVisible({ timeout: 10_000 });
  await page.getByRole('button', { name: 'Collapse the chat panel' }).click();
  await expect(page.getByRole('button', { name: 'Expand the chat panel' })).toBeVisible({ timeout: 10_000 });
}

/**
 * Bring the whole graph on screen and wait until it stops moving.
 *
 * `locator.click()` on WebKit refuses an element outside the viewport
 * outright ("element is outside of the viewport"), where Chromium scrolls to
 * it — and this canvas cannot be scrolled. The editor's own Fit View is the
 * fix, and the box check afterwards is what makes "fitted" an assertion
 * rather than a hope: a React Flow node far below the fold still has a
 * non-zero box and still reports visible, so `toBeVisible()` cannot answer
 * this question (the same measurement `pipelines.graph-authoring.spec.ts`'s
 * `handleCentre` records).
 */
async function fitAndSettle(page: Page, target: ReturnType<Page['locator']>): Promise<void> {
  await page.getByRole('button', { name: 'Fit View' }).click();
  await expect
    .poll(
      async () => {
        const canvas = await page.getByTestId('rf__wrapper').boundingBox();
        const box = await target.boundingBox();
        if (canvas === null || box === null) return false;
        const x = box.x + box.width / 2;
        const y = box.y + box.height / 2;
        return x >= canvas.x && x <= canvas.x + canvas.width && y >= canvas.y && y <= canvas.y + canvas.height;
      },
      {
        timeout: 30_000,
        intervals: [250],
        message: 'the node control never came to rest inside the canvas — Fit View did not bring the graph on screen',
      },
    )
    .toBe(true);
}

/** Click Save and wait for the version PUT to land — not merely for the click. */
async function saveAndAwaitPersist(page: Page): Promise<void> {
  const persisted = page.waitForResponse(
    response =>
      response.request().method() === 'PUT' &&
      response.url().includes('/version/prompt_lib/') &&
      response.status() < 400,
    { timeout: 30_000 },
  );
  // Enabled is itself an assertion: `GraphAdmissionGate` holds this button
  // disabled while the live graph is one the runtime would refuse, so a graph
  // broken by the entrypoint move fails HERE, naming the button.
  await expect(
    page.getByTestId('pipeline-save-button'),
    'Save is disabled — the editor considers the graph inadmissible after the entrypoint move',
  ).toBeEnabled({ timeout: 20_000 });
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

test('J16g: "Make entrypoint" on a node’s own menu moves the entry point in the STORED document', async ({ page }) => {
  test.slow();
  const pipeline = await createPipelineThroughApi(
    page.request,
    `${AUTOTEST_PREFIX}entry-${String(Date.now()).slice(-7)}`,
  );
  created.push(pipeline);

  // What the pipeline was created with — read from the server, so the "moved"
  // claim below is measured against a starting point this file did not
  // invent. The starter template enters at its single LLM node.
  const before = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, pipeline.versionId);
  expect(
    parseStoredGraph(before.instructions).entry_point,
    'the starter template must enter at its own LLM node, or there is nothing to move',
  ).toBe(PIPELINE_STARTER_ENTRY_NODE_ID);

  await openCanvas(page, pipeline);

  // A SECOND node, added through the editor's own menu so its id and its
  // seeded fields are the ones the editor really mints. Printer is one of the
  // types whose defaults are complete on creation, so the graph stays
  // saveable without configuring anything (`pipelines.lifecycle.spec.ts`'s
  // SAVEABLE_NODE_LABELS records which are and which are not).
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: 'Printer', exact: true }).click();
  const addedNode = page.locator(`.react-flow__node[data-id="${ADDED_NODE_ID}"]`);
  await expect(addedNode).toBeVisible({ timeout: 20_000 });

  const nodeMenuTrigger = addedNode.getByRole('button', { name: 'Node actions' });
  await fitAndSettle(page, nodeMenuTrigger);

  await nodeMenuTrigger.click();
  const menuItem = page.getByRole('menuitem', { name: 'Make entrypoint' });
  await expect(
    menuItem,
    'a node that is not already the entry point must offer "Make entrypoint" — the legacy test skipped itself here',
  ).toBeVisible({ timeout: 10_000 });
  await menuItem.click();

  // DIAGNOSTIC ONLY, and deliberately labelled as such: `NodeCardHeader.tsx`
  // withholds "Make entrypoint" from the node that already IS the entry
  // point, so re-opening this menu and finding only "Delete" says the click
  // reached the editor's store — and nothing more, because that store is
  // exactly what a screen assertion here would be reading back. The evidence
  // is the stored document below.
  await nodeMenuTrigger.click();
  const reopened = page.getByRole('menu');
  await expect(reopened).toBeVisible({ timeout: 10_000 });
  await expect(reopened.getByRole('menuitem', { name: 'Make entrypoint' })).toHaveCount(0);
  await expect(reopened.getByRole('menuitem')).toHaveCount(1);
  await page.keyboard.press('Escape');
  await expect(reopened).toHaveCount(0);

  await saveAndAwaitPersist(page);

  const stored = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, pipeline.versionId);
  const graph = parseStoredGraph(stored.instructions);

  // THE ASSERTION. `entry_point` in the document the worker would compile,
  // naming the node the menu was opened on.
  expect(
    graph.entry_point,
    'the entry point never reached the stored document — `setYamlJsonObject` updated the canvas and not `yamlCode`, ' +
      'which is the string the save actually sends',
  ).toBe(ADDED_NODE_ID);

  // The rest of the graph is intact: moving the entry point is not a rewrite.
  // Without this, a "make entrypoint" that replaced the document with a
  // single node would satisfy the line above.
  const ids = storedNodeIds(graph);
  expect(ids, 'the node the pipeline used to enter at must still be in the document').toContain(
    PIPELINE_STARTER_ENTRY_NODE_ID,
  );
  expect(ids).toContain(ADDED_NODE_ID);
  expect(stored.agentType, 'a save must not demote the pipeline to an agent').toBe('pipeline');
});
