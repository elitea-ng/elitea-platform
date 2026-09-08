/**
 * Journey 16f: the editor's OTHER view — the YAML pane, and the round trip
 * between it and the canvas.
 *
 * Ported BY USE CASE from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/pipelines/
 * test_pipeline_advanced.py::TestYamlEditor`), each test naming the legacy
 * test it answers.
 *
 * ── Why this had no coverage, and what it costs ──────────────────────────
 *
 * Every other pipelines journey works in Flow mode. `chat.pipeline.spec.ts`'s
 * header explains why none of them go near this pane: a multi-line YAML
 * document cannot be TYPED into it, because `fill()` on a CodeMirror
 * `.cm-content` goes through CM6's own input handling and re-indents on every
 * newline. That is a reason not to author through it — it is not a reason to
 * leave the pane itself unchecked. `EditorPanel.tsx`'s mode switch does real
 * work in both directions (`dumpYaml(yamlJsonObject)` going one way,
 * `onParseCodeToJson(yamlCode)` coming back), and a conversion that lost a
 * node would show up nowhere else: the canvas would simply come back smaller,
 * and the next save would store the smaller graph.
 *
 * So these three read the pane and drive the toggle; none of them types into
 * it.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX } from '../../fixtures/api';
import { createPipelineThroughApi, deletePipeline, type CreatedPipeline } from '../../fixtures/pipelines';

/**
 * The node id this file's probe pipeline is stored with.
 *
 * Deliberately NOT `LLM_1` — that is what the starter template ships and what
 * the editor itself mints for a first LLM node, so a pane rendering a default
 * document instead of the stored one would satisfy an assertion on it. This
 * id can only be on screen because the version this journey created is the
 * version the pane is showing. It passes the compiler's own `valid_graph_id`
 * (ASCII alphanumerics plus `_ - . :` — `services/elitea-worker-rust/src/
 * agents/graph/yaml.rs`), so the pipeline it names is a real, loadable one.
 */
const PROBE_NODE_ID = 'autotest_yaml_probe';

/** The starter document with its single node renamed to {@link PROBE_NODE_ID} — same shape, distinctive id. */
const PROBE_TEMPLATE = `state:
  input:
    type: str
  messages:
    type: list
entry_point: ${PROBE_NODE_ID}
nodes:
  - id: ${PROBE_NODE_ID}
    type: llm
    input:
      - input
    input_mapping:
      system:
        type: fstring
        value: You are a helpful assistant.
      task:
        type: variable
        value: input
    output:
      - messages
    transition: END
`;

const created: CreatedPipeline[] = [];

test.afterEach(async ({ page }) => {
  while (created.length > 0) {
    const pipeline = created.pop();
    if (pipeline === undefined) continue;
    await deletePipeline(page.request, pipeline);
  }
});

function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
}

/** The Flow/Yaml pair — `TabGroupButton` renders `PipelineEditorMode`'s keys as the button labels. */
function modeButton(page: Page, label: 'Flow' | 'Yaml') {
  return page.getByRole('button', { name: label, exact: true });
}

/** Open a pipeline's editor and wait for the canvas, which is the mode the editor opens in. */
async function openPipelineEditor(page: Page, pipeline: CreatedPipeline): Promise<void> {
  await page.goto(`${BASE_URL}/app/pipelines/latest/${pipeline.id}`);
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 30_000 });
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 20_000 });
}

/** Every node id currently drawn on the canvas — the same read `pipelines.lifecycle.spec.ts` uses. */
async function canvasNodeIds(page: Page): Promise<readonly string[]> {
  return page.locator('.react-flow__node').evaluateAll(nodes => nodes.map(node => node.getAttribute('data-id') ?? ''));
}

/*
 * Legacy: `TestYamlEditor::test_yaml_view_toggle` — "switch to YAML, the
 * editor is visible; switch back, the canvas is".
 *
 * Both halves are asserted in both directions, which is what separates a
 * toggle that SWITCHES from one that merely adds: the canvas has to go away
 * when the YAML pane arrives. It really does go away rather than unmount —
 * `computeFlowWrapperSx` sets `display: none` on the wrapper — so a
 * visibility assertion is the right one and a `toHaveCount(0)` would be wrong.
 */
test('J16f: the Flow/Yaml toggle swaps the canvas for the YAML editor, and back', async ({ page }) => {
  const pipeline = await createPipelineThroughApi(page.request, uniqueName('yamltog'));
  created.push(pipeline);
  await openPipelineEditor(page, pipeline);

  // The editor opens on Flow. `aria-pressed` is `ToggleButton`'s own state,
  // so this says the CONTROL agrees with the pane rather than merely that a
  // canvas is on screen.
  await expect(modeButton(page, 'Flow')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByTestId('pipeline-yaml-editor')).toHaveCount(0);

  await modeButton(page, 'Yaml').click();
  await expect(page.getByTestId('pipeline-yaml-editor'), 'the YAML editor must open').toBeVisible({ timeout: 20_000 });
  await expect(modeButton(page, 'Yaml')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByTestId('rf__wrapper'), 'the canvas must go away, not sit behind the YAML pane').toBeHidden();

  await modeButton(page, 'Flow').click();
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 20_000 });
  await expect(modeButton(page, 'Flow')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByTestId('pipeline-yaml-editor')).toHaveCount(0);
});

/*
 * Legacy: `TestYamlEditor::test_yaml_content_reflects_pipeline` — "an LLM
 * pipeline's YAML contains `entry_point` plus `nodes`/`llm`".
 *
 * Those three words are also in the document a create page seeds, so the
 * legacy assertion cannot tell "showing THIS pipeline" from "showing a
 * default". This journey stores a node id nothing else in the app mints and
 * requires the pane to show that — and requires the CANVAS to draw the same
 * id, which is the claim the two views are one document.
 */
test('J16f: the YAML pane shows the graph the server stored for this pipeline', async ({ page }) => {
  const pipeline = await createPipelineThroughApi(page.request, uniqueName('yamlcnt'), {
    instructions: PROBE_TEMPLATE,
  });
  created.push(pipeline);
  await openPipelineEditor(page, pipeline);

  // The canvas drew the stored node — so the editor really did load this
  // version, and a YAML pane that disagreed with it below would be the
  // pane's own failure rather than a load that never happened.
  await expect(page.locator(`.react-flow__node[data-id="${PROBE_NODE_ID}"]`)).toBeVisible({ timeout: 20_000 });

  await modeButton(page, 'Yaml').click();
  const yamlDocument = page.getByTestId('pipeline-yaml-editor').locator('.cm-content');
  await expect(yamlDocument).toBeVisible({ timeout: 20_000 });

  // `entry_point` is the key that uniquely identifies a pipeline document,
  // and the value beside it is this journey's own id — matched together, on
  // one line, so a pane showing some other pipeline's graph cannot pass.
  await expect(yamlDocument).toContainText(`entry_point: ${PROBE_NODE_ID}`);
  await expect(yamlDocument).toContainText(`id: ${PROBE_NODE_ID}`);
  await expect(yamlDocument).toContainText('type: llm');
});

/*
 * Legacy: `TestYamlEditor::test_flow_yaml_round_trip` — "add a node, toggle
 * Flow → YAML → Flow, and the node count is unchanged".
 *
 * Partially covered by J16's "an edited flow graph survives a save + reload"
 * (`pipelines.lifecycle.spec.ts`), which proves the SAVE round trip. This is
 * the other one: no save, no reload, purely the two in-memory conversions
 * `EditorPanel.tsx`'s mode switch runs. The node added here has never been
 * persisted, so nothing but those conversions can carry it across.
 *
 * Compared by ID SET rather than by count: a conversion that dropped the
 * added node and duplicated `END` would keep the count and lose the graph.
 */
test('J16f: a Flow → Yaml → Flow round trip preserves the canvas, node for node', async ({ page }) => {
  const pipeline = await createPipelineThroughApi(page.request, uniqueName('yamlrt'));
  created.push(pipeline);
  await openPipelineEditor(page, pipeline);

  // Add a node the STORED document does not have, so the round trip has
  // something to carry that a re-read of the version could not supply.
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: 'Printer', exact: true }).click();
  await expect(page.locator('.react-flow__node[data-id="Printer_1"]')).toBeVisible({ timeout: 20_000 });

  const before = [...(await canvasNodeIds(page))].sort();
  expect(before, 'the canvas must hold the starter graph, END and the added Printer').toContain('Printer_1');

  await modeButton(page, 'Yaml').click();
  await expect(page.getByTestId('pipeline-yaml-editor')).toBeVisible({ timeout: 20_000 });
  // The added node reached the YAML side of the conversion. Without this the
  // test could pass on a toggle that never converted anything at all.
  await expect(page.getByTestId('pipeline-yaml-editor').locator('.cm-content')).toContainText('id: Printer_1');

  await modeButton(page, 'Flow').click();
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 20_000 });

  // Node ORDER inside the canvas is not part of the claim; node CONTENT is.
  await expect
    .poll(async () => [...(await canvasNodeIds(page))].sort(), {
      timeout: 20_000,
      message: 'the Flow → Yaml → Flow round trip changed the graph — a node was lost or invented in the conversion',
    })
    .toEqual(before);
});
