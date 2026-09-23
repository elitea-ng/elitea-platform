/**
 * The HITL node's own configuration form (`HITLNode`/`HITLNode.parts.tsx`,
 * `src/features/pipelines/ui/nodes/HITLNode*.tsx`) — the Router-mapping
 * (Approve/Edit/Reject route targets), the Edit-state-key picker, and the
 * user-message F-String gate. No case in this package's list names this
 * screen directly; adding and wiring a HITL node correctly is a precondition
 * of ELITEA-0865 ("add a HITL node … observe the trigger reset"), whose own
 * trigger-reset half is FAIL-MARKED in `pipelines.entrypoint.spec.ts`
 * (Schedule/Webhook can never be selected in the first place, so there is no
 * non-Chat-Message trigger to reset FROM). This file is the independent,
 * PASSING half: the node itself can be added, its routes are seeded legally,
 * the Edit route's own validation gate holds, and a configured route
 * persists.
 *
 * Runtime behaviour of a HITL node (pausing a real run, Approve/Reject
 * actually resuming execution) is `w1-pipeline-nodes-hitl.md`'s package —
 * all 5 of its cases are STREAM-DEFERRED (a real model turn is required);
 * see the ledger.
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
  storedNodeIds,
  type CreatedPipeline,
} from '../../fixtures/pipelines';

const HITL_NODE_ID = 'HITL_1';

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline !== undefined) await deletePipeline(page.request, pipeline);
  }
});

async function openEditorAndAddHitlNode(page: Page, pipeline: CreatedPipeline) {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });

  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: 'Human-in-the-loop', exact: true }).click();
  const card = page.locator(`.react-flow__node[data-id="${HITL_NODE_ID}"]`);
  await expect(card).toBeVisible({ timeout: 15_000 });
  return card;
}

async function saveAndAwaitPersist(page: Page): Promise<void> {
  const persisted = page.waitForResponse(
    (response) => response.request().method() === 'PUT' && response.url().includes('/version/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  await expect(page.getByTestId('pipeline-save-button'), 'the freshly added HITL node must be admissible as-seeded').toBeEnabled({
    timeout: 10_000,
  });
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

/* onetest: ELITEA-0865 (editor half — see this file's own doc comment) — a HITL node can be added; its Approve/Reject routes are seeded legally (END) and its Edit route is empty, not the empty-string the compiler refuses. */
test('a freshly added HITL node seeds legal Approve/Reject routes and no Edit route', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}hitl-seed-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const card = await openEditorAndAddHitlNode(page, pipeline);

  const routeSelects = card.getByRole('combobox', { name: 'Route' });
  await expect(routeSelects).toHaveCount(3);
  // HITL_ACTIONS order: Approve, Edit, Reject.
  await expect(routeSelects.nth(0)).toHaveText('END');
  await expect(routeSelects.nth(2)).toHaveText('END');

  await saveAndAwaitPersist(page);

  const versionId = await resolveLatestPipelineVersionId(page.request, pipeline.projectId, pipeline.id);
  const stored = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, versionId);
  const graph = parseStoredGraph(stored.instructions);
  expect(storedNodeIds(graph)).toContain(HITL_NODE_ID);
  const hitlNode = graph.nodes.find((node) => node['id'] === HITL_NODE_ID) as { routes?: Record<string, string> } | undefined;
  expect(hitlNode?.routes?.['approve']).toBe('END');
  expect(hitlNode?.routes?.['reject']).toBe('END');
  expect(hitlNode?.routes?.['edit'], 'an unconfigured Edit route must not reach the stored document').toBeFalsy();
});

/* onetest: ELITEA-0865 (editor half) — the Edit route selector is disabled until an Edit-state-key is chosen (`computeRouteSelectDisabled`); setting the key first enables it, and both reach the stored document together. */
test('the Edit route is unlocked by choosing an Edit-state-key first', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}hitl-edit-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const card = await openEditorAndAddHitlNode(page, pipeline);

  // The Edit route's own options exclude END (`editRouteOptions` filters it
  // out — an Edit route must resume at a real node); `LLM_1`, the starter
  // template's own entry node, is the legal target picked below.

  // Before a key is chosen, `canEditRoute` is false and the Edit route
  // selector is disabled outright — the card never lets a route be picked
  // for a key that is not there yet.
  const editRoute = card.getByRole('combobox', { name: 'Route' }).nth(1);
  await expect(editRoute).toBeDisabled();

  const editStateKey = card.getByRole('combobox', { name: 'Value' });
  await editStateKey.click();
  await page.getByRole('option', { name: 'input', exact: true }).click();

  await expect(editRoute).toBeEnabled({ timeout: 10_000 });
  await editRoute.click();
  await page.getByRole('option', { name: 'LLM_1', exact: true }).click();
  await expect(card.getByText('Provide an edit state key before using the Edit route.')).toHaveCount(0);

  await saveAndAwaitPersist(page);

  const versionId = await resolveLatestPipelineVersionId(page.request, pipeline.projectId, pipeline.id);
  const stored = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.id, versionId);
  const graph = parseStoredGraph(stored.instructions);
  const hitlNode = graph.nodes.find((node) => node['id'] === HITL_NODE_ID) as
    | { routes?: Record<string, string>; edit_state_key?: string }
    | undefined;
  expect(hitlNode?.routes?.['edit']).toBe('LLM_1');
  expect(hitlNode?.edit_state_key).toBe('input');
});

/* onetest: ELITEA-0865 (editor half) — the user-message Input (state-variable) picker is enabled only in F-String mode, matching the tooltip's own stated condition. */
test('the HITL node\'s Input picker is enabled only when the user message is F-String', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}hitl-fstr-${Date.now() % 1e9}`;
  const pipeline = await createPipelineThroughApi(page.request, name);
  created.push(pipeline);
  const card = await openEditorAndAddHitlNode(page, pipeline);

  // Fixed (the default seed) — the Input picker stays disabled. `HITLNode`
  // renders this `InputSelect` with an empty `label` (its own label row is
  // separate, via `LabelWithTooltip`), which leaves the control with no
  // accessible name at all — but it is the FIRST thing in the card's body
  // (above the user-message editor, above the Router-mapping accordion),
  // so it is always the first `combobox` on a non-Entrypoint node.
  const inputPicker = card.getByRole('combobox').first();
  await expect(inputPicker).toBeDisabled();

  // Switch the user message's own "Type" mapping (`SimpleLLMInputItem`) to
  // F-String — only then does the tooltip's stated condition ("Available
  // only when the User message type is set to F-String") hold.
  const messageType = card.getByRole('combobox', { name: 'Type' });
  await messageType.click();
  await page.getByRole('option', { name: 'F-String', exact: true }).click();

  await expect(inputPicker).toBeEnabled({ timeout: 10_000 });
});
