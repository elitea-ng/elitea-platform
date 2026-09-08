/**
 * The pipeline a USER could actually have built: authored on the canvas,
 * saved, and then talked to from the editor's own chat pane — never through
 * a REST body this file wrote, and never through the chat page.
 *
 * ── HOW THIS DIFFERS FROM `chat.pipeline.spec.ts`, AND WHY BOTH EXIST ─────
 *
 * That file writes the graph with a raw version PUT and chats through the
 * `/chat` page the "Chat with pipeline" button navigates to. It is the right
 * shape for what it pins (the runtime, the participant mapping, the
 * assembler), and its header explains why it does not fight the CodeMirror
 * YAML pane: `fill()` on a `.cm-content` goes through CM6's own input
 * handling, which re-indents on every newline, so a multi-line document does
 * not arrive as the document typed.
 *
 * But a REST PUT skips the two things a user actually does. This file closes
 * both:
 *
 *  1. THE GRAPH IS AUTHORED THROUGH THE CANVAS. The Add-node menu mints the
 *     node; the node card renames it. Nothing here composes YAML. That
 *     matters because the editor is the only writer of node ids, and a
 *     minted id the compiler refuses makes every visually-authored pipeline
 *     unloadable while every screen assertion stays green — the exact defect
 *     `e2e/fixtures/pipelines.ts` was extracted for. The CodeMirror pane is
 *     deliberately never touched, for the reason above.
 *
 *  2. THE CHAT IS THE EDITOR'S OWN PANE. That pane used to be a disclosed
 *     gap: `slots.renderChat` rendered "Live test chat is not available yet"
 *     and the `adapter` resolved every method to `{error:'not_available'}`.
 *     It now mounts the real `widgets/chat-box` `ChatBox`
 *     (`pages/pipelines/ui/PipelineTestChat.tsx`). A user who authors a
 *     graph tests it where they authored it, and this is the only journey
 *     that exercises that surface at all.
 *
 * ── THE DISCRIMINATOR, AND WHY A STORED ANSWER IS NOT ONE ────────────────
 *
 * `attrs.response_metadata.tool_name` on a stored execution-trace step must
 * equal THE NODE ID THIS UI MINTED. Three things it rules out, none of which
 * a non-empty answer can:
 *
 *  - A fall-through to the direct-agent assembler. It would answer too,
 *    treating the YAML as a system prompt, and its thinking step carries
 *    `attrs: {}` — measured in `chat.pipeline.spec.ts`'s own step 6.
 *  - A direct agent under the SDK worker, which writes this same key but
 *    fills it with the langgraph node name `"agent"`, never a pipeline node
 *    id (same measurement, recorded in that file's skip note).
 *  - A coincidence. The id is renamed to something no model would emit and
 *    no default would produce, through the node card's own rename box.
 *
 * The id is asserted against the STORED graph first, so a failure at the
 * trace step cannot be blamed on a save that never carried the rename.
 *
 * Lives in `streaming/` for the same reason its sibling does: a pipeline
 * turn needs the full standalone stack (`scripts/chat-stream-e2e.sh`).
 */
import { expect, test, type Locator, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { AUTOTEST_PREFIX, expectStoredAssistantAnswer, fillComposer } from '../fixtures/api';
import {
  COMPILER_LEGAL_NODE_ID,
  parseStoredGraph,
  readStoredPipelineVersion,
  storedNodeIds,
} from '../fixtures/pipelines';

const APPLICATIONS_RE = /\/elitea_core\/applications\/prompt_lib\/(\d+)/;
/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/**
 * The id the canvas is driven to mint, and the single most load-bearing
 * string in this file.
 *
 * Constrained by the compiler's own `valid_graph_id` (ASCII alphanumerics
 * plus `_ - . :`, ≤128 bytes — `services/elitea-worker-rust/src/agents/graph/
 * yaml.rs`), which is also what the editor's rename box now enforces
 * (`NodeCardHeader.tsx`'s `findRenameRejection`). Deliberately not `answer`,
 * not `LLM_1`, and not any English word a model might echo: it has to be
 * impossible for the trace assertion below to match by accident.
 */
const LLM_NODE_ID = 'autotest_ui_authored_llm';

/** The id `getInitialNodeId` mints for the first LLM node on an empty canvas. */
const MINTED_LLM_NODE_ID = 'LLM_1';

/**
 * Rename a canvas node through its own card — double-click the name, type,
 * blur.
 *
 * The blur is what commits (`NodeCardHeader`'s `onBlur`), so `Tab` is not
 * cosmetic: without it the rename never runs and the node keeps its minted
 * id, which would make every assertion below fail somewhere less obvious.
 */
async function renameNodeThroughCard(page: Page, fromId: string, toId: string): Promise<void> {
  const node = page.locator(`.react-flow__node[data-id="${fromId}"]`);
  await expect(node).toBeVisible({ timeout: 15_000 });
  await node.getByText(fromId, { exact: true }).dblclick();
  const nameInput = node.locator('input').first();
  await expect(nameInput).toBeVisible({ timeout: 10_000 });
  await nameInput.fill(toId);
  await nameInput.press('Tab');
  // The rename rewrites the react-flow node's own id, `entry_point`, and any
  // transition pointing at the old one — so the canvas re-keying is itself
  // the evidence the commit landed.
  await expect(page.locator(`.react-flow__node[data-id="${toId}"]`)).toBeVisible({ timeout: 10_000 });
  await expect(page.locator(`.react-flow__node[data-id="${fromId}"]`)).toHaveCount(0);
}

/**
 * Point the LLM node's `task` prompt slot at the turn's own text, through the
 * node card's own two dropdowns.
 *
 * WHY THIS STEP IS NOT OPTIONAL, measured rather than assumed. A freshly
 * added LLM node does not carry an EMPTY `input_mapping` — `useLLMInputMapping`
 * seeds `{system: fixed "", task: fixed "", chat_history: fixed []}` into the
 * YAML the moment the card mounts. The worker's `validate_input_mapping` only
 * falls back to `messages -> messages` when the mapping is absent, so what it
 * actually saw was a fixed, EMPTY task with an empty history — and
 * `LlmExecutionInput::resolve` refuses exactly that (`task_text.is_empty()`
 * with no trailing tool message). Observed on the first run of this journey:
 * the start POST answered 200, the graph compiled, and the node failed at
 * `stage="input_mapping"` with `pipeline.llm_node.failed`, storing an
 * `is_error` row with no content. `input` is the builtin the turn's text is
 * seeded into.
 *
 * The rows are `system`, `task`, `chat_history` in that order —
 * `getDefaultLLMInputMapping`'s own key order, which `SimpleLLMInputs`
 * iterates. Picking the row by index is what the DOM allows (the heading chip
 * is a sibling of the field row, not an ancestor), so the STORED mapping is
 * asserted after the save: a wrong row would otherwise leave `task` empty and
 * fail later, at the run, saying nothing about the click.
 */
async function mapTaskToTurnInput(page: Page, nodeId: string): Promise<void> {
  const node = page.locator(`.react-flow__node[data-id="${nodeId}"]`);
  const typeSelects = node.getByRole('combobox', { name: 'Type' });
  await expect(typeSelects).toHaveCount(3, { timeout: 15_000 });
  await openSelect(typeSelects.nth(1));
  await page.getByRole('option', { name: 'Variable', exact: true }).click();

  // Only the `task` row is a variable now, so this names exactly one field.
  const valueSelect = node.getByRole('combobox', { name: 'Value' });
  await expect(valueSelect).toHaveCount(1, { timeout: 10_000 });
  await openSelect(valueSelect);
  await page.getByRole('option', { name: 'input', exact: true }).click();
}

/**
 * Open a `SingleSelect` that lives INSIDE a React Flow node, WITH THE MOUSE.
 *
 * This used to be keyboard-only, and the reason was a real defect rather than
 * a Playwright quirk: `SimpleLLMInputItem`'s two dropdowns carried none of
 * React Flow's escape-hatch classes, while every sibling field on the same
 * card did. The canvas's drag layer handled the mouse-down, so a `click()`
 * focused the combobox and never opened its menu — the failure snapshot
 * showed it `[active]` with no listbox anywhere in the tree. A real user
 * could not open them with the mouse at all; only the keyboard worked, which
 * is why the unit suite saw a working control.
 *
 * Both wrappers now carry `nopan nodrag`, so the click is the real user path
 * again — and driving it here is deliberate: this is the only test that
 * exercises the mouse on those dropdowns, so it is what stops the escape
 * hatch being dropped again. The unit test pins the class; this pins that the
 * class actually does its job inside the canvas.
 *
 * The options themselves render in a portal at the document root, outside the
 * canvas, so clicking THEM was never affected.
 */
async function openSelect(select: Locator): Promise<void> {
  await select.click();
  await expect(page_listbox(select)).toBeVisible({ timeout: 10_000 });
}

/** The portal-rendered listbox a `SingleSelect` opens — anchored off the page, not the select, because MUI renders it at the document root. */
function page_listbox(select: Locator): Locator {
  return select.page().getByRole('listbox');
}

/**
 * Leave the chat panel EXPANDED, whichever state the save left it in.
 *
 * Not defensive padding: the save triggers a refetch of the pipeline detail,
 * and if that refetch ever blanks the editor (a spinner in place of the two
 * panels) their collapse state is local component state and does not
 * survive. Asserting on one specific button here made this journey fail with
 * "waiting for Expand the chat panel" while the page was showing
 * "Collapse the chat panel" — a message about the harness, not the feature.
 * What the journey actually needs is the pane open.
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

/** Click Save and wait for the version PUT to land — not merely for the click. */
async function saveAndAwaitPersist(page: Page): Promise<void> {
  const persisted = page.waitForResponse(
    (response) =>
      response.request().method() === 'PUT' &&
      response.url().includes('/version/prompt_lib/') &&
      response.status() < 400,
    { timeout: 30_000 },
  );
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled({ timeout: 20_000 });
  await page.getByTestId('pipeline-save-button').click();
  await persisted;
  // No toast infrastructure exists; a failed save renders a role="alert"
  // banner (`EditPipeline.tsx`).
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

/**
 * Latch the `isPerforming` highlight before the turn starts.
 *
 * A plain `toHaveAttribute('data-performing', 'true')` would be a RACE: the
 * highlight is cleared the moment the run leaves the node, so a poll that
 * happens to look after the last frame reports a failure that says "the run
 * feed is dead" about a feed that worked. A MutationObserver installed
 * BEFORE the send cannot miss it, and what it proves is the same thing: a
 * streamed run event reached `useRunEvent` and was matched to this node.
 */
async function latchPerformingHighlight(page: Page): Promise<void> {
  await page.evaluate(() => {
    const flagged = window as unknown as { __eliteaPerformingSeen?: boolean };
    flagged.__eliteaPerformingSeen = document.querySelector('[data-performing="true"]') !== null;
    const observer = new MutationObserver(() => {
      if (document.querySelector('[data-performing="true"]') !== null) flagged.__eliteaPerformingSeen = true;
    });
    observer.observe(document.body, {
      subtree: true,
      childList: true,
      attributes: true,
      attributeFilter: ['data-performing'],
    });
  });
}

test('a pipeline authored on the canvas runs its graph and answers in the editor’s own chat pane', async ({ page }) => {
  // Same leg restriction as `chat.pipeline.spec.ts`, and for the same two
  // measured reasons: the SDK worker rejects a bare `state` type name, and
  // its pipeline run publishes no execution-trace step naming the node this
  // file reads. Without that step the central assertion here cannot tell a
  // compiled graph from a fall-through to the direct-agent runtime.
  test.skip(
    (process.env['E2E_WORKER'] ?? 'rust') !== 'rust',
    'the SDK worker emits no pipeline trace step naming the node this spec reads',
  );
  // Create, author, save, admit, compile, one model call and the stream
  // back. Every wait below is bounded well under this.
  test.setTimeout(300_000);

  // Capped at 32 characters by the form's own `maxLength` — a longer name is
  // silently truncated and every later lookup by it finds nothing.
  const name = `${AUTOTEST_PREFIX}uipipe-${Date.now() % 1_000_000}`;

  // ── 1. The pipeline shell, through the real create form ────────────────
  // Armed BEFORE the navigation: the response carries the project id, the
  // version, and `agent_type`, so what is asserted is what the server wrote.
  const created = page.waitForResponse(
    (r) => APPLICATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
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
  expect(versionId, 'the created pipeline must carry a version, or the resolver joins nothing').not.toBe('');
  expect(
    body.version_details?.agent_type,
    'the pipelines create page must store a PIPELINE row — an agent here means the page dropped `forPipeline`',
  ).toBe('pipeline');

  // ── 2. Author the graph ON THE CANVAS ──────────────────────────────────
  // The editor page, live: `rf__wrapper` and the default `END` node are
  // emitted by a mounted @xyflow/react canvas holding real editor state.
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 15_000 });

  // GIVE THE CANVAS THE WIDTH, through the editor's own collapse affordance.
  // MEASURED, not defensive: with the configuration panel open, a node added
  // at the viewport centre lands UNDER it, and Playwright reported
  // `<div data-testid="edit-pipeline-configuration-form-gap"> … intercepts
  // pointer events` on every retry until the test timed out. React Flow's
  // canvas is a transformed surface, so scrolling cannot move the node out
  // from under an overlay — the panel has to go. Collapsing it also runs the
  // editor's own `fitView`, which is what a user does for the same reason.
  await page.getByRole('button', { name: 'Collapse the configuration panel' }).click();
  await expect(page.getByRole('button', { name: 'Expand the configuration panel' })).toBeVisible({ timeout: 10_000 });
  // The chat pane is the other overlay on this row, and it is not needed
  // until step 4. Collapsing it unmounts the slot (`ChatPanel` renders
  // `renderChat` only while expanded), which is fine here and is why the
  // conversation is not created until it is expanded again below.
  await page.getByRole('button', { name: 'Collapse the chat panel' }).click();
  await expect(page.getByRole('button', { name: 'Expand the chat panel' })).toBeVisible({ timeout: 10_000 });

  // ONE `llm` node, added through the editor's own menu, then given the two
  // things it cannot run without — a distinctive id and a task that reads the
  // turn's own text. Nothing else is filled in: `output:` is left empty on
  // purpose, because `project_response` writes the reply into `messages`
  // regardless (worker `graph/llm.rs`) and the terminal result policy falls
  // back to the last assistant message there, so an extra field here would be
  // decoration this journey did not need to prove.
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: 'LLM', exact: true }).click();

  // The rename is the node-card field that makes the trace assertion below
  // mean anything; the task mapping is the one that makes the node runnable.
  await renameNodeThroughCard(page, MINTED_LLM_NODE_ID, LLM_NODE_ID);
  await mapTaskToTurnInput(page, LLM_NODE_ID);

  await saveAndAwaitPersist(page);

  // ── 3. The id the UI minted actually reached the backend ───────────────
  // Read back over the API, not off the screen: the canvas renders the
  // editor's own store, so a screen assertion here would pass even if the
  // save carried nothing.
  const stored = await readStoredPipelineVersion(page.request, projectId, pipelineId, versionId);
  const graph = parseStoredGraph(stored.instructions);
  const ids = storedNodeIds(graph);
  expect(ids, 'the renamed node must reach the stored document').toContain(LLM_NODE_ID);
  expect(LLM_NODE_ID).toMatch(COMPILER_LEGAL_NODE_ID);
  // `entry_point` is set to the first node added and rewritten by the rename;
  // it goes through the same `valid_graph_id` check as every node id, so it is
  // the first thing a broken rename would break.
  expect(graph.entry_point, 'the canvas must name its entry point').toBe(LLM_NODE_ID);
  // The two dropdowns above, read back off the stored document. Asserted here
  // rather than trusted: an off-by-one row would leave `task` fixed and empty,
  // and the run would fail three steps later with a message about the model.
  const llmNode = graph.nodes.find((node) => String(node['id']) === LLM_NODE_ID);
  expect(
    (llmNode?.['input_mapping'] as Record<string, unknown> | undefined)?.['task'],
    'the node card must have pointed `task` at the turn’s own input',
  ).toEqual({ type: 'variable', value: 'input' });
  expect(
    stored.instructions.includes(MINTED_LLM_NODE_ID),
    'the stored document still names the pre-rename node — the rename did not reach `entry_point`/`transition`',
  ).toBe(false);

  // ── 4. Chat FROM THE EDITOR'S OWN PANE ─────────────────────────────────
  // The pane is closed while the editor holds unsaved YAML (`ChatPanel`'s own
  // `disableChat`), and reopens once the save's refetch re-seeds the
  // baseline snapshot — so its appearance is itself the check that the
  // post-save refetch works. Without it the pane stayed shut until a full
  // page reload.
  await ensureChatPanelExpanded(page);
  const pane = page.getByTestId('edit-pipeline-test-chat');
  await expect(pane, 'the editor’s test chat must open once the graph is saved').toBeVisible({ timeout: 30_000 });

  const input = pane.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });

  // The run feed's latch goes in BEFORE the send — see the helper.
  await latchPerformingHighlight(page);

  // Clicking the composer is also what MINTS the conversation: the pane
  // creates it on first interaction rather than on mount, so opening the
  // editor to read a graph leaves no conversation behind. That response is
  // also where the serial id comes from — unlike `chat.pipeline.spec.ts`
  // there is no Chat button whose click this file could read it off.
  const conversationCreated = page.waitForResponse(
    (r) =>
      /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
      r.request().method() === 'POST',
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

  // The pane has just created its conversation, and the re-render that lands
  // with it DISCARDS a fill that arrives mid-flight — leaving no Send control
  // to enable. The `toBeEnabled` gate below used to be the whole guard, and it
  // cannot recover text that is already gone: it failed here in run
  // 33814567670. `fillComposer` retries the fill itself.
  const sendButton = await fillComposer(pane, `autotest canvas pipeline ${Date.now()}`);
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 60_000,
  });
  await sendButton.click();

  const startResponse = await started;
  // 422 is the status EVERY admission refusal produces, and its body is the
  // sentence a maintainer will search for: a malformed graph, an illegal node
  // id, a participant under the wrong `entity_name` and a missing
  // `version_id` all land here saying different things.
  expect(
    startResponse.status(),
    `the pipeline turn was refused: ${(await startResponse.text()).slice(0, 400)}`,
  ).toBe(200);

  // ── 5. The run reached the CANVAS, not only the transcript ─────────────
  // `useRunEvent` builds this chip from the streamed `agent_*` frames the
  // chat transport forwards. Before this change nothing fed that consumer:
  // the receive side was complete and tested, and no caller ever passed
  // `onAgentEvent`. A chip here is the wire, end to end.
  await expect(
    page.getByText('Run 1 details'),
    'no run chip appeared — the streamed run events never reached the flow editor',
  ).toBeVisible({ timeout: 120_000 });

  // ── 6. A stored answer, not an error card ──────────────────────────────
  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 120_000,
    message: 'the pipeline turn stored no answer — the graph was admitted and then failed to produce a result',
  });

  // The highlight, read from the latch rather than polled live.
  await expect
    .poll(
      async () => page.evaluate(() => (window as unknown as { __eliteaPerformingSeen?: boolean }).__eliteaPerformingSeen === true),
      {
        timeout: 30_000,
        message:
          'no node was ever marked `data-performing` — the run chip may have arrived from a frame that named no node, ' +
          'which is what a run feed carrying the wrong frames looks like',
      },
    )
    .toBe(true);

  // ── 7. THE GRAPH RAN, under the id THIS UI MINTED ──────────────────────
  // The half a stored answer cannot give (header). Polled rather than read
  // once: the trace projection is written from the same frames the stream
  // carries and finishes after the reply text does.
  await expect
    .poll(
      async () => {
        const traces = await page.request.get(
          `${BASE_URL}/api/v2/elitea_core/message_traces/prompt_lib/${projectId}/${conversationId}`,
        );
        if (!traces.ok()) return [];
        const traceBody = (await traces.json()) as {
          rows?: readonly { attrs?: { response_metadata?: { tool_name?: string } } }[];
        };
        return (traceBody.rows ?? []).map((row) => row.attrs?.response_metadata?.tool_name ?? '');
      },
      {
        timeout: 60_000,
        message:
          `no execution step named \`${LLM_NODE_ID}\` — the node id came from the canvas, so a trace naming ` +
          '"agent" (or nothing) means the graph compiler never ran the node this journey authored',
      },
    )
    .toContain(LLM_NODE_ID);

  // Cleanup is best-effort and deliberately last: a failure above leaves the
  // pipeline in place for inspection.
  await page.request.delete(`${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${pipelineId}`);
});
