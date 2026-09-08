/**
 * A pipeline with TWO nodes and a real EDGE between them, authored entirely on
 * the canvas and then run against the live model.
 *
 * ── Why this exists next to `chat.pipeline-authored.spec.ts` ─────────────
 *
 * That file is the keystone for "a graph a user built in the editor actually
 * runs", and it proves it with ONE node. One node needs no `transition`: the
 * entry point runs, writes its reply into `messages`, and the terminal result
 * policy picks it up. So the whole edge → YAML path
 * (`connectionOperations.helpers.ts`, `useConnectNodes.ts`, `onConnect` in
 * `FlowEditor.tsx`) was unit-tested, wired, and never once executed by a test
 * that then RAN the result.
 *
 * `e2e/journeys/pipelines/pipelines.graph-authoring.spec.ts` closes half of
 * that: it drags between two handles and reads `transition: LLM_2` back out of
 * the stored document. But a stored `transition` is a string in a YAML file.
 * It says nothing about whether the compiler builds an edge from it, or
 * whether control actually reaches the second node — and a graph whose second
 * node never runs answers exactly like a one-node graph, because the first
 * node's reply is already in `messages`. That is what this file is for: if the
 * edge were decorative, everything else would still look right.
 *
 * ── The discriminator ────────────────────────────────────────────────────
 *
 * `attrs.response_metadata.tool_name` on the stored execution-trace steps must
 * name BOTH node ids, and the first node's step must come BEFORE the second
 * node's. Following `chat.pipeline-authored.spec.ts`'s own step 7, which
 * measured what that key holds on each runtime: a pipeline node id under the
 * native runtime, the literal langgraph name `"agent"` under the SDK worker,
 * and `attrs: {}` on the direct-agent fall-through. Two DISTINCT ids in order
 * cannot be produced by any of those.
 *
 * The ids are renamed to strings no model would emit and no default would
 * mint, through the node card's own rename box, and are asserted against the
 * STORED graph first — so a failure at the trace step cannot be blamed on a
 * save that never carried them.
 *
 * ── One quirk of the authoring order, stated rather than hidden ──────────
 *
 * Each node is configured immediately after it is added, while it is the
 * TOPMOST card. It has to be: the Add-node menu drops each new node only 60px
 * right and down from the last (`calculatePositionForNewNode`), a card is
 * 29.4375rem wide and ~800px tall, and the newest node is the selected one and
 * therefore on top — so the previous node's body controls are covered and
 * Playwright reports them as intercepting pointer events.
 *
 * A side effect of that order: because the first node has already been renamed
 * away from `LLM_1` by the time the second is added, `getInitialNodeId` finds
 * `LLM_1` free again and mints it for the SECOND node. That is correct
 * behaviour (the counter skips taken ids, and none is taken), and it is why
 * the constant below is named after the ORDER the nodes were added rather than
 * after the id they were minted with.
 */
import { expect, test, type Locator, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { AUTOTEST_PREFIX, expectStoredAssistantAnswer, fillComposer } from '../fixtures/api';
import { COMPILER_LEGAL_NODE_ID, parseStoredGraph, readStoredPipelineVersion } from '../fixtures/pipelines';

const APPLICATIONS_RE = /\/elitea_core\/applications\/prompt_lib\/(\d+)/;
/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/**
 * The two ids this journey drives the canvas to mint, and the strings the
 * whole file rests on.
 *
 * Both satisfy the compiler's `valid_graph_id` (ASCII alphanumerics plus
 * `_ - . :`, ≤128 bytes — `services/elitea-worker-rust/src/agents/graph/
 * yaml.rs`), which is also what the rename box enforces
 * (`NodeCardHeader.tsx`'s `findRenameRejection`). Deliberately not English
 * words and not `LLM_1`/`LLM_2`: the trace assertion must be impossible to
 * satisfy by accident.
 */
const FIRST_NODE_ID = 'autotest_ui_multinode_head';
const SECOND_NODE_ID = 'autotest_ui_multinode_tail';

/** The id `getInitialNodeId` mints for the first LLM node added to an empty canvas — and, for the reason in the header, for the second one too. */
const MINTED_LLM_NODE_ID = 'LLM_1';

/**
 * Rename a canvas node through its own card — double-click the name, type,
 * blur.
 *
 * The blur is what commits (`NodeCardHeader`'s `onBlur`), so `Tab` is not
 * cosmetic. Re-keying the react-flow node is itself the evidence the commit
 * landed: the rename rewrites the node id, `entry_point`, and any transition
 * pointing at the old one.
 */
async function renameNodeThroughCard(page: Page, fromId: string, toId: string): Promise<void> {
  const node = page.locator(`.react-flow__node[data-id="${fromId}"]`);
  await expect(node).toBeVisible({ timeout: 20_000 });
  await node.getByText(fromId, { exact: true }).dblclick();
  const nameInput = node.locator('input').first();
  await expect(nameInput).toBeVisible({ timeout: 10_000 });
  await nameInput.fill(toId);
  await nameInput.press('Tab');
  await expect(page.locator(`.react-flow__node[data-id="${toId}"]`)).toBeVisible({ timeout: 10_000 });
  await expect(page.locator(`.react-flow__node[data-id="${fromId}"]`)).toHaveCount(0);
}

/**
 * Open a `SingleSelect` that lives INSIDE a React Flow node, by keyboard.
 *
 * A `click()` on these two dropdowns focuses them and does NOT open the menu —
 * the canvas handles mouse-down on a node itself (React Flow's drag layer),
 * and the escape hatch is the `nodrag` class, which every sibling field on
 * this card carries and `SimpleLLMInputItem`'s two dropdowns do not. That is a
 * finding about the app, not about Playwright: a real user cannot open them
 * with the mouse either. It is measured and recorded in
 * `chat.pipeline-authored.spec.ts`'s own helper; this file drives the surface
 * as it is rather than reaching across into another unit's files to change it.
 *
 * `focus()` dispatches no mouse event, and MUI's `Select` opens on Enter. The
 * options render in a portal at the document root, outside the canvas, so
 * clicking THEM is fine.
 */
async function openSelect(select: Locator): Promise<void> {
  await select.focus();
  await select.press('Enter');
  await expect(select.page().getByRole('listbox')).toBeVisible({ timeout: 10_000 });
}

/**
 * Point one LLM node's `task` prompt slot at the turn's own text, through the
 * node card's own two dropdowns.
 *
 * NOT optional, and not padding. A freshly added LLM node does not carry an
 * EMPTY `input_mapping`: `useLLMInputMapping` seeds
 * `{system: fixed "", task: fixed "", chat_history: fixed []}` the moment the
 * card mounts, and the worker's `validate_input_mapping` only falls back to
 * `messages -> messages` when the mapping is ABSENT. So what it sees is a
 * fixed, EMPTY task — which `LlmExecutionInput::resolve` refuses outright
 * (`task_text.is_empty()`), failing the node at `stage="input_mapping"` with
 * `pipeline.llm_node.failed` and storing an `is_error` row with no content.
 * Both nodes need it, and BOTH read `input`: it is a builtin seeded with the
 * turn's own text and nothing consumes it, so the second node reads it just as
 * the first did.
 *
 * The rows are `system`, `task`, `chat_history` in that order —
 * `getDefaultLLMInputMapping`'s own key order, which `SimpleLLMInputs`
 * iterates. Picking the row by index is what the DOM allows (the heading chip
 * is a sibling of the field row, not an ancestor), so the STORED mapping is
 * asserted after the save rather than trusted.
 */
async function mapTaskToTurnInput(page: Page, nodeId: string): Promise<void> {
  const node = page.locator(`.react-flow__node[data-id="${nodeId}"]`);
  const typeSelects = node.getByRole('combobox', { name: 'Type' });
  await expect(typeSelects).toHaveCount(3, { timeout: 20_000 });
  await openSelect(typeSelects.nth(1));
  await page.getByRole('option', { name: 'Variable', exact: true }).click();

  // Only the `task` row is a variable on this card now, so this names exactly
  // one field.
  const valueSelect = node.getByRole('combobox', { name: 'Value' });
  await expect(valueSelect).toHaveCount(1, { timeout: 10_000 });
  await openSelect(valueSelect);
  await page.getByRole('option', { name: 'input', exact: true }).click();
}

/**
 * Block until the canvas geometry stops moving — three identical consecutive
 * samples. See `e2e/journeys/pipelines/pipelines.graph-authoring.spec.ts`'s
 * copy of this helper for the full account of why a save makes it necessary.
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
        const window = seen.slice(-3);
        return window.length === 3 && window[0] !== '' && new Set(window).size === 1;
      },
      { timeout: 30_000, intervals: [250], message: 'the canvas never stopped moving' },
    )
    .toBe(true);
}

/**
 * Collapse the cards (which also runs the dagre auto-arrange) and fit the
 * graph on screen, through the editor's own two controls.
 *
 * Two expanded LLM cards overlap almost completely and hang ~1000px below the
 * fold at the editor's opening zoom, which puts the first node's source handle
 * under the second card and off screen. Collapsed and auto-arranged they sit
 * side by side with both handles reachable.
 */
async function compactCanvas(page: Page): Promise<void> {
  await settleCanvas(page);
  await page.getByRole('button', { name: 'Toggle cards size' }).click();
  await settleCanvas(page);
  await page.getByRole('button', { name: 'Fit View' }).click();
  await settleCanvas(page);
}

/**
 * Drag a connection between two handles with the real mouse.
 *
 * @xyflow/system's `onPointerDown` (12.11.2) listens for plain
 * `mousemove`/`mouseup` on the document and snaps to the closest handle within
 * `connectionRadius`, so a synthesised mouse drives it exactly as a hand does.
 * The first move has to clear the 1px `dragThreshold` before `startConnection`
 * runs at all.
 */
async function connectHandles(page: Page, source: string, target: string): Promise<void> {
  const sourceHandle = page.locator(`.react-flow__node[data-id="${source}"] .react-flow__handle[data-handleid="source"]`);
  const targetHandle = page.locator(`.react-flow__node[data-id="${target}"] .react-flow__handle[data-handleid="target"]`);
  // A handle React Flow considers unconnectable swallows the whole gesture
  // silently — no edge, no error, no YAML write.
  await expect(sourceHandle, `${source}'s source handle is not connectable`).toHaveClass(/(^|\s)connectable(\s|$)/);

  const canvas = await page.getByTestId('rf__wrapper').boundingBox();
  const from = await sourceHandle.boundingBox();
  const to = await targetHandle.boundingBox();
  expect(canvas, 'the flow canvas must have a box').not.toBeNull();
  expect(from, `${source}'s source handle must have a box on screen`).not.toBeNull();
  expect(to, `${target}'s target handle must have a box on screen`).not.toBeNull();
  const area = canvas as NonNullable<typeof canvas>;
  const fromBox = from as NonNullable<typeof from>;
  const toBox = to as NonNullable<typeof to>;
  const fromX = fromBox.x + fromBox.width / 2;
  const fromY = fromBox.y + fromBox.height / 2;
  const toX = toBox.x + toBox.width / 2;
  const toY = toBox.y + toBox.height / 2;
  // React Flow draws nodes in a transformed container, so an off-screen node
  // still reports visible and still has a box; pressing the mouse down there
  // does nothing at all.
  for (const [label, x, y] of [
    [source, fromX, fromY],
    [target, toX, toY],
  ] as const) {
    expect(
      x >= area.x && x <= area.x + area.width && y >= area.y && y <= area.y + area.height,
      `${label}'s handle is outside the canvas — the graph is not fitted on screen`,
    ).toBe(true);
  }

  await page.mouse.move(fromX, fromY);
  await page.mouse.down();
  await page.mouse.move(fromX + 6, fromY + 6, { steps: 3 });
  await page.mouse.move((fromX + toX) / 2, (fromY + toY) / 2, { steps: 10 });
  await page.mouse.move(toX, toY, { steps: 10 });
  await page.mouse.up();

  // Diagnostic only — the canvas renders `flowEdges`, which is written whether
  // or not the YAML helper wrote anything. The evidence is the stored document.
  await expect(
    page.locator(`.react-flow__edge[data-id*="${source}"][data-id*="${target}"]`),
    `the drag from ${source} to ${target} produced no edge on the canvas at all`,
  ).toHaveCount(1, { timeout: 10_000 });
}

/** Click Save and wait for the version PUT to land — not merely for the click. */
async function saveAndAwaitPersist(page: Page): Promise<void> {
  const persisted = page.waitForResponse(
    response =>
      response.request().method() === 'PUT' && response.url().includes('/version/prompt_lib/') && response.status() < 400,
    { timeout: 30_000 },
  );
  // Enabled is an assertion in itself: `GraphAdmissionGate` holds this button
  // disabled while the live graph is one the runtime would refuse.
  await expect(
    page.getByTestId('pipeline-save-button'),
    'Save is disabled — the editor considers the authored graph inadmissible',
  ).toBeEnabled({ timeout: 20_000 });
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

/**
 * Leave the chat panel EXPANDED, whichever state the save left it in.
 *
 * Asserting on one specific button here made the sibling journey fail with
 * "waiting for Expand the chat panel" while the page showed "Collapse the chat
 * panel" — a message about the harness. What is needed is the pane open.
 */
async function ensureChatPanelExpanded(page: Page): Promise<void> {
  const expand = page.getByRole('button', { name: 'Expand the chat panel' });
  const collapse = page.getByRole('button', { name: 'Collapse the chat panel' });
  await expect(expand.or(collapse), 'the editor must still render its chat panel after a save').toBeVisible({
    timeout: 60_000,
  });
  if ((await expand.count()) > 0) await expand.click();
  await expect(collapse).toBeVisible({ timeout: 10_000 });
}

/** Every `tool_name` the stored execution trace carries, in the order the rows come back. */
async function traceToolNames(page: Page, projectId: string, conversationId: string): Promise<readonly string[]> {
  const traces = await page.request.get(
    `${BASE_URL}/api/v2/elitea_core/message_traces/prompt_lib/${projectId}/${conversationId}`,
  );
  if (!traces.ok()) return [];
  const body = (await traces.json()) as {
    rows?: readonly { attrs?: { response_metadata?: { tool_name?: string } } }[];
  };
  return (body.rows ?? []).map(row => row.attrs?.response_metadata?.tool_name ?? '');
}

test('a two-node graph connected on the canvas runs BOTH nodes, in the order the edge declares', async ({ page }) => {
  // Same leg restriction as its siblings, for the measured reason recorded in
  // `chat.pipeline-authored.spec.ts`: the SDK worker publishes no
  // execution-trace step naming a pipeline node, so the central assertion here
  // cannot tell a compiled graph from a fall-through under that runtime.
  test.skip(
    (process.env['E2E_WORKER'] ?? 'rust') !== 'rust',
    'the SDK worker emits no pipeline trace step naming the nodes this spec reads',
  );
  // Create, author two nodes, connect, save, admit, compile, TWO model calls
  // and the stream back.
  test.setTimeout(300_000);

  const name = `${AUTOTEST_PREFIX}multi-${Date.now() % 1_000_000}`;

  // ── 1. The pipeline shell, through the real create form ────────────────
  const created = page.waitForResponse(
    response => APPLICATIONS_RE.test(new URL(response.url()).pathname) && response.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await page.goto(`${BASE_URL}/app/pipelines/create`);
  await expect(page.getByTestId('agent-name-input')).toBeVisible({ timeout: 30_000 });
  await page.getByTestId('agent-name-input').fill(name);
  await page.getByTestId('agent-description-input').fill(`${name} description`);
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('pipeline-save-button').click();

  const response = await created;
  expect(response.status(), `the pipeline must be created: ${(await response.text()).slice(0, 300)}`).toBe(201);
  const projectId = APPLICATIONS_RE.exec(new URL(response.url()).pathname)?.[1] ?? '';
  expect(projectId, 'the pipeline must belong to a project').not.toBe('');
  const body = (await response.json()) as { id?: string; version_details?: { id?: string; agent_type?: string } };
  const pipelineId = body.id ?? '';
  expect(pipelineId, 'the created pipeline must carry an id').toMatch(/^\d+$/);
  const versionId = String(body.version_details?.id ?? '');
  expect(versionId, 'the created pipeline must carry a version, or the stored read joins nothing').not.toBe('');
  expect(body.version_details?.agent_type, 'the pipelines create page must store a PIPELINE row').toBe('pipeline');

  // ── 2. Author TWO nodes and the EDGE between them, on the canvas ───────
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 20_000 });
  // With the configuration panel open, a node added at the viewport centre
  // lands under it and every click reports the panel intercepting pointer
  // events. Collapsing it also runs the editor's own `fitView`.
  await page.getByRole('button', { name: 'Collapse the configuration panel' }).click();
  await expect(page.getByRole('button', { name: 'Expand the configuration panel' })).toBeVisible({ timeout: 10_000 });
  await page.getByRole('button', { name: 'Collapse the chat panel' }).click();
  await expect(page.getByRole('button', { name: 'Expand the chat panel' })).toBeVisible({ timeout: 10_000 });

  // Each node configured while it is the topmost card — see the module header.
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: 'LLM', exact: true }).click();
  await renameNodeThroughCard(page, MINTED_LLM_NODE_ID, FIRST_NODE_ID);
  await mapTaskToTurnInput(page, FIRST_NODE_ID);

  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: 'LLM', exact: true }).click();
  await renameNodeThroughCard(page, MINTED_LLM_NODE_ID, SECOND_NODE_ID);
  await mapTaskToTurnInput(page, SECOND_NODE_ID);

  await compactCanvas(page);
  await connectHandles(page, FIRST_NODE_ID, SECOND_NODE_ID);

  await saveAndAwaitPersist(page);

  // ── 3. The EDGE reached the backend, as a transition ───────────────────
  const stored = await readStoredPipelineVersion(page.request, projectId, pipelineId, versionId);
  const graph = parseStoredGraph(stored.instructions);
  const first = graph.nodes.find(node => String(node['id']) === FIRST_NODE_ID);
  const second = graph.nodes.find(node => String(node['id']) === SECOND_NODE_ID);
  expect(first, `the stored graph must hold ${FIRST_NODE_ID}`).toBeTruthy();
  expect(second, `the stored graph must hold ${SECOND_NODE_ID}`).toBeTruthy();
  expect(FIRST_NODE_ID).toMatch(COMPILER_LEGAL_NODE_ID);
  expect(SECOND_NODE_ID).toMatch(COMPILER_LEGAL_NODE_ID);
  expect(graph.entry_point, 'the entry point is the first node added').toBe(FIRST_NODE_ID);
  expect(
    first?.['transition'],
    'the dragged connection never reached the stored document — the canvas showed an edge the YAML never got',
  ).toBe(SECOND_NODE_ID);
  expect(second?.['transition'], 'the second node still ends the graph').toBe('END');
  // Both mappings, read back rather than trusted: an off-by-one row would
  // leave a task fixed and empty, and the run would fail later with a message
  // about the model.
  for (const node of [first, second]) {
    expect(
      (node?.['input_mapping'] as Record<string, unknown> | undefined)?.['task'],
      `the node card must have pointed ${String(node?.['id'])}'s \`task\` at the turn's own input`,
    ).toEqual({ type: 'variable', value: 'input' });
  }
  expect(
    stored.instructions.includes(MINTED_LLM_NODE_ID),
    'the stored document still names a pre-rename node — a rename did not reach `entry_point`/`transition`',
  ).toBe(false);

  // ── 4. Run it from the editor's own chat pane ──────────────────────────
  await ensureChatPanelExpanded(page);
  const pane = page.getByTestId('edit-pipeline-test-chat');
  await expect(pane, 'the editor’s test chat must open once the graph is saved').toBeVisible({ timeout: 30_000 });

  const input = pane.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });

  // Clicking the composer is what MINTS the conversation — the pane creates it
  // on first interaction, not on mount.
  const conversationCreated = page.waitForResponse(
    response =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(response.url()).pathname) &&
      response.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await input.click();
  const conversationResponse = await conversationCreated;
  expect(
    conversationResponse.status(),
    `the editor pane must create its own conversation: ${(await conversationResponse.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
  const conversationId = String(((await conversationResponse.json()) as { id?: string | number }).id ?? '');
  expect(conversationId, 'the pane must create a conversation before it can send').not.toBe('');

  // Same shape, same reason as `chat.pipeline-authored.spec.ts`: the
  // conversation-create re-render discards a mid-flight fill and the Send
  // control never appears. See `fillComposer`.
  const sendButton = await fillComposer(pane, `autotest canvas multinode pipeline ${Date.now()}`);
  const started = page.waitForResponse(response => START_RE.test(response.url()) && response.request().method() === 'POST', {
    timeout: 60_000,
  });
  await sendButton.click();

  const startResponse = await started;
  // 422 is the status every admission refusal produces, and its body names
  // which one: a malformed graph, an illegal node id, a participant under the
  // wrong `entity_name` and a missing `version_id` all land here.
  expect(startResponse.status(), `the pipeline turn was refused: ${(await startResponse.text()).slice(0, 400)}`).toBe(200);

  // ── 5. A stored answer, not an error card ──────────────────────────────
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 180_000,
    message: 'the two-node pipeline turn stored no answer — the graph was admitted and then failed to produce a result',
  });

  // ── 6. BOTH NODES RAN, IN THE ORDER THE EDGE DECLARES ──────────────────
  // The assertion the whole file exists for. A decorative edge produces a run
  // in which only the entry point appears here — and, because that node's
  // reply is already in `messages`, a perfectly ordinary-looking answer.
  await expect
    .poll(async () => (await traceToolNames(page, projectId, conversationId)).filter(name => name !== ''), {
      timeout: 120_000,
      message:
        `no execution step named both \`${FIRST_NODE_ID}\` and \`${SECOND_NODE_ID}\` — a trace naming only the ` +
        'entry point means the transition authored on the canvas never became an edge the compiler followed',
    })
    .toEqual(expect.arrayContaining([FIRST_NODE_ID, SECOND_NODE_ID]));

  const names = await traceToolNames(page, projectId, conversationId);
  expect(
    names.indexOf(FIRST_NODE_ID),
    `the trace steps came back as ${JSON.stringify(names)} — the entry point must run before the node it transitions to`,
  ).toBeLessThan(names.lastIndexOf(SECOND_NODE_ID));

  // Cleanup is best-effort and deliberately last: a failure above leaves the
  // pipeline in place for inspection.
  await page.request.delete(`${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${pipelineId}`);
});
