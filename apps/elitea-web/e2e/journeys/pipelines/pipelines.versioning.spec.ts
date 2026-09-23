/**
 * Journey 16b: pipeline VERSIONING — save as a new version, switch versions,
 * pin a default.
 *
 * Pipelines are `application` rows and their versions are
 * `application_versions` rows, so the editor mounts the same version bar the
 * agent editor does (`features/agents`' `AgentVersionControls`). What is NOT
 * shared is the create path's behaviour, and that is what this file exists to
 * pin down against the running backend rather than against the schema:
 *
 *  - `CreateVersion` -> `versionFromBody`
 *    (`services/elitea-main/internal/api/v2/applications/handler.go:496-525`)
 *    substitutes the literal `"openai"` for an empty `agent_type`
 *    (`internal/infra/db/repos/applications.go:29`), and
 *  - reads no `pipeline_settings` key at all — `insertVersion` (:517-525)
 *    does not name the column, so the POST physically cannot store the graph
 *    geometry. The editor follows it with the PUT that can.
 *
 * Every assertion below is read back over the API from the STORED document,
 * never off the screen. A version selector that relabels its trigger and a
 * version selector that actually loads a different graph look identical on a
 * screenshot; only the persisted row tells them apart. That is the same trap
 * `e2e/fixtures/pipelines.ts` was written for.
 */
import { test, expect, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, clickCreateButton, deleteAgent } from '../../fixtures/api';
import {
  COMPILER_LEGAL_NODE_ID,
  parseStoredGraph,
  readDefaultPipelineVersionId,
  readStoredPipelineVersion,
  resolveLatestPipelineVersionId,
  storedNodeIds,
} from '../../fixtures/pipelines';

const createdIds: string[] = [];

test.afterEach(async ({ page }) => {
  while (createdIds.length > 0) {
    const id = createdIds.pop();
    if (id !== undefined) await deleteAgent(page.request, id);
  }
});

/** Add one node of `label` through the editor's own menu — no store pokes. */
async function addNodeThroughMenu(page: Page, label: string): Promise<void> {
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: label, exact: true }).click();
}

/**
 * Drive the real create form to completion and return the backend-assigned id.
 * A SERIAL id in the URL is something no stub route can produce.
 */
async function createPipelineThroughUi(page: Page, name: string): Promise<string> {
  await page.goto(BASE_URL + '/app/pipelines/my');
  await page.waitForURL('**/pipelines**', { timeout: 15_000 });
  await clickCreateButton(page);
  await page.waitForURL('**/app/pipelines/create**', { timeout: 15_000 });

  const panel = page.getByTestId('create-pipeline-form-panel');
  await expect(panel.getByTestId('agent-name-input')).toBeVisible({ timeout: 10_000 });
  await panel.getByTestId('agent-name-input').fill(name);
  await panel.getByTestId('agent-description-input').fill(`${AUTOTEST_PREFIX}JRNY-016b versioning`);
  await page.getByTestId('pipeline-save-button').click();

  await page.waitForURL(/\/app\/pipelines\/latest\/\d+/, { timeout: 20_000 });
  const id = /\/app\/pipelines\/latest\/(\d+)/.exec(page.url())?.[1];
  expect(id, 'create must navigate to the backend-assigned pipeline id').toBeTruthy();
  createdIds.push(id as string);
  return id as string;
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
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

/**
 * "Save As Version" through the bar's own dialog, returning the id the POST
 * minted.
 *
 * `expectGraphCarry` waits for the follow-up PUT as well — the create alone
 * leaves the new version without its geometry, and asserting before that lands
 * would race. Pass `false` in a test that asserts nothing about the graph.
 *
 * `usePipelineGraphDraft` returns `undefined`, and no second request is made,
 * only for an editor whose YAML pane is EMPTY — deliberately, since writing
 * then would blank a real stored graph. That is no longer the state a freshly
 * created pipeline is in: `pages/pipelines/CreatePipeline.tsx` stores
 * `shared/lib/pipelineStarterTemplate.ts` when the author typed no document,
 * so a new pipeline opens on a real graph and the carry PUT follows every
 * clone. A `false` here therefore means "do not wait for it", not "it does
 * not happen".
 */
async function saveAsVersion(page: Page, versionName: string, expectGraphCarry = true): Promise<string> {
  const created = page.waitForResponse(
    response =>
      response.request().method() === 'POST' &&
      response.url().includes('/versions/prompt_lib/') &&
      response.status() < 400,
    { timeout: 30_000 },
  );
  // Armed only when it is expected: an un-awaited `waitForResponse` that times
  // out rejects with nobody listening.
  const carried = expectGraphCarry
    ? page.waitForResponse(
        response =>
          response.request().method() === 'PUT' &&
          response.url().includes('/version/prompt_lib/') &&
          response.status() < 400,
        { timeout: 30_000 },
      )
    : undefined;

  await page.getByRole('button', { name: 'Save As Version' }).click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible({ timeout: 10_000 });
  await dialog.getByLabel('Version name').fill(versionName);
  await dialog.getByRole('button', { name: 'Save', exact: true }).click();

  const body = (await (await created).json()) as { id?: string | number };
  if (carried !== undefined) await carried;
  expect(body.id, 'the create response must carry the new version id').toBeTruthy();
  return String(body.id);
}

test('J16b: "Save As Version" stores a runnable pipeline version, graph and all', async ({ page }) => {
  test.slow();
  const name = `${AUTOTEST_PREFIX}ver-${Date.now() % 1e9}`;
  const id = await createPipelineThroughUi(page, name);

  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });

  // Author a graph on the BASE version and persist it, so the clone below has
  // something real to carry and the two versions are distinguishable.
  await addNodeThroughMenu(page, 'Printer');
  await saveAndAwaitPersist(page);

  const baseVersionId = await resolveLatestPipelineVersionId(page.request, DEFAULT_PROJECT_ID, id);
  const baseStored = await readStoredPipelineVersion(page.request, DEFAULT_PROJECT_ID, id, baseVersionId);
  /**
   * Every node id the base version holds BEFORE the clone.
   *
   * Read once, and used below as the exclusion set that names the node this
   * test added. A new pipeline is no longer created on an empty document:
   * `pages/pipelines/CreatePipeline.tsx` stores
   * `shared/lib/pipelineStarterTemplate.ts` when the author typed none, so a
   * new pipeline opens runnable. That template ships one LLM node, and it is
   * called `LLM_1`. "The LLM node in the clone" is therefore ambiguous, and
   * `find(id => id.startsWith('LLM'))` matched the STARTER's node — which the
   * base legitimately holds — so the last assertion read the base's own
   * content as an overwrite. Identify the added node by difference instead.
   */
  const baseIdsBefore = storedNodeIds(parseStoredGraph(baseStored.instructions));
  const baseNodeId = baseIdsBefore.find(nodeId => nodeId.startsWith('Printer'));
  expect(baseNodeId, 'the authored Printer node must reach the base version').toBeTruthy();

  // Add a SECOND node, do NOT press Save, and clone. The clone must carry the
  // live canvas, not the last-stored document — that is what "Save As Version"
  // means, and the create POST alone would send the stored `instructions`.
  // The editor mints `LLM_2` here, because `getInitialNodeId`
  // (`features/pipelines/lib/flow-editor/helpers/nodeIdentity.helpers.ts`)
  // skips the starter's `LLM_1`.
  await addNodeThroughMenu(page, 'LLM');
  const newVersionId = await saveAsVersion(page, `v${Date.now() % 1e5}`);
  expect(newVersionId).not.toBe(baseVersionId);

  const stored = await readStoredPipelineVersion(page.request, DEFAULT_PROJECT_ID, id, newVersionId);

  // 1. It is still a PIPELINE. An empty `agent_type` on create silently
  //    becomes "openai" — the same rows, run by the wrong executor.
  expect(stored.agentType, 'a cloned pipeline version must stay a pipeline').toBe('pipeline');

  // 2. The LIVE graph reached it: both the node saved earlier and the one that
  //    was only ever on the canvas.
  const graph = parseStoredGraph(stored.instructions);
  const ids = storedNodeIds(graph);
  expect(ids).toContain(baseNodeId as string);
  // The LLM node the base did NOT already carry — that one, and only that
  // one, was on the canvas and never saved. Matching any `LLM*` would also
  // match the starter template's `LLM_1`, which the base and the clone share.
  const clonedLlmId = ids.find(nodeId => nodeId.startsWith('LLM') && !baseIdsBefore.includes(nodeId));
  expect(clonedLlmId, 'the unsaved canvas node must travel with the clone').toBeTruthy();
  for (const nodeId of ids) {
    expect(nodeId, `stored node id "${nodeId}" is not addressable by the pipeline compiler`).toMatch(
      COMPILER_LEGAL_NODE_ID,
    );
  }

  // 3. The LIVE laid-out geometry reached the new version.
  const nodes = stored.pipelineSettings['nodes'];
  expect(Array.isArray(nodes), 'pipeline_settings.nodes must preserve the live canvas').toBe(true);
  expect((nodes as unknown[]).length).toBeGreaterThan(0);

  // 4. `meta` is not reset. `versionFromBody` DOES read the key and only
  //    defaults `step_limit` when the caller sends none.
  expect(stored.meta['step_limit']).toBeDefined();

  // The base version is untouched — a clone is a new row, not an overwrite.
  // Compared against the ids read BEFORE the clone, not against a name shape:
  // the exclusion above already removed the only id both versions may share by
  // right, so an equality here says "nothing moved", which is the claim.
  const baseAfter = await readStoredPipelineVersion(page.request, DEFAULT_PROJECT_ID, id, baseVersionId);
  const baseIdsAfter = storedNodeIds(parseStoredGraph(baseAfter.instructions));
  // Sorted: node ORDER inside the document is not part of the claim, node
  // CONTENT is. A reorder must not fail this test for the wrong reason.
  expect(baseIdsAfter).not.toContain(clonedLlmId as string);
  expect([...baseIdsAfter].sort()).toEqual([...baseIdsBefore].sort());
});

/**
 * Main now accepts graph geometry on version creation. Pin its exact stored
 * value, not the earlier omission. The editor still sends its live canvas
 * through the follow-up PUT; its POST body contains the stored instructions.
 * The preceding UI test independently verifies that unsaved edits survive.
 */
test('J16b: version creation preserves graph instructions and geometry', async ({ page }) => {
  const name = `${AUTOTEST_PREFIX}post-${Date.now() % 1e9}`;
  const id = await createPipelineThroughUi(page, name);

  const instructions = 'entry_point: Printer_1\nnodes:\n  - id: Printer_1\n    type: printer\n';
  const pipelineSettings = { nodes: [{ id: 'Printer_1' }], edges: [], orientation: 'vertical', layout_version: '1.0' };
  const resp = await page.request.post(
    `${API_BASE}/elitea_core/versions/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`,
    {
      data: {
        name: `probe${Date.now() % 1e5}`,
        agent_type: 'pipeline',
        instructions,
        pipeline_settings: pipelineSettings,
      },
    },
  );
  expect(resp.status(), await resp.text()).toBe(201);
  const created = (await resp.json()) as { id?: string | number };

  const stored = await readStoredPipelineVersion(page.request, DEFAULT_PROJECT_ID, id, String(created.id));
  expect(stored.instructions).toBe(instructions);
  expect(stored.agentType).toBe('pipeline');
  expect(stored.pipelineSettings).toEqual(pipelineSettings);
});

test('J16b: the version selector loads the chosen version graph into the editor', async ({ page }) => {
  test.slow();
  const name = `${AUTOTEST_PREFIX}sel-${Date.now() % 1e9}`;
  const id = await createPipelineThroughUi(page, name);
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });

  await addNodeThroughMenu(page, 'Printer');
  await saveAndAwaitPersist(page);
  const baseVersionId = await resolveLatestPipelineVersionId(page.request, DEFAULT_PROJECT_ID, id);

  // Clone, then diverge: the new version gains a node the base never had.
  const versionName = `v${Date.now() % 1e5}`;
  const newVersionId = await saveAsVersion(page, versionName);
  await page.waitForURL(new RegExp(`/app/pipelines/\\w+/${id}/${newVersionId}`), { timeout: 20_000 });
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });
  await addNodeThroughMenu(page, 'Decision');
  await saveAndAwaitPersist(page);

  const divergent = await readStoredPipelineVersion(page.request, DEFAULT_PROJECT_ID, id, newVersionId);
  const divergentId = storedNodeIds(parseStoredGraph(divergent.instructions)).find(nodeId => nodeId.startsWith('Decision'));
  expect(divergentId, 'the second version must hold a node the base does not').toBeTruthy();
  await expect(page.locator(`.react-flow__node[data-id="${divergentId as string}"]`)).toBeVisible();

  // Now switch BACK to base through the dropdown. The canvas must lose the
  // node that only the other version has — the assertion a trigger relabel
  // cannot satisfy.
  await page.getByTestId('version-selector-trigger').click();
  await page.getByRole('menuitem', { name: 'base', exact: true }).click();

  await page.waitForURL(new RegExp(`/app/pipelines/\\w+/${id}/${baseVersionId}`), { timeout: 20_000 });
  await expect(page.locator(`.react-flow__node[data-id="${divergentId as string}"]`)).toHaveCount(0, { timeout: 15_000 });
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });

  // …and forward again, to prove the switch is not one-way.
  await page.getByTestId('version-selector-trigger').click();
  await page.getByRole('menuitem', { name: new RegExp(versionName) }).click();
  await expect(page.locator(`.react-flow__node[data-id="${divergentId as string}"]`)).toBeVisible({ timeout: 15_000 });
});

test('J16b: "Set as default" moves the pipeline default the SERVER reports', async ({ page }) => {
  test.slow();
  const name = `${AUTOTEST_PREFIX}def-${Date.now() % 1e9}`;
  const id = await createPipelineThroughUi(page, name);
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });

  const baseVersionId = await resolveLatestPipelineVersionId(page.request, DEFAULT_PROJECT_ID, id);
  // This test is about the default POINTER, not the graph, so it does not
  // wait for the carry PUT — see `saveAsVersion`.
  const newVersionId = await saveAsVersion(page, `v${Date.now() % 1e5}`, false);
  await page.waitForURL(new RegExp(`/app/pipelines/\\w+/${id}/${newVersionId}`), { timeout: 20_000 });

  // Before: `GetDefaultVersion` falls back to the version named `base`.
  expect(await readDefaultPipelineVersionId(page.request, DEFAULT_PROJECT_ID, id)).toBe(baseVersionId);

  const patched = page.waitForResponse(
    response =>
      response.request().method() === 'PATCH' &&
      response.url().includes('/default_version/prompt_lib/') &&
      response.status() < 400,
    { timeout: 30_000 },
  );
  await page.getByTestId('version-selector-trigger').click();
  await page.getByTestId('agent-version-set-default').click();
  await page.getByRole('button', { name: 'Set as a default' }).click();
  await patched;

  // After: the SERVER's own answer moved. The bar only remembers what it set
  // (nothing in the documented contract reports a default back), so this is
  // the one reading that is not the component's own state played back.
  await expect
    .poll(() => readDefaultPipelineVersionId(page.request, DEFAULT_PROJECT_ID, id), { timeout: 15_000 })
    .toBe(newVersionId);
});
