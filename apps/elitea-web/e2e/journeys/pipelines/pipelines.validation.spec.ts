/**
 * Journey 16 (validation half): an illegal pipeline graph fails IN THE
 * EDITOR, not at turn time in another process.
 *
 * ## What this file exists to catch
 *
 * The native Rust pipeline compiler admits a stored document whole or not at
 * all: `#[serde(deny_unknown_fields)]` on every node struct, plus required
 * fields and reference checks (`services/elitea-worker-rust/src/agents/
 * graph/compiler.rs` and friends). Its refusal is a stable, data-free code
 * — `graph.pipeline.invalid_configuration` — raised inside the worker, and
 * there is no server-side YAML validation route to ask beforehand (only
 * `publish_validate`, which is a publication check). So until the editor
 * refused, the FIRST signal a user got that a graph was illegal was a chat
 * turn that failed somewhere else, naming no field.
 *
 * ## The discriminator, and why the panel text alone is not it
 *
 * The obvious test — "the panel says `summary` is undeclared" — passes just
 * as well against an editor that shows the message AND saves anyway. What
 * separates those two worlds is the backend: this file saves a good graph
 * first, records the exact bytes the backend stored, makes the graph
 * illegal, tries to save, and asserts the stored `instructions` are
 * BYTE-IDENTICAL afterwards. Nothing about the screen can fake that.
 */
import { test, expect, type Locator, type Page } from '@playwright/test';

import { load as loadYaml } from 'js-yaml';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, clickCreateButton, deleteAgent } from '../../fixtures/api';
import { parseStoredGraph, readStoredPipelineVersion, resolveLatestPipelineVersionId } from '../../fixtures/pipelines';

/**
 * A one-node pipeline the compiler admits: the LLM node's single data output
 * (`summary`) is declared in `state:`, its id passes `valid_graph_id`
 * (`graph/yaml.rs:362`), and `entry_point` names it.
 *
 * Written in YAML FLOW style, on one line, on purpose: it is typed into a
 * CodeMirror document, and a block-style body would depend on the editor not
 * re-indenting what is inserted.
 */
const ADMISSIBLE_YAML =
  '{state: {input: str, messages: list, summary: str}, entry_point: LLM_1, ' +
  'nodes: [{id: LLM_1, type: llm, input: [messages], output: [summary], transition: END}]}';

/**
 * The SAME graph with one thing changed: `summary` is gone from `state:`, so
 * the LLM node now writes to a state key nobody declared —
 * `compiler.rs:1284`, "a node output or clean key is not declared in
 * pipeline state". One edit, one rule, one named key.
 */
const INADMISSIBLE_YAML =
  '{state: {input: str, messages: list}, entry_point: LLM_1, ' +
  'nodes: [{id: LLM_1, type: llm, input: [messages], output: [summary], transition: END}]}';

/** A graph carrying a static interrupt — what the switches would author if they were live. */
const INTERRUPTED_YAML =
  '{state: {input: str, messages: list, summary: str}, entry_point: LLM_1, interrupt_before: [LLM_1], ' +
  'nodes: [{id: LLM_1, type: llm, input: [messages], output: [summary], transition: END}]}';

const VERSION_PUT = '/version/prompt_lib/';

/** Application ids minted below, deleted in `afterEach`. */
const createdIds: string[] = [];

test.afterEach(async ({ page }) => {
  while (createdIds.length > 0) {
    const id = createdIds.pop();
    if (id !== undefined) await deleteAgent(page.request, id);
  }
});

/**
 * Drive the real create form and return the backend-assigned id. A SERIAL id
 * in the URL is something no stub route can produce.
 */
async function createPipeline(page: Page, name: string): Promise<string> {
  await page.goto(BASE_URL + '/app/pipelines/my');
  await page.waitForURL('**/pipelines**', { timeout: 15_000 });
  await clickCreateButton(page);
  await page.waitForURL('**/app/pipelines/create**', { timeout: 15_000 });

  const panel = page.getByTestId('create-pipeline-form-panel');
  const nameInput = panel.getByTestId('agent-name-input');
  await expect(nameInput).toBeVisible({ timeout: 10_000 });
  await nameInput.fill(name);
  await panel.getByTestId('agent-description-input').fill(`${AUTOTEST_PREFIX}JRNY-016 validation`);
  await page.getByTestId('pipeline-save-button').click();

  await page.waitForURL(/\/app\/pipelines\/latest\/\d+/, { timeout: 20_000 });
  const id = /\/app\/pipelines\/latest\/(\d+)/.exec(page.url())?.[1];
  expect(id, 'create must navigate to the backend-assigned pipeline id').toBeTruthy();
  createdIds.push(id as string);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 15_000 });
  return id as string;
}

/**
 * Author `document` through the editor's own YAML tab, then return to the
 * flow canvas so the node panels render it.
 *
 * The YAML tab is the authoring surface reached here because the Output
 * picker offers only DECLARED state variables — the illegal state this
 * journey is about is exactly the one the pickers cannot produce, and is
 * reached in the real product by deleting a state variable a node still
 * writes to.
 */
async function authorYaml(page: Page, document: string): Promise<void> {
  await page.getByRole('button', { name: 'YAML', exact: true }).click();
  const editor = page.locator('.cm-content');
  await expect(editor).toBeVisible({ timeout: 10_000 });

  /*
   * Select-all + `keyboard.insertText`, NOT `locator.fill()`. CodeMirror 6
   * owns its own document; `fill()` rewrites the contenteditable DOM, which
   * leaves the text on screen (so a `toContainText` assertion passes) while
   * the editor's state — and therefore the `onChange` that feeds the YAML
   * store — never sees it. `insertText` goes through `beforeinput`, which is
   * the path CodeMirror actually listens on.
   */
  await editor.click();
  await page.keyboard.press('ControlOrMeta+a');
  await page.keyboard.insertText(document);
  await expect(editor).toContainText('entry_point');
  // `CodeMirrorEditor` debounces its `onChange` by 30ms
  // (`CHANGE_DEBOUNCE_MS`), and the Flow toggle below reads the STORE, not
  // the editor. Ten debounce periods, so the store is settled before the
  // mode switch reads it.
  await page.waitForTimeout(300);

  await page.getByRole('button', { name: 'Flow', exact: true }).click();
  await expect(page.locator('.react-flow__node[data-id="LLM_1"]')).toBeVisible({ timeout: 15_000 });
}

/**
 * Click Save and wait until the editor has taken the saved document back.
 *
 * The version PUT is not the end of a save. Once the save settles,
 * `useRefetchPipelineAfterSave` re-reads the application detail, and
 * `usePipelineVersionSync` re-initialises the canvas from the CHANGED
 * `instructions` it carries. That rebuilds the flow nodes without their
 * selection, so a node panel open at Save time closes.
 *
 * Issue 3029 (WebKit): this helper used to return on the PUT alone. The next
 * `openNodePanel` then saw the panel that was still open, skipped opening it,
 * and the refetch closed it under the following assertion, which waited its
 * full 10 s for a panel nothing would reopen. In the passing retry's trace
 * the refetch (`GET application/prompt_lib/…`) answered about 200 ms after
 * the version PUT and the panel was already closed when the next step
 * looked. The failing attempt timed out on `openNodePanel`'s LAST assertion,
 * not inside its open branch, and its final snapshot shows no panel: the
 * panel was open when the helper checked and closed afterwards.
 *
 * So the helper waits for that refetch (a GET of THIS application that
 * arrives after the PUT answered) and, when a node panel was open, for the
 * panel to close: the visible proof that the re-initialisation ran.
 */
async function saveAndAwaitPersist(page: Page): Promise<void> {
  const applicationId = /\/app\/pipelines\/latest\/(\d+)/.exec(page.url())?.[1];
  expect(applicationId, 'Save is asserted on an existing pipeline only').toBeTruthy();
  const panel = page.getByTestId('pipeline-node-panel');
  const panelWasOpen = await panel.first().isVisible();
  let putAnswered = false;
  const persisted = page
    .waitForResponse(
      (response) => response.request().method() === 'PUT' && response.url().includes(VERSION_PUT) && response.status() < 400,
      { timeout: 30_000 },
    )
    .then(() => {
      putAnswered = true;
    });
  const detailPath = new RegExp(`/application/prompt_lib/${DEFAULT_PROJECT_ID}/${applicationId as string}(?:[?#]|$)`);
  const reloaded = page.waitForResponse(
    (response) => putAnswered && response.request().method() === 'GET' && detailPath.test(response.url()) && response.status() === 200,
    { timeout: 30_000 },
  );
  await page.getByTestId('pipeline-save-button').click();
  // Awaited together: a PUT that fails rejects at once, and the refetch
  // waiter, armed before the click, is never left rejecting with nobody
  // listening (it would be reported against a later test).
  await Promise.all([persisted, reloaded]);
  if (panelWasOpen) {
    await expect(panel, 'the post-save re-initialisation closes the open node panel').toHaveCount(0, { timeout: 10_000 });
  }
  await expect(page.getByText('Failed to save your changes.')).toHaveCount(0);
}

/** The stored `instructions` for this pipeline's `latest` version, straight from the API. */
async function readStoredInstructions(page: Page, applicationId: string): Promise<string> {
  const versionId = await resolveLatestPipelineVersionId(page.request, DEFAULT_PROJECT_ID, applicationId);
  const stored = await readStoredPipelineVersion(page.request, DEFAULT_PROJECT_ID, applicationId, versionId);
  return stored.instructions;
}

/**
 * The WHOLE stored document, not the narrowed graph.
 *
 * `parseStoredGraph` deliberately rebuilds a `{nodes, entry_point, state}`
 * object, so a top-level key it does not model — `interrupt_before` is
 * exactly one — comes back `undefined` from it whether the backend stored
 * it or not. Asserting "no interrupts" against that shape would pass on an
 * empty object, which is the failure mode this whole file exists to avoid.
 */
function parseStoredDocument(instructions: string): Readonly<Record<string, unknown>> {
  const parsed = loadYaml(instructions);
  expect(typeof parsed === 'object' && parsed !== null, `stored instructions are not a YAML mapping:\n${instructions}`).toBe(true);
  return parsed as Readonly<Record<string, unknown>>;
}

async function openNodePanel(page: Page, nodeId: string): Promise<Locator> {
  const panel = page.locator(`[data-testid="pipeline-node-panel"][data-node-id="${nodeId}"]`);
  if (!(await panel.isVisible().catch(() => false))) {
    const card = page.locator(`.react-flow__node[data-id="${nodeId}"]`);
    await expect(card).toBeVisible({ timeout: 15_000 });
    await page.getByRole('button', { name: 'Fit view' }).click();
    await expect(async () => {
      if (!(await panel.isVisible().catch(() => false))) {
        await card.click({ timeout: 3_000 });
      }
      await expect(panel).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 15_000 });
  }
  await expect(panel).toBeVisible({ timeout: 10_000 });
  return panel;
}

async function openInterruptSwitches(page: Page, nodeId: string): Promise<{ before: Locator; after: Locator }> {
  const panel = await openNodePanel(page, nodeId);
  const advancedSection = panel.getByTestId('pipeline-node-panel-advanced-section');
  const advancedToggle = advancedSection.getByRole('button', { name: 'Advanced', exact: true });
  await expect(advancedToggle).toBeVisible({ timeout: 10_000 });
  if ((await advancedToggle.getAttribute('aria-expanded')) !== 'true') {
    await advancedToggle.click();
  }
  await expect(advancedToggle).toHaveAttribute('aria-expanded', 'true', { timeout: 10_000 });
  return {
    before: advancedSection.getByRole('switch', { name: 'Interrupt before' }),
    after: advancedSection.getByRole('switch', { name: 'Interrupt after' }),
  };
}

/** The admission panel inside one node's config drawer. */
function nodeIssues(page: Page, nodeId: string): Locator {
  return page.locator(`[data-testid="pipeline-node-panel"][data-node-id="${nodeId}"]`).getByTestId('node-admission-issues');
}

test('J16: an output naming an undeclared state key is named in the panel and never reaches the backend', async ({
  page,
}) => {
  // test.slow() (90 s, as the other create-edit-save-reload journeys in this
  // folder): on a webkit CI leg with four workers this journey measured
  // 30-35 s with no single slow call (every API read < 0.3 s), so the
  // default 30 s budget cut it off mid-reload and it read as a hang.
  test.slow();
  const name = `${AUTOTEST_PREFIX}adm-${Date.now() % 1e9}`;
  const id = await createPipeline(page, name);

  // 1. A LEGAL graph saves. Without this half, "the PUT did not fire" below
  //    would also be true of an editor that can never save anything.
  await authorYaml(page, ADMISSIBLE_YAML);
  await openNodePanel(page, 'LLM_1');
  await expect(nodeIssues(page, 'LLM_1')).toHaveCount(0);
  await expect(page.locator('.react-flow__node[data-id="LLM_1"]').getByTestId('pipeline-node-v2-status-needs-configuration')).toHaveCount(0);
  await expect(page.getByTestId('pipeline-save-button')).toBeEnabled();
  await saveAndAwaitPersist(page);

  const before = await readStoredInstructions(page, id);
  expect(parseStoredGraph(before).state, 'the saved graph declares the key its LLM node writes to').toHaveProperty('summary');

  // 2. Delete the state variable the LLM node writes to. One edit; the graph
  //    is now one the runtime refuses whole (compiler.rs:1284).
  await authorYaml(page, INADMISSIBLE_YAML);

  // 3. The canvas card shows needs-configuration status and the node panel names
  //    the exact field AND the exact key — not "invalid node".
  await expect(
    page.locator('.react-flow__node[data-id="LLM_1"]').getByTestId('pipeline-node-v2-status-needs-configuration'),
  ).toBeVisible({ timeout: 10_000 });
  await openNodePanel(page, 'LLM_1');
  const issues = nodeIssues(page, 'LLM_1');
  await expect(issues).toBeVisible({ timeout: 10_000 });
  await expect(issues).toContainText('output[0]: "summary" is not declared');

  // 4. The gate: Save is disabled, and a click forced past that disabled
  //    state (dispatchEvent skips Playwright's actionability wait) still
  //    issues no PUT.
  const saveButton = page.getByTestId('pipeline-save-button');
  await expect(saveButton).toBeDisabled();
  const put = page.waitForResponse((response) => response.request().method() === 'PUT' && response.url().includes(VERSION_PUT), {
    timeout: 5_000,
  });
  await saveButton.dispatchEvent('click');
  await expect(put, 'an inadmissible graph must not issue a version PUT').rejects.toThrow();

  // 5. The proof the screen cannot fake: the stored document is byte-for-byte
  //    what step 1 saved.
  const after = await readStoredInstructions(page, id);
  expect(after, 'the stored pipeline must be byte-identical to the last legal save').toBe(before);
});

/* onetest: Authored static pauses bind to stored node identities and survive save. */
test('J16: active interrupt controls roundtrip the exact stored node lists', async ({ page }) => {
  const id = await createPipeline(page, `${AUTOTEST_PREFIX}intr-${Date.now() % 1e9}`);
  await authorYaml(page, ADMISSIBLE_YAML);
  const { before, after } = await openInterruptSwitches(page, 'LLM_1');
  await expect(before).toBeEnabled();
  await expect(after).toBeEnabled();
  await before.check();
  await after.check();
  await expect(before).toBeChecked();
  await expect(after).toBeChecked();
  await saveAndAwaitPersist(page);
  const stored = parseStoredDocument(await readStoredInstructions(page, id));
  expect(parseStoredGraph(await readStoredInstructions(page, id)).nodes.map((node) => node['id'])).toContain('LLM_1');
  expect(stored['interrupt_before']).toEqual(['LLM_1']);
  expect(stored['interrupt_after']).toEqual(['LLM_1']);

  const reopened = await openInterruptSwitches(page, 'LLM_1');
  await reopened.before.uncheck();
  await reopened.after.uncheck();
  await saveAndAwaitPersist(page);
  const cleared = parseStoredDocument(await readStoredInstructions(page, id));
  expect(cleared['interrupt_before'] ?? []).toEqual([]);
  expect(cleared['interrupt_after'] ?? []).toEqual([]);
});

/* onetest: Invalid static pause identities cannot replace a saved, legal document. */
test('J16: missing and duplicate static interrupt identities are refused before save', async ({ page }) => {
  // test.slow() (90 s), as the other create-edit-save journeys in this file:
  // it creates a pipeline, saves it, reads the stored document back, then
  // re-authors it twice with a stored-document read after each. That is
  // three API round trips and three YAML authorings on top of a create and a
  // save, which the bare 30 s budget only just held on a compose leg.
  test.slow();
  const id = await createPipeline(page, `${AUTOTEST_PREFIX}invalid-intr-${Date.now() % 1e9}`);
  await authorYaml(page, INTERRUPTED_YAML);
  await saveAndAwaitPersist(page);
  const before = await readStoredInstructions(page, id);
  expect(parseStoredDocument(before)['interrupt_before']).toEqual(['LLM_1']);

  for (const entries of ['missing_node', 'LLM_1, LLM_1']) {
    await authorYaml(page, INTERRUPTED_YAML.replace('[LLM_1]', `[${entries}]`));
    const gate = page.getByTestId('graph-admission-gate');
    await expect(gate).toBeVisible({ timeout: 10_000 });
    await expect(gate).toContainText(entries.includes(',') ? 'interrupt_before[1]' : 'interrupt_before[0]');
    await expect(gate).toContainText(entries.includes(',') ? 'LLM_1' : 'missing_node');
    await expect(page.getByTestId('pipeline-save-button')).toBeDisabled();
    expect(await readStoredInstructions(page, id)).toBe(before);
  }
});
