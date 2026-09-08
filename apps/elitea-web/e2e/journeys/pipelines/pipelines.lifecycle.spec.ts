/**
 * Journey 16: Create pipeline → edit flow graph → save (JRNY-016)
 *
 * Spec §8.5 acceptance (from parity/manifest/pipelines.json JRNY-016):
 *   (1) the saved pipeline reloads with the same graph;
 *   (2) validation errors block saving with a visible reason.
 *
 * ── Why this file no longer contains a single `.or()` chain ──────────────
 * The previous version accepted `getByRole('heading', {name: /create
 * pipeline/i})` as a substitute for the create form, and wrapped every
 * subsequent step in `if (!hasForm) return;` / `.catch(() => false)`. Under
 * that shape a route that rendered nothing but an `<h5>` reported green —
 * and it did exactly that for as long as `CreatePipeline.tsx` shipped a
 * self-closing `<Box data-testid="create-pipeline-form-panel" />`. Every
 * assertion below now names something a stub cannot produce: a specific
 * form control, a backend-minted SERIAL id in the URL, a React-Flow node
 * element, or a name echoed back by the detail fetch.
 *
 * ── A new pipeline is NOT empty ─────────────────────────────────────────────
 * `pages/pipelines/CreatePipeline.tsx` shows and stores
 * `shared/lib/pipelineStarterTemplate.ts` when the author typed no document,
 * so the editor opens on one `LLM_1` node wired to `END` and that graph runs.
 * Every "the node I added" claim below is therefore computed by DIFFERENCE
 * against the ids present right after creation — on the canvas through
 * `canvasNodeIds`, in the stored document through a read of the created
 * version. A bare `startsWith('LLM')` or a `>= n` count matches the starter's
 * own content and would let a journey prove itself with a node it never
 * authored, which is the trap `pipelines.versioning.spec.ts`'s J16b already
 * hit.
 */
import { test, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, clickCreateButton, deleteAgent } from '../../fixtures/api';
import {
  COMPILER_LEGAL_NODE_ID,
  parseStoredGraph,
  readStoredPipelineVersion,
  resolveLatestPipelineVersionId,
  storedNodeIds,
} from '../../fixtures/pipelines';

/**
 * The Add-node menu labels for the nine node types the Rust pipeline
 * compiler admits (`parse_pipeline_node`,
 * `services/elitea-worker-rust/src/agents/graph/compiler.rs:1236`).
 * `Code` and `Custom` are deliberately NOT here: the compiler has no arm for
 * either, so a pipeline containing one cannot load.
 */
const ADMITTED_NODE_LABELS = [
  'Agent',
  'Decision',
  'Human-in-the-loop',
  'LLM',
  'MCP',
  'Printer',
  'Router',
  'State modifier',
  'Toolkit',
] as const;

/**
 * The subset of {@link ADMITTED_NODE_LABELS} whose seeded defaults are
 * COMPLETE the moment the node is added, so a pipeline holding them can be
 * saved with no further configuration.
 *
 * The three that are missing — Agent, Toolkit, MCP — are missing on purpose,
 * and their absence is a statement about this change rather than a gap in
 * it. Their runtime-required fields (`tool` for an Agent,
 * `toolkit_name`+`tool` for a direct tool) are seeded EMPTY: the runtime
 * calls the Agent one a participant alias (`application.rs:49`) and nothing
 * pins which string resolves, and a toolkit binding is a user choice, so the
 * editor asks rather than guesses. The editor's own validation then refuses
 * to save until they are filled — which is exactly the intended "empty and
 * required" behaviour, and means a stored-document assertion cannot cover
 * them without first driving a toolkit picker. Their minted ids are checked
 * on the canvas by the test below instead.
 */
const SAVEABLE_NODE_LABELS = ADMITTED_NODE_LABELS.filter(
  label => label !== 'Agent' && label !== 'Toolkit' && label !== 'MCP',
);

/** Add one node of `label` through the editor's own menu — no store pokes. */
async function addNodeThroughMenu(page: Page, label: string): Promise<void> {
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: label, exact: true }).click();
}

/**
 * Every node id currently drawn on the canvas.
 *
 * Read BEFORE the first `addNodeThroughMenu` call, this is the baseline every
 * "the node I added" assertion below is computed against. A pipeline created
 * through this form is no longer empty: `pages/pipelines/CreatePipeline.tsx`
 * seeds `shared/lib/pipelineStarterTemplate.ts`, so the editor opens on one
 * `LLM_1` node next to `END`. "The LLM node" is therefore ambiguous, and a
 * `find(id => id.startsWith('LLM'))` matches the STARTER — which would let a
 * journey report the starter's own content as the thing it added.
 * `pipelines.versioning.spec.ts`'s J16b hit exactly that and identifies its
 * node by difference for the same reason.
 */
async function canvasNodeIds(page: Page): Promise<readonly string[]> {
  return page.locator('.react-flow__node').evaluateAll(nodes => nodes.map(node => node.getAttribute('data-id') ?? ''));
}

/** Click Save and wait for the version PUT to actually land, not just for the click. */
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
  // A failed save renders a role="alert" banner (EditPipeline.tsx); no toast
  // infrastructure exists.
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

/** Application ids minted by the tests below, deleted in `afterEach`. */
const createdIds: string[] = [];

test.afterEach(async ({ page }) => {
  while (createdIds.length > 0) {
    const id = createdIds.pop();
    if (id !== undefined) await deleteAgent(page.request, id);
  }
});

/**
 * Drive the REAL create form to completion and return the application id the
 * backend assigned. Everything here is a hard assertion — there is no
 * "the route may still be a placeholder" branch, because a placeholder is
 * precisely the failure this journey exists to catch.
 */
async function createPipelineThroughUi(page: Page, name: string): Promise<string> {
  await page.goto(BASE_URL + '/app/pipelines/my');
  await page.waitForURL('**/pipelines**', { timeout: 15_000 });
  await checkA11y(page);

  await clickCreateButton(page);
  await page.waitForURL('**/app/pipelines/create**', { timeout: 15_000 });

  // The panel must CONTAIN the real controls. `toBeVisible()` on the panel
  // alone is what let the hollow version ship (see CreatePipeline.test.tsx's
  // own comment on the same trap): an empty <Box> is in the document, and
  // an <h5> is visible.
  const panel = page.getByTestId('create-pipeline-form-panel');
  const nameInput = panel.getByTestId('agent-name-input');
  const descriptionInput = panel.getByTestId('agent-description-input');
  await expect(nameInput).toBeVisible({ timeout: 10_000 });
  await expect(descriptionInput).toBeVisible();

  const saveButton = page.getByTestId('pipeline-save-button');

  // Acceptance (2): a validation error blocks the save AND states a reason.
  // Fill both fields first so the "blocked" state below is demonstrably
  // reached by clearing a field, not merely by the form being untouched.
  await nameInput.fill(name);
  await descriptionInput.fill(`${AUTOTEST_PREFIX}JRNY-016 pipeline`);
  await expect(saveButton).toBeEnabled();

  await nameInput.fill('');
  await expect(saveButton).toBeDisabled();
  await expect(panel.getByText('Name is required')).toBeVisible();

  await nameInput.fill(name);
  await expect(panel.getByText('Name is required')).toHaveCount(0);
  await expect(saveButton).toBeEnabled();

  await checkA11y(page);
  await saveButton.click();

  // A SERIAL id minted by POST /elitea_core/applications — nothing the
  // frontend can invent, so this URL cannot be satisfied by a stub route.
  await page.waitForURL(/\/app\/pipelines\/latest\/\d+/, { timeout: 20_000 });
  const id = /\/app\/pipelines\/latest\/(\d+)/.exec(page.url())?.[1];
  expect(id, 'create must navigate to the backend-assigned pipeline id').toBeTruthy();
  createdIds.push(id as string);
  return id as string;
}

test('J16: create a pipeline through the real form and land on a live flow editor', async ({ page }) => {
  // `MAX_NAME_LENGTH` (CreateAgentForm.tsx:147) is 32 — keep the unique
  // suffix short so the value asserted on is the value actually stored.
  const name = `${AUTOTEST_PREFIX}pipe-${Date.now() % 1e9}`;
  await createPipelineThroughUi(page, name);

  // The editor page, not a shell. `pipeline-config-tab` is GeneralFormPanel.tsx:93;
  // `rf__wrapper`/`rf__node-END` are emitted by a mounted @xyflow/react canvas
  // holding the default pipeline state — a stub page renders neither.
  await expect(page.getByTestId('pipeline-config-tab')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId('rf__wrapper')).toBeVisible();
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible();

  /*
   * `END` ALONE is the state this product decision moved away from: a new
   * pipeline is created on `shared/lib/pipelineStarterTemplate.ts`, so the
   * editor opens on a graph that runs rather than on an empty canvas. The
   * starter's own id goes through the same compiler grammar as a minted one —
   * it is stored in the very same document.
   */
  const starterIds = (await canvasNodeIds(page)).filter(nodeId => nodeId !== 'END');
  expect(starterIds.length, 'a new pipeline must open on the starter graph, not on END alone').toBeGreaterThan(0);
  for (const nodeId of starterIds) {
    expect(nodeId, `starter node id "${nodeId}" is not addressable by the pipeline compiler`).toMatch(
      COMPILER_LEGAL_NODE_ID,
    );
  }

  // Backend-derived: EditPipeline.tsx renders `pipelineDetailDisplayName(detail)`
  // from the GET application-detail response, not from anything it navigated with.
  await expect(page.getByRole('heading', { name, exact: true })).toBeVisible({ timeout: 15_000 });
});

test('J16: the pipeline editor screen is accessible (spec §6.4)', async ({ page }) => {
  /*
   * Was RED — two critical `button-name` violations (WCAG 4.1.2) from the
   * icon-only collapse buttons in `features/pipelines/ui/GeneralFormPanel.tsx`
   * and `features/pipelines/ui/ChatPanel.tsx`, neither carrying an
   * `aria-label`, `title`, or visible text. Both now carry a state-tracking
   * `aria-label` (#135), so axe reports a clean screen.
   *
   * Kept as its own test rather than folded into the journey above so a
   * future a11y regression names itself instead of failing somewhere inside
   * the create/save path. `checkA11y`'s own rule list is NOT extended to
   * swallow `button-name`: a violation here is a finding, not noise.
   */
  const name = `${AUTOTEST_PREFIX}a11y-${Date.now() % 1e9}`;
  await createPipelineThroughUi(page, name);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 15_000 });

  await checkA11y(page);
});

test('J16: an edited flow graph survives a save + reload', async ({ page }) => {
  /*
   * JRNY-016 acceptance (1). This was RED: the save submitted only
   * `toVersionDraft(activeVersion, conversationStarters)`, so the PUT carried
   * no nodes, no edges and no `pipeline_settings`, answered 200, and the
   * canvas came back as the single default `END` node on reload.
   *
   * Fixed end to end in #135 — `pipeline_settings` added to
   * `VersionWriteRequest` (services/elitea-main/api/openapi/v2.yaml) and
   * persisted by `UpdateVersion`; the page seeds the flow-editor stores from
   * the loaded version and sends the live graph (`instructions` = the pipeline
   * YAML) back on save.
   */
  const name = `${AUTOTEST_PREFIX}graph-${Date.now() % 1e9}`;
  const id = await createPipelineThroughUi(page, name);

  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });

  /*
   * What the STARTER already stored, read before this journey touches the
   * graph. The added node is named by difference against this list — the
   * starter ships an `LLM_1` node of its own, so "the LLM node in the stored
   * document" is ambiguous and the first `LLM*` id in document order is the
   * starter's, not the one added below. Matching that would have let this
   * journey prove the round trip using content it never authored.
   */
  const versionId = await resolveLatestPipelineVersionId(page.request, DEFAULT_PROJECT_ID, id);
  const before = await readStoredPipelineVersion(page.request, DEFAULT_PROJECT_ID, id, versionId);
  const idsBefore = storedNodeIds(parseStoredGraph(before.instructions));
  expect(idsBefore.length, 'a new pipeline must be created on the starter graph').toBeGreaterThan(0);

  // Edit the graph: add an LLM node through the editor's own menu. (Was an
  // Agent node; an Agent's `tool` participant alias is seeded empty on
  // purpose — see SAVEABLE_NODE_LABELS — so the editor refuses to save one
  // until it is picked, and this journey is about the save/reload round trip,
  // not about that refusal.)
  await addNodeThroughMenu(page, 'LLM');
  await saveAndAwaitPersist(page);

  /*
   * The node id asserted on below is read back from the BACKEND, not typed
   * in here. The previous version of this test hardcoded
   * `.react-flow__node[data-id="Agent 1"]` — a screen echo: the canvas shows
   * whatever the editor's own store holds, so the assertion passed for as
   * long as the editor minted `"Agent 1"`, an id the pipeline compiler
   * refuses outright (`valid_graph_id`, worker `graph/yaml.rs:362`). Every
   * pipeline authored in this editor was unloadable and this journey was
   * green the whole time.
   */
  const stored = await readStoredPipelineVersion(page.request, DEFAULT_PROJECT_ID, id, versionId);
  const graph = parseStoredGraph(stored.instructions);
  const storedIds = storedNodeIds(graph);
  const storedAddedId = storedIds.find(nodeId => nodeId.startsWith('LLM') && !idsBefore.includes(nodeId));
  expect(storedAddedId, 'the added LLM node must reach the stored document').toBeTruthy();
  expect(storedAddedId).toMatch(COMPILER_LEGAL_NODE_ID);
  // A save ADDS to the graph the pipeline was created with; it does not
  // replace it. Without this, a save that dropped the starter and wrote only
  // the new node would still satisfy every assertion above.
  for (const nodeId of idsBefore) {
    expect(storedIds, 'the starter graph must survive the save').toContain(nodeId);
  }

  await page.goto(BASE_URL + `/app/pipelines/latest/${id}`);
  // The reload really did re-fetch this pipeline (backend-derived name).
  await expect(page.getByRole('heading', { name, exact: true })).toBeVisible({ timeout: 15_000 });
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });

  // Acceptance (1): the saved pipeline reloads with the SAME graph — matched
  // against the ids the backend stored, not ones this file invented. Both
  // halves: the node this journey added, and the starter nodes it did not.
  await expect(page.locator(`.react-flow__node[data-id="${storedAddedId as string}"]`)).toBeVisible({
    timeout: 10_000,
  });
  for (const nodeId of idsBefore) {
    await expect(page.locator(`.react-flow__node[data-id="${nodeId}"]`)).toBeVisible({ timeout: 10_000 });
  }
});

/*
 * Ported BY USE CASE from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/pipelines/
 * test_pipeline_advanced.py::TestMultiNodeTopology::
 * test_save_multi_node_pipeline`) — "build a multi-node pipeline, save,
 * reload, and the nodes persist".
 *
 * It extends this file rather than starting a new one because the two tests
 * above already own half of it: "an edited flow graph survives a save +
 * reload" proves the node it added and the starter nodes are BACK on the
 * canvas, and "the STORED pipeline document carries only compiler-legal ids"
 * proves every added node reached the backend. Neither states the claim the
 * legacy test is really making, which is an EQUALITY: the reloaded canvas is
 * the stored graph, no more and no less. A save that also wrote a phantom
 * node, or a reload that drew one, satisfies both of the existing tests.
 *
 * `END` is the one canvas node with no document entry — the starter template
 * and every node's `transition: END` name it, and the editor draws it as the
 * graph's terminal — so it is added to the expected set explicitly rather
 * than excluded by a filter that would also hide a real stray.
 */
test('J16: a saved multi-node graph reloads as exactly the graph that was stored', async ({ page }) => {
  test.slow();
  const name = `${AUTOTEST_PREFIX}multi-${Date.now() % 1e9}`;
  const id = await createPipelineThroughUi(page, name);
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });

  // Two nodes of DIFFERENT types, so a reload that kept one shape and dropped
  // the other cannot pass. Both are in SAVEABLE_NODE_LABELS — their seeded
  // defaults are complete, so the editor's admission gate lets the save
  // through with no further configuration.
  await addNodeThroughMenu(page, 'LLM');
  await addNodeThroughMenu(page, 'Printer');
  await saveAndAwaitPersist(page);

  const versionId = await resolveLatestPipelineVersionId(page.request, DEFAULT_PROJECT_ID, id);
  const stored = await readStoredPipelineVersion(page.request, DEFAULT_PROJECT_ID, id, versionId);
  const storedIds = storedNodeIds(parseStoredGraph(stored.instructions));
  // Three: the starter's own node plus the two added here. Stated as a number
  // because "the canvas matches the document" is vacuous for a document that
  // came back with one node in it.
  expect(storedIds.length, 'the starter node and both added nodes must reach the stored document').toBe(3);

  await page.goto(BASE_URL + `/app/pipelines/latest/${id}`);
  await expect(page.getByRole('heading', { name, exact: true })).toBeVisible({ timeout: 15_000 });
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });

  // Polled: the canvas is seeded from the version fetch and laid out
  // afterwards, so a single read can land between the two and report a graph
  // that is merely not drawn yet.
  const expected = [...storedIds, 'END'].sort();
  await expect
    .poll(async () => [...(await canvasNodeIds(page))].sort(), {
      timeout: 20_000,
      message:
        'the reloaded canvas is not the stored graph — a node was dropped by the save, or drawn by the editor ' +
        'without being in the document the worker would compile',
    })
    .toEqual(expected);
});

test('J16: the Add-node menu offers exactly the node types the pipeline compiler admits', async ({ page }) => {
  /*
   * `Code` and `Custom` had no `parse_pipeline_node` arm
   * (`services/elitea-worker-rust/src/agents/graph/compiler.rs:1236`), so a
   * pipeline containing either was refused whole with "the pipeline contains
   * a node type that is not enabled" (`compiler.rs:1267`). They are withheld
   * from AUTHORING only — their renderers stay registered so stored
   * documents holding one still display.
   */
  const name = `${AUTOTEST_PREFIX}menu-${Date.now() % 1e9}`;
  await createPipelineThroughUi(page, name);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 15_000 });

  await page.getByRole('button', { name: 'Add node' }).click();
  const menu = page.getByRole('menu');
  await expect(menu).toBeVisible({ timeout: 10_000 });

  for (const label of ADMITTED_NODE_LABELS) {
    await expect(menu.getByRole('menuitem', { name: label, exact: true })).toBeVisible();
  }
  await expect(menu.getByRole('menuitem', { name: 'Code', exact: true })).toHaveCount(0);
  await expect(menu.getByRole('menuitem', { name: 'Custom', exact: true })).toHaveCount(0);
  // Nothing else either — the menu is exactly the compiler's allow-list.
  await expect(menu.getByRole('menuitem')).toHaveCount(ADMITTED_NODE_LABELS.length);
});

test('J16: every node type the menu offers mints a compiler-legal id', async ({ page }) => {
  /*
   * The measured defect. `getInitialNodeId` minted `` `${prefix} ${n}` `` —
   * "Agent 1", WITH A SPACE — and `valid_graph_id`
   * (`services/elitea-worker-rust/src/agents/graph/yaml.rs:362`) admits ASCII
   * alphanumerics plus `_ - . :` and nothing else. It is called on every
   * node's raw `id` (`application.rs:119`, `decision.rs:80`,
   * `direct_tool.rs:154`, `hitl.rs:150`, `router.rs:165`) and on
   * `entry_point` (`compiler.rs:464`) — and the editor sets `entry_point` to
   * the FIRST node added. So the first node a user added produced a document
   * the compiler refuses (`graph.pipeline.invalid_configuration`). The Python
   * SDK worker hid this by silently rewriting ids through `clean_string`; the
   * Rust worker never rewrites.
   *
   * This covers all nine admitted types. The three that cannot be SAVED
   * unconfigured (see SAVEABLE_NODE_LABELS) are only reachable here; the
   * stored-document gate is the test after this one.
   */
  test.slow();
  const name = `${AUTOTEST_PREFIX}mint-${Date.now() % 1e9}`;
  await createPipelineThroughUi(page, name);
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });

  // The canvas the starter opened on — the baseline every "added" count below
  // is taken against. See `canvasNodeIds`.
  const starterIds = await canvasNodeIds(page);

  for (const label of ADMITTED_NODE_LABELS) {
    await addNodeThroughMenu(page, label);
  }

  const canvasIds = await canvasNodeIds(page);
  // Exactly one node per menu item was added. Counted over the whole canvas,
  // `>= ADMITTED_NODE_LABELS.length` now counts the starter node and `END`
  // towards a total the MENU is supposed to have produced, so two menu items
  // could stop adding anything and the count would still pass.
  const addedIds = canvasIds.filter(nodeId => !starterIds.includes(nodeId));
  expect(addedIds.length, 'every menu item must add exactly one node').toBe(ADMITTED_NODE_LABELS.length);
  expect(canvasIds).not.toContain('Agent 1');
  for (const nodeId of canvasIds) {
    expect(nodeId, `minted node id "${nodeId}" is not addressable by the pipeline compiler`).toMatch(
      COMPILER_LEGAL_NODE_ID,
    );
  }
  // The Agent node specifically — the id in the defect report.
  expect(canvasIds).toContain('Agent_1');
});

test('J16: the STORED pipeline document carries only compiler-legal ids', async ({ page }) => {
  /*
   * The real gate. Everything above this line reads the canvas, which shows
   * whatever the editor's own store holds; only the persisted `instructions`
   * document is what the worker ever compiles. The previous version of this
   * journey asserted `.react-flow__node[data-id="Agent 1"]` and nothing else,
   * and stayed green for as long as the editor was minting an id no pipeline
   * could be built from.
   */
  test.slow();
  const name = `${AUTOTEST_PREFIX}ids-${Date.now() % 1e9}`;
  const id = await createPipelineThroughUi(page, name);
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });

  /*
   * The document the CREATE stored, before this journey adds anything. The
   * starter template is itself a stored graph now, so it goes through the same
   * grammar as anything the editor mints — and it is the baseline the added
   * count is taken against, because `ids.length >= SAVEABLE_NODE_LABELS.length`
   * over the whole document counts the starter's node towards a total the
   * editor is supposed to have produced.
   */
  const versionId = await resolveLatestPipelineVersionId(page.request, DEFAULT_PROJECT_ID, id);
  const before = await readStoredPipelineVersion(page.request, DEFAULT_PROJECT_ID, id, versionId);
  const idsBefore = storedNodeIds(parseStoredGraph(before.instructions));
  expect(idsBefore.length, 'a new pipeline must be created on the starter graph').toBeGreaterThan(0);
  for (const nodeId of idsBefore) {
    expect(nodeId, `starter node id "${nodeId}" is not addressable by the pipeline compiler`).toMatch(
      COMPILER_LEGAL_NODE_ID,
    );
  }

  for (const label of SAVEABLE_NODE_LABELS) {
    await addNodeThroughMenu(page, label);
  }
  await saveAndAwaitPersist(page);

  const stored = await readStoredPipelineVersion(page.request, DEFAULT_PROJECT_ID, id, versionId);
  const graph = parseStoredGraph(stored.instructions);

  // One node per label actually reached the backend — otherwise every id
  // assertion below would be vacuously true of the starter's own node.
  const ids = storedNodeIds(graph);
  const addedIds = ids.filter(nodeId => !idsBefore.includes(nodeId));
  expect(addedIds.length, 'every saveable node type must reach the stored document').toBe(
    SAVEABLE_NODE_LABELS.length,
  );

  for (const nodeId of ids) {
    expect(nodeId, `stored node id "${nodeId}" is not addressable by the pipeline compiler`).toMatch(
      COMPILER_LEGAL_NODE_ID,
    );
  }

  // `entry_point` goes through the same `valid_graph_id` check
  // (`compiler.rs:464`). It names the starter's node on a pipeline created
  // through this form, and the first node ADDED on a document that has none,
  // so it is the very first thing a space would break either way.
  expect(graph.entry_point, 'the stored graph must declare an entry point').toBeTruthy();
  expect(graph.entry_point as string).toMatch(COMPILER_LEGAL_NODE_ID);
  expect(ids).toContain(graph.entry_point as string);

  // The literal defect string, anywhere in the stored document — including
  // any transition, route target or Decision `nodes:` entry that a
  // space-separated id would have been written into.
  expect(
    stored.instructions.includes('Agent 1'),
    'the stored pipeline still contains the literal "Agent 1" — the compiler cannot address it',
  ).toBe(false);
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 25 (pipelines half): Unsaved-changes navigation block
// ─────────────────────────────────────────────────────────────────────────────
test('J25: unsaved-changes nav block: navigate away from a dirty pipeline → dialog → cancel → stay', async ({
  page,
}) => {
  /*
   * The `/pipelines` half of #133. The issue's agents half was measured (J25
   * in `agents.lifecycle.spec.ts`); the pipelines half was INFERENCE — the
   * identical "nav-blocking-when-dirty is dropped" disclosure sat in
   * `EditPipeline.tsx` with no test exercising it. This journey measures it.
   *
   * The dialog asserted on is the REAL production `NavBlockerDialog`
   * (`widgets/app-shell`), armed by `CreatePipeline.tsx`'s own
   * `useUnsavedChangesNavBlocker` call. Nothing here stubs the served bundle.
   */
  await page.goto(BASE_URL + '/app/pipelines/my');
  await page.waitForURL('**/pipelines**', { timeout: 15_000 });

  await clickCreateButton(page);
  await page.waitForURL('**/app/pipelines/create**', { timeout: 15_000 });

  const panel = page.getByTestId('create-pipeline-form-panel');
  const nameInput = panel.getByTestId('agent-name-input');
  await expect(nameInput).toBeVisible({ timeout: 10_000 });

  // Dirty the form.
  const dirtyName = `${AUTOTEST_PREFIX}dirty-pipe`;
  await nameInput.fill(dirtyName);

  // A real in-app link. A `goto()` fallback would bypass the router's blocker
  // entirely and pass against an app with no guard at all.
  await page.getByRole('link', { name: /chat/i }).first().click();

  const navBlockerDialog = page.getByRole('dialog');
  await expect(navBlockerDialog).toBeVisible({ timeout: 10_000 });
  await checkA11y(page);

  // Cancelling keeps us on the pipeline form with the typed value intact.
  await navBlockerDialog.getByRole('button', { name: /cancel|stay|no/i }).first().click();
  await expect(nameInput).toHaveValue(dirtyName);
  expect(page.url()).toContain('/pipelines');

  await checkA11y(page);
});
