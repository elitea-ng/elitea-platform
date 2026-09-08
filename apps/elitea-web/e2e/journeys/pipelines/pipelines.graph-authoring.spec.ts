/**
 * The EDGES. Authored by dragging between two handles on the real canvas, and
 * checked against the document the backend stored.
 *
 * ── The gap this closes ──────────────────────────────────────────────────
 *
 * Commit 58600cb0 made the pipeline editor author graphs the Rust runtime
 * accepts, and its keystone journey (`e2e/streaming/chat.pipeline-authored.
 * spec.ts`) drives the Add-node menu and runs the result against a live
 * model. That graph has exactly ONE node. Nothing anywhere authored an EDGE:
 * every other pipeline journey adds nodes and asserts on ids, admission or a
 * save round trip.
 *
 * Meanwhile the edge -> YAML conversion was fully built and unit-tested —
 * `lib/flow-editor/helpers/connectionOperations.helpers.ts`,
 * `lib/flow-editor/hooks/useConnectNodes.ts`, and `onConnect` in
 * `ui/FlowEditor.tsx` — with no proof that a real drag on the canvas ever
 * reaches any of it. That is this repository's dominant defect class: code
 * that is present, unit-tested and has no working caller (#126/#129/#134/
 * #136/#138/#149). So none of it was assumed here; every claim below is a
 * read of the STORED `instructions` document.
 *
 * ── Why not a screen assertion ───────────────────────────────────────────
 *
 * `.react-flow__edge[data-id=…]` would appear the moment `setFlowEdges` ran,
 * whether or not `updateYamlNode` wrote anything — the canvas renders the
 * editor's own edge array, and the YAML document is a SEPARATE piece of
 * state written by a different helper. A canvas edge is used below only as a
 * fast-failing diagnostic (a drag that missed its target says so where it
 * happened rather than three steps later); it is never the evidence.
 *
 * ── What "connect" costs on this canvas, measured ────────────────────────
 *
 * A node card is 29.4375rem wide and, expanded, about 800px tall in flow
 * units; the Add-node menu drops each new node 60px right and 60px down from
 * the last (`calculatePositionForNewNode`). Two expanded LLM cards therefore
 * OVERLAP almost completely, and the first one's source handle — bottom
 * centre — sits underneath the second card, which is also the selected node
 * and so has the higher z-index. On top of that the editor opens at zoom 2
 * after its initial `fitView` over the lone END node, which puts the bottom
 * of a card ~1000px below the fold.
 *
 * `compactCanvas` is what a user does about that, through the editor's own
 * two controls: "Toggle cards size" collapses every card to its header AND
 * runs the dagre auto-layout (`FlowEditor.tsx`'s `onExpandAll`), which never
 * overlaps; "Fit View" then brings the whole graph on screen. After it, both
 * handles are visible, ~12px, and about 550px apart.
 *
 * The drag itself is a REAL mouse path — `mouse.down`, four `mouse.move`s,
 * `mouse.up`. @xyflow/system's `onPointerDown` (12.11.2) listens for plain
 * `mousemove`/`mouseup` on the document and snaps to the closest handle
 * within `connectionRadius`, so Playwright's synthesised mouse drives it the
 * same way a hand does. No `dispatchEvent`, no store poke, no keyboard
 * substitute.
 */
import { expect, test, type Locator, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';
import { checkA11y } from '../../fixtures/axe';
import { parseStoredGraph, readStoredPipelineVersion, storedNodeIds } from '../../fixtures/pipelines';

/** The create POST carries the project id; nothing here assumes project 1 (the chat persona owns its own — #290). */
const APPLICATIONS_RE = /\/elitea_core\/applications\/prompt_lib\/(\d+)/;

/** One pipeline created through the real form, addressed by everything the stored-document reads need. */
interface CreatedPipeline {
  readonly projectId: string;
  readonly pipelineId: string;
  readonly versionId: string;
}

/** Pipelines minted by the tests below, removed in `afterEach` whatever happened. */
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

/**
 * Create a pipeline through its own form and read the ids off the RESPONSE.
 *
 * The project id comes from the POST's own URL rather than `DEFAULT_PROJECT_ID`:
 * a persona that owns a personal project is switched into it by the app, and a
 * hardcoded `1` then reads a pipeline that does not exist there (measured on
 * this stack — `application not found`, on a pipeline that had just saved).
 */
async function createPipeline(page: Page, name: string): Promise<CreatedPipeline> {
  const response = page.waitForResponse(
    request => APPLICATIONS_RE.test(new URL(request.url()).pathname) && request.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await page.goto(`${BASE_URL}/app/pipelines/create`);
  await expect(page.getByTestId('agent-name-input')).toBeVisible({ timeout: 30_000 });
  // 32 characters is the form's own `maxLength`; a longer name is truncated.
  await page.getByTestId('agent-name-input').fill(name);
  await page.getByTestId('agent-description-input').fill(`${name} description`);
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('pipeline-save-button').click();

  const resolved = await response;
  expect(resolved.status(), `the pipeline must be created: ${(await resolved.text()).slice(0, 300)}`).toBe(201);
  const body = (await resolved.json()) as { id?: string; version_details?: { id?: string; agent_type?: string } };
  const pipeline: CreatedPipeline = {
    projectId: APPLICATIONS_RE.exec(new URL(resolved.url()).pathname)?.[1] ?? '',
    pipelineId: String(body.id ?? ''),
    versionId: String(body.version_details?.id ?? ''),
  };
  expect(pipeline.projectId, 'the pipeline must belong to a project').not.toBe('');
  expect(pipeline.pipelineId, 'the created pipeline must carry an id').toMatch(/^\d+$/);
  expect(pipeline.versionId, 'the created pipeline must carry a version, or the stored read joins nothing').not.toBe('');
  expect(body.version_details?.agent_type, 'the create page must store a PIPELINE row').toBe('pipeline');
  created.push(pipeline);
  return pipeline;
}

/**
 * Wait for the canvas and give it the width, through the editor's own two
 * collapse affordances.
 *
 * Not defensive padding: with the configuration panel open a node added at the
 * viewport centre lands UNDER it, and Playwright reports
 * `<div data-testid="edit-pipeline-configuration-form-gap"> … intercepts
 * pointer events` on every retry — the same measurement
 * `chat.pipeline-authored.spec.ts` records. React Flow's canvas is a
 * transformed surface, so no amount of scrolling moves a node out from under
 * an overlay.
 */
async function openCanvas(page: Page): Promise<void> {
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 20_000 });
  await page.getByRole('button', { name: 'Collapse the configuration panel' }).click();
  await expect(page.getByRole('button', { name: 'Expand the configuration panel' })).toBeVisible({ timeout: 10_000 });
  await page.getByRole('button', { name: 'Collapse the chat panel' }).click();
  await expect(page.getByRole('button', { name: 'Expand the chat panel' })).toBeVisible({ timeout: 10_000 });
}

/** Add one node of `label` through the editor's own menu — no store pokes. */
async function addNode(page: Page, label: string): Promise<void> {
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: label, exact: true }).click();
}

/**
 * Block until the canvas geometry stops moving.
 *
 * "Toggle cards size" re-layouts on a 200ms timer and then calls `fitView` on
 * another 100ms one (`FlowEditor.tsx`'s `onExpandAll`), and the collapse
 * itself changes every card's measured height afterwards. A `boundingBox()`
 * read in the middle of that returns a position the handle no longer occupies
 * by the time the mouse gets there — which fails as "the connection was
 * refused" and says nothing about the timing. Two identical consecutive
 * samples is the cheapest honest settle.
 */
async function settleCanvas(page: Page): Promise<void> {
  const seen: string[] = [];
  await expect
    .poll(
      async () => {
        const current = await page.evaluate(() => {
          const viewport = document.querySelector('.react-flow__viewport');
          const transform = viewport instanceof HTMLElement ? viewport.style.transform : '';
          const nodes = [...document.querySelectorAll('.react-flow__node')]
            .map(node => {
              const box = node.getBoundingClientRect();
              return `${node.getAttribute('data-id') ?? ''}@${Math.round(box.x)},${Math.round(box.y)},${Math.round(box.height)}`;
            })
            .join('|');
          return `${transform}#${nodes}`;
        });
        seen.push(current);
        // THREE identical samples, not two. A save re-seeds the editor from
        // the refetched version, and that arrives some hundreds of
        // milliseconds after the PUT resolves — two samples 250ms apart can
        // both land in the gap before it and call a moving canvas settled.
        const window = seen.slice(-3);
        return window.length === 3 && window[0] !== '' && new Set(window).size === 1;
      },
      { timeout: 30_000, intervals: [250], message: 'the canvas never stopped moving' },
    )
    .toBe(true);
}

/**
 * Collapse the cards (which also auto-arranges) and fit the graph on screen —
 * see the module header.
 *
 * Call it AFTER any save, never before a drag that follows one: saving makes
 * the page refetch the version and re-seed the editor's nodes from it, which
 * throws away the fitted viewport and puts the cards back where the stored
 * layout says (measured — the canvas came back showing only END, with both LLM
 * cards below the fold, and the drag then pressed the mouse down on nothing).
 */
async function compactCanvas(page: Page): Promise<void> {
  await settleCanvas(page);
  await page.getByRole('button', { name: 'Toggle cards size' }).click();
  await settleCanvas(page);
  await fitCanvas(page);
}

/**
 * Bring the whole graph back on screen, without touching the card size.
 *
 * Needed after every save for the reason above, and it is not optional on
 * WebKit: `locator.click()` there refuses an element outside the viewport
 * outright ("element is outside of the viewport", retried until the test timed
 * out), where Chromium had scrolled to it. The canvas cannot be scrolled — it
 * is a transformed surface — so the fix is the editor's own Fit View.
 */
async function fitCanvas(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Fit View' }).click();
  await settleCanvas(page);
}

function handleOf(page: Page, nodeId: string, handleId: string): Locator {
  return page.locator(`.react-flow__node[data-id="${nodeId}"] .react-flow__handle[data-handleid="${handleId}"]`);
}

interface Connection {
  readonly source: string;
  readonly sourceHandle: string;
  readonly target: string;
  readonly targetHandle: string;
}

/**
 * The centre of a handle, checked to be INSIDE the canvas.
 *
 * `toBeVisible()` is not enough and the difference is the whole reason this
 * exists: React Flow draws its nodes in a CSS-transformed container, so a node
 * scrolled far below the fold still has a non-zero box and still reports
 * visible. Pressing the mouse down at that coordinate silently does nothing,
 * and the failure surfaces later as "the conversion is broken". Measured
 * exactly that way here after a save re-seeded the canvas.
 */
async function handleCentre(
  handle: Locator,
  canvas: { x: number; y: number; width: number; height: number } | null,
  label: string,
): Promise<readonly [number, number]> {
  expect(canvas, 'the flow canvas must have a box').not.toBeNull();
  const box = await handle.boundingBox();
  expect(box, `${label} must have a box on screen`).not.toBeNull();
  const area = canvas as NonNullable<typeof canvas>;
  const rect = box as NonNullable<typeof box>;
  const x = rect.x + rect.width / 2;
  const y = rect.y + rect.height / 2;
  expect(
    x >= area.x && x <= area.x + area.width && y >= area.y && y <= area.y + area.height,
    `${label} sits at (${Math.round(x)}, ${Math.round(y)}), outside the canvas ` +
      `(${Math.round(area.x)}, ${Math.round(area.y)}) ${Math.round(area.width)}x${Math.round(area.height)} — ` +
      'the graph is not fitted on screen, so no mouse gesture can reach it',
  ).toBe(true);
  return [x, y];
}

/**
 * Drag a connection between two handles with the real mouse.
 *
 * The `connectable` assertion is not decoration. A handle React Flow considers
 * unconnectable swallows the whole gesture SILENTLY — no edge, no error, no
 * YAML write — and the test would then fail on a stored-document assertion
 * that reads as "the conversion is broken". Measured here on a Router's
 * `default_output` handle, which stops being connectable the moment
 * `default_output` is set (and it is seeded to `END`).
 */
async function connectHandles(page: Page, connection: Connection): Promise<void> {
  const source = handleOf(page, connection.source, connection.sourceHandle);
  const target = handleOf(page, connection.target, connection.targetHandle);
  await expect(source, `${connection.source}/${connection.sourceHandle} must be visible to drag from`).toBeVisible({
    timeout: 15_000,
  });
  await expect(
    source,
    `${connection.source}/${connection.sourceHandle} is not connectable — React Flow will swallow the drag`,
  ).toHaveClass(/(^|\s)connectable(\s|$)/);
  await expect(target, `${connection.target}/${connection.targetHandle} must be visible to drop on`).toBeVisible({
    timeout: 15_000,
  });

  const canvas = await page.getByTestId('rf__wrapper').boundingBox();
  const [fromX, fromY] = await handleCentre(source, canvas, `${connection.source}/${connection.sourceHandle}`);
  const [toX, toY] = await handleCentre(target, canvas, `${connection.target}/${connection.targetHandle}`);

  await page.mouse.move(fromX, fromY);
  await page.mouse.down();
  // The first move must clear `dragThreshold` (1px) for `startConnection` to
  // run at all; the rest are there because a single jump to the target leaves
  // React Flow one `mousemove` to compute the closest handle from.
  await page.mouse.move(fromX + 6, fromY + 6, { steps: 3 });
  await page.mouse.move((fromX + toX) / 2, (fromY + toY) / 2, { steps: 10 });
  await page.mouse.move(toX, toY, { steps: 10 });
  await page.mouse.up();

  // DIAGNOSTIC ONLY, and deliberately labelled as such: the canvas renders
  // `flowEdges`, which `applyEdgeChanges` writes whether or not the YAML
  // helper wrote anything. It proves the gesture landed on the right handle,
  // nothing more. The evidence is the stored document each test reads next.
  await expect(
    page.locator(`.react-flow__edge[data-id*="${connection.source}"][data-id*="${connection.target}"]`),
    `the drag from ${connection.source} to ${connection.target} produced no edge on the canvas at all`,
  ).toHaveCount(1, { timeout: 10_000 });
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
  // disabled for as long as the live graph is one the runtime would refuse, so
  // a graph broken by an edit fails HERE, naming the button, rather than
  // silently never being stored.
  await expect(
    page.getByTestId('pipeline-save-button'),
    'Save is disabled — the editor considers the current graph inadmissible',
  ).toBeEnabled({ timeout: 20_000 });
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  // A failed save renders a role="alert" banner (`EditPipeline.tsx`); there is
  // no toast infrastructure.
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

/** The parsed graph the BACKEND holds — the only thing any assertion below trusts. */
async function storedGraph(page: Page, pipeline: CreatedPipeline): Promise<ReturnType<typeof parseStoredGraph>> {
  const stored = await readStoredPipelineVersion(page.request, pipeline.projectId, pipeline.pipelineId, pipeline.versionId);
  return parseStoredGraph(stored.instructions);
}

/** One node out of a stored graph, by id — with a failure message that names what was there instead. */
function storedNode(
  graph: ReturnType<typeof parseStoredGraph>,
  id: string,
): Readonly<Record<string, unknown>> {
  const node = graph.nodes.find(candidate => String(candidate['id']) === id);
  expect(node, `the stored graph holds no node "${id}" — it holds ${JSON.stringify(storedNodeIds(graph))}`).toBeTruthy();
  return node as Readonly<Record<string, unknown>>;
}

test('a connection dragged between two handles is stored as a `transition`', async ({ page }) => {
  test.setTimeout(180_000);
  const pipeline = await createPipeline(page, `${AUTOTEST_PREFIX}edge-${Date.now() % 1e6}`);
  await openCanvas(page);
  /*
   * Spec §6.4's one call per distinct screen. The state is not the one
   * `pipelines.lifecycle.spec.ts`'s a11y test covers — that one runs with both
   * side panels EXPANDED, and this runs with both collapsed to their rails,
   * where the only remaining controls are the icon-only collapse toggles that
   * #135 had to give `aria-label`s.
   *
   * It says nothing about the canvas itself: `checkA11y` excludes the whole
   * `.react-flow` subtree, which is also why the two custom canvas control
   * buttons went unnamed until this change — see
   * `features/pipelines/ui/FlowEditorCanvasControls.tsx`.
   */
  await checkA11y(page);

  await addNode(page, 'LLM');
  await addNode(page, 'LLM');
  await expect(page.locator('.react-flow__node[data-id="LLM_2"]')).toBeVisible({ timeout: 20_000 });

  // Before: the two nodes are in the document and NEITHER points at the other.
  // Without this the assertion after the drag could be satisfied by a default
  // the editor happened to seed. This save comes BEFORE `compactCanvas` on
  // purpose — see that helper's own note on what a save does to the viewport.
  await saveAndAwaitPersist(page);
  const before = await storedGraph(page, pipeline);
  expect(storedNodeIds(before)).toEqual(expect.arrayContaining(['LLM_1', 'LLM_2']));
  expect(
    storedNode(before, 'LLM_1')['transition'],
    'a freshly added LLM node must start pointing at END, or the assertion after the drag proves nothing',
  ).toBe('END');

  await compactCanvas(page);
  await connectHandles(page, { source: 'LLM_1', sourceHandle: 'source', target: 'LLM_2', targetHandle: 'target' });
  await saveAndAwaitPersist(page);

  // THE ASSERTION. A `transition` naming the second node, in the document the
  // worker would compile — written by `handleNormalConnection`'s
  // `updateYamlNode(yamlNode.id, 'transition', connection.target, …)`, reached
  // from `onConnect` and from nowhere else.
  const after = await storedGraph(page, pipeline);
  expect(
    storedNode(after, 'LLM_1')['transition'],
    'the dragged connection never reached the stored document — the canvas showed an edge the YAML never got',
  ).toBe('LLM_2');
  // The other end is untouched: a connection writes the SOURCE's transition,
  // and a helper that wrote both would pass the line above while corrupting
  // the graph.
  expect(storedNode(after, 'LLM_2')['transition'], 'the target node must keep its own transition').toBe('END');
  expect(after.entry_point, 'the entry point is still the first node added').toBe('LLM_1');
});

test('deleting a transition’s target repairs the dangling transition in the stored document', async ({ page }) => {
  /*
   * `deletionOperations.helpers.ts`'s `cleanupNodeReferences` claims to clear
   * every field matching the deleted node's id, and `handleEdgeFromNormalNode`
   * claims to re-point the source at END. Both run only when the deletion
   * actually queues the connected EDGE — which is why this drives the `Delete`
   * key, the one path that used not to queue it (see `useDeleteItems.ts`'s
   * `useDeleteKeyTrigger` doc comment for the measurement: it left
   * `transition: ''`, and the editor's own admission gate then refused to save
   * the pipeline at all).
   */
  test.setTimeout(180_000);
  const pipeline = await createPipeline(page, `${AUTOTEST_PREFIX}del-${Date.now() % 1e6}`);
  await openCanvas(page);

  await addNode(page, 'LLM');
  await addNode(page, 'LLM');
  await expect(page.locator('.react-flow__node[data-id="LLM_2"]')).toBeVisible({ timeout: 20_000 });
  await compactCanvas(page);
  await connectHandles(page, { source: 'LLM_1', sourceHandle: 'source', target: 'LLM_2', targetHandle: 'target' });
  await saveAndAwaitPersist(page);
  expect(
    storedNode(await storedGraph(page, pipeline), 'LLM_1')['transition'],
    'the transition must be stored before this test can prove anything about removing it',
  ).toBe('LLM_2');

  // Select the transition's TARGET and delete it, through the canvas. The
  // refit is what makes the node clickable after the save re-seeded the
  // editor — see `fitCanvas`.
  await fitCanvas(page);
  await page.locator('.react-flow__node[data-id="LLM_2"]').click();
  await page.keyboard.press('Delete');
  const confirm = page.getByRole('dialog');
  await expect(confirm).toBeVisible({ timeout: 10_000 });
  await expect(confirm.getByText('LLM_2', { exact: false })).toBeVisible();
  await confirm.getByRole('button', { name: 'Remove' }).click();
  await expect(page.locator('.react-flow__node[data-id="LLM_2"]')).toHaveCount(0, { timeout: 10_000 });

  await saveAndAwaitPersist(page);

  const after = await storedGraph(page, pipeline);
  expect(storedNodeIds(after), 'the deleted node must be gone from the stored document').not.toContain('LLM_2');
  // The point of the test: no reference to the deleted node survives ANYWHERE
  // in the document, and what replaces it is a target the runtime accepts —
  // `END` — not the empty string, which `graphAdmission`'s `node.route-target`
  // rule (`router.rs:331`) refuses.
  expect(
    storedNode(after, 'LLM_1')['transition'],
    'LLM_1 still points somewhere the runtime cannot resolve after its target was deleted',
  ).toBe('END');
});

test('a Router’s branch handle is stored as a route list, not as a transition', async ({ page }) => {
  /*
   * The third YAML shape `connectionOperations.helpers.ts` writes. A plain
   * node gets `transition`; a Router gets `routes[]` (or `default_output`,
   * from its second source handle); a Decision gets `nodes[]`. Router and
   * Decision are both covered here — the two that mint a route LIST, which is
   * where an "append" that silently replaced would go unnoticed.
   *
   * The Router's `default_output` handle is deliberately not dragged from: it
   * is seeded to `END` (`nodeDefaults.constants.ts`, because `''` fails the
   * runtime's `validate_target`), and `isDefaultConnectable` therefore reports
   * it unconnectable for as long as that edge exists. Dragging from it is a
   * silent no-op — measured — which is exactly what `connectHandles`'s
   * `connectable` assertion exists to refuse.
   */
  test.setTimeout(180_000);
  const pipeline = await createPipeline(page, `${AUTOTEST_PREFIX}branch-${Date.now() % 1e6}`);
  await openCanvas(page);

  await addNode(page, 'Router');
  await addNode(page, 'Decision');
  await addNode(page, 'LLM');
  await addNode(page, 'Printer');
  await expect(page.locator('.react-flow__node[data-id="Printer_1"]')).toBeVisible({ timeout: 20_000 });

  // Before, from the stored document — a route list that is already populated
  // would make both assertions below vacuous. Saved before `compactCanvas` for
  // the reason that helper documents.
  await saveAndAwaitPersist(page);
  const empty = await storedGraph(page, pipeline);
  expect(storedNode(empty, 'Router_1')['routes'], 'a fresh Router starts with no routes').toEqual([]);
  expect(storedNode(empty, 'Decision_1')['nodes'], 'a fresh Decision starts with no branches').toEqual([]);

  await compactCanvas(page);
  await connectHandles(page, {
    source: 'Router_1',
    sourceHandle: 'routerNode_routes',
    target: 'LLM_1',
    targetHandle: 'target',
  });
  await connectHandles(page, {
    source: 'Decision_1',
    sourceHandle: 'nodes',
    target: 'Printer_1',
    targetHandle: 'target',
  });
  await saveAndAwaitPersist(page);

  const after = await storedGraph(page, pipeline);
  const router = storedNode(after, 'Router_1');
  expect(router['routes'], 'the Router’s branch handle must append its target to `routes`').toEqual(['LLM_1']);
  expect(router['default_output'], 'the Router’s default output is untouched by a `routes` connection').toBe('END');
  expect(router['transition'], 'a Router must not grow a plain `transition`').toBeUndefined();

  const decision = storedNode(after, 'Decision_1');
  expect(decision['nodes'], 'the Decision’s branch handle must append its target to `nodes`').toEqual(['Printer_1']);
  expect(decision['default_output'], 'the Decision’s default output is untouched by a branch connection').toBe('END');
});

/*
 * Ported BY USE CASE from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/pipelines/
 * test_pipeline_nodes.py::TestAddNode::
 * test_add_human_in_the_loop_node_and_connect_to_end`) — "add a HITL node and
 * connect its approve handle to END".
 *
 * It extends this file because this is where the connection machinery lives,
 * and it is the FOURTH YAML shape `connectionOperations.helpers.ts` writes:
 * a plain node gets `transition`, a Router gets `routes[]`, a Decision gets
 * `nodes[]`, and a HITL gets a route MAP keyed by the handle that was dragged
 * (`handleFromHitlNodeConnection`). J16's node-type coverage
 * (`pipelines.lifecycle.spec.ts`) proves a HITL node can be added and saved;
 * nothing proved one of its three branch handles writes anything.
 *
 * ── Why this connects to a Printer and not to END ────────────────────────
 *
 * The legacy test drags approve → END, which is a NO-OP on this platform and
 * would make the assertion vacuous: `nodeDefaults.constants.ts` seeds
 * `routes: {approve: 'END', reject: 'END'}` on every freshly added HITL node,
 * and its own doc comment records why — the original `approve: ''` seed
 * failed the runtime's `validate_target` (`hitl.rs:464-468`) and rejected the
 * whole document on the first save. So the route already points at END before
 * anything is dragged. Connecting it somewhere ELSE is the same use case
 * (a HITL branch handle wires into the graph) stated so that only a working
 * conversion can satisfy it.
 *
 * `edit` is deliberately not dragged: `rejectHitlConnection` refuses an
 * `edit` route to END outright, and any other target renders the node card's
 * "Provide an edit state key before using the Edit route." error until a
 * state key is picked — a second control, and a different journey.
 */
test('a HITL branch handle is stored as a route map entry, not as a transition', async ({ page }) => {
  test.setTimeout(180_000);
  const pipeline = await createPipeline(page, `${AUTOTEST_PREFIX}hitl-${Date.now() % 1e6}`);
  await openCanvas(page);

  await addNode(page, 'Human-in-the-loop');
  await addNode(page, 'Printer');
  await expect(page.locator('.react-flow__node[data-id="Printer_1"]')).toBeVisible({ timeout: 20_000 });

  // Before, from the STORED document. Saved before `compactCanvas` for the
  // reason that helper documents. Both seeded routes are asserted, because
  // the point of the drag below is that it moves ONE of them.
  await saveAndAwaitPersist(page);
  const before = await storedGraph(page, pipeline);
  expect(
    storedNode(before, 'HITL_1')['routes'],
    'a freshly added HITL node must be seeded with both routes at END — see `nodeDefaults.constants.ts`',
  ).toEqual({ approve: 'END', reject: 'END' });

  await compactCanvas(page);
  await connectHandles(page, {
    source: 'HITL_1',
    sourceHandle: 'hitlNode_approve',
    target: 'Printer_1',
    targetHandle: 'target',
  });
  await saveAndAwaitPersist(page);

  const after = await storedGraph(page, pipeline);
  const hitl = storedNode(after, 'HITL_1');
  expect(
    hitl['routes'],
    'the dragged approve route never reached the stored document — the canvas showed an edge the YAML never got, ' +
      'and the other branch must be untouched by it',
  ).toEqual({ approve: 'Printer_1', reject: 'END' });
  // A HITL node routes; it does not transition. `handleFromHitlNodeConnection`
  // clears `transition` as part of the same write, and a node carrying both
  // is one the compiler reads differently from the graph on screen.
  expect(hitl['transition'], 'a HITL node must not grow a plain `transition`').toBeUndefined();
  // The target end is untouched: a connection writes the SOURCE's routes, and
  // a helper that wrote both would pass the line above while corrupting the
  // graph.
  expect(storedNode(after, 'Printer_1')['transition'], 'the target node must keep its own transition').toBe('END');
});
