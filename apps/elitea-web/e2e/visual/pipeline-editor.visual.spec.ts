/**
 * Snapshot coverage for the PIPELINE EDITOR — `/pipelines/:tab/:agentId`, the
 * route `parity/screenshot-index.json` describes as "three panes: config
 * column, Flow/Yaml canvas, right test-chat panel".
 *
 * `routes.visual.spec.ts`'s NOT COVERED block listed this route among the
 * "detail/editor routes that need a seeded entity to reach with real data",
 * and 58600cb0 is why it now earns a baseline: that change turned the canvas
 * from something that drew nodes into something that AUTHORS a document the
 * native Rust runtime accepts — compiler-legal node ids (`Agent_1`, not
 * `Agent 1`), a 12-rule admission catalogue rendered on the offending node
 * card, a Save button vetoed while the graph is inadmissible, two interrupt
 * switches disabled with a visible reason, and a real `widgets/chat-box`
 * `ChatBox` in the test-chat pane. Every one of those is a RENDERING, and none
 * of them had a pixel reference.
 *
 * ── WHY THIS FILE SERVES THE PIPELINE'S OWN DETAIL RESPONSE, AND NOTHING ELSE ─
 *
 * This is the one deviation from the rest of the suite and it is not a
 * convenience. Reaching this route needs an application row, and creating one
 * inside the `visual` project BREAKS ANOTHER BASELINE IN THE SAME RUN:
 *
 *   - `routes.visual.spec.ts`'s `pipelines-list-empty` navigates to
 *     `/app/pipelines/latest` and waits for the literal "You have no
 *     pipelines." That copy is `PrivatePipelinesList.tsx`'s empty branch, and
 *     `usePipelineTabs` renders the PRIVATE list for a non-public project
 *     whatever the `:tab` segment says — so it is the branch this stack shows
 *     for that URL.
 *   - `playwright.config.ts` sets `fullyParallel: true` and `workers: 4` in
 *     CI, and `--grep @visual` runs every file in this directory together. A
 *     pipeline this file created would be alive in the tenant while that shot
 *     was taken, on a schedule nothing here controls. Not a small risk — a
 *     coin-flip, in a gate whose whole value is that a failure means
 *     something.
 *
 * There is no cross-file mutual exclusion in Playwright short of `workers: 1`,
 * and serialising the whole `visual` job for one spec would be a worse trade
 * than this one. So this file creates NO server state at all: it fulfils the
 * ONE GET the editor loads its pipeline from, with a fixed document, and lets
 * every other request reach the real stack.
 *
 * WHAT THAT DOES AND DOES NOT PIN, stated plainly so nobody reads more into a
 * green run than is there:
 *   - PINNED: the whole real composition root above the document — the app
 *     shell, `/pipelines/$tab/$agentId` routing, `EditPipeline`,
 *     `usePipelineVersionSync`'s parse + dagre layout, `ConfigurationTab`'s
 *     three panes, every node card, the admission gate, the interrupt rows,
 *     and the real `ChatBox`. Also the real project, permission, author and
 *     model-catalogue reads, which are NOT intercepted.
 *   - NOT PINNED: that elitea-main returns this shape. That is the pipelines
 *     JOURNEY suite's job and it does it against the live backend
 *     (`e2e/journeys/pipelines/*.spec.ts` read the STORED document back over
 *     the API), plus `e2e/streaming/chat.pipeline-authored.spec.ts` end to
 *     end. A pixel gate was never the right instrument for a transport.
 *
 * The fixture's shape is not invented either: it is the same
 * `ApplicationDetail` body `src/pages/pipelines/EditPipeline.test.tsx`'s own
 * `detail()` builds and the generated `getGetApplicationMockHandler` serves,
 * so it cannot drift from the contract the page is typed against without that
 * suite going red first.
 *
 * ── LANDMARKS ──────────────────────────────────────────────────────────────
 *
 * The suite's rule is that no landmark is admitted on inspection, because a
 * shot taken before a screen resolved pins the loading state as the reference
 * and then matches forever (#159, #174). The usual instrument — stall the
 * route's own API and check the landmark still resolves — cannot be run
 * against a response this file serves itself, so the discriminator is read off
 * the source instead, and it is a real one rather than a substitute:
 *
 *   `EditPipeline.tsx` renders the heading as
 *   `detail ? pipelineDetailDisplayName(detail) : t(…, 'Pipeline')`, so the
 *   pipeline's NAME is on screen only once the detail query resolved; the
 *   loading branch renders the literal "Pipeline".
 *
 *   `editorIsLoading(isFetching, detail)` makes `ConfigurationTab` render a
 *   spinner INSTEAD of the editor while `detail === undefined`, unmounting the
 *   canvas and both side panels — so `rf__wrapper` and the `END` node are
 *   structurally exclusive with the loading state, not merely late.
 *
 * Both are asserted before every shot. The name is the load-bearing half: the
 * canvas would also be absent on a 404, and a screen that renders "Pipeline
 * not found" has no `rf__wrapper` either.
 *
 * ── WHAT IS MASKED (almost nothing, deliberately) ──────────────────────────
 *
 * Only the shared `volatileRegions()` list, which on this screen matches
 * nothing at all. Every value visible here is fixed by construction:
 *   - the pipeline name, description, version name and `created_at` come from
 *     the fixture below, not the wall clock;
 *   - node ids are minted deterministically (`Agent_1`, `LLM_1`, …) —
 *     `getInitialNodeId` counts from the live node list, and the one id that
 *     IS timestamped (`Condition`) belongs to a deprecated type the Add menu
 *     does not offer;
 *   - node POSITIONS are a pure function of the pane geometry and the existing
 *     nodes (`calculatePositionForNewNode`), and the viewport is pinned to
 *     1602x848 by the `visual` project;
 *   - the test-chat pane mints no conversation until it is interacted with
 *     (`PipelineTestChat`'s `onPointerDownCapture`), so nothing here creates a
 *     row and no id or timestamp reaches the screen;
 *   - the author block comes from the real `/social/author`, which is the
 *     seeded `E2E Member` persona.
 * Edge animation and MUI transitions are frozen by `settle()`, which is where
 * that belongs — a mask over the canvas would hide the graph, i.e. the subject.
 *
 * ── NOT COVERED HERE, AND WHY ──────────────────────────────────────────────
 *
 *  - THE RUN FEED. `NodeCard` exposes the executing node as
 *    `data-performing="true"` and `EditorPanel` adds a "Run N details" node
 *    while a turn streams. Reaching that needs a live turn, which needs the
 *    runtime plane, the worker and a model backend — none of which the stack
 *    this job stands up (`deploy/docker-compose.e2e-standalone.yml`) has. It
 *    is the `chat-stream` project's stack, and asserting it is already
 *    `e2e/streaming/chat.pipeline-authored.spec.ts`'s job, which reads that
 *    attribute during a real run. A baseline of a run would have to live in a
 *    project with a different `webServer`, and a still frame of a moving
 *    highlight is a poor gate for it besides.
 *  - A SHOT OF THE TEST-CHAT PANE ON ITS OWN. Its two states are both pinned
 *    already, full-size, inside the page shots below: live in
 *    `pipeline-editor-empty`/`pipeline-editor-interrupts-disabled`, and
 *    "Save the pipeline to test it" in the two authored-graph shots. A
 *    component-scoped duplicate of the same pixels would add a PNG and no
 *    signal — `lib/settle.ts` records the measurement behind that: at
 *    `maxDiffPixels: 3` the page shot already catches an 11px change, so the
 *    ~7x amplification a scoped shot buys has nothing to buy it for.
 *  - THE YAML TAB, the collapsed side panels, and the node CONFIGURATION
 *    pickers (toolkit/tool/output selects). Real screens, no baseline; stated
 *    rather than implied.
 */
import { test, expect, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { SNAPSHOT_TOLERANCE, settle, shellSettled, volatileRegions } from './lib/settle';

/*
 * The `parity/screenshot-index.json` route this file claims, spelled verbatim.
 * `scripts/check-visual-coverage.mjs` counts ONLY exact matches, and coverage
 * is DECLARED rather than inferred from navigation, because inference
 * over-reported and laundered the very gap it existed to expose.
 *
 * It appears ONCE in this file, and deliberately not inside a backticked
 * phrase: the gate's pattern is `@covers\s+(\S+)`, so a trailing backtick
 * would be captured as part of the route and put a string matching no index
 * row into the declared set.
 *
 * @covers /pipelines/:tab/:agentId
 */

/**
 * The application id this file's fixture answers for.
 *
 * Deliberately far outside the SERIAL range `scripts/e2e-stack.sh seed` and
 * the journey suite ever reach, so the interception below can never shadow a
 * real row that some other spec created, and so a stray request for it against
 * an un-intercepted page 404s loudly instead of finding something.
 */
const PIPELINE_ID = '990001';
const VERSION_ID = '990002';

/**
 * On screen in the page heading, and therefore the loaded-vs-loading
 * discriminator (see this file's header). No `autotest_` prefix: nothing is
 * created, so there is nothing for the sweeper to find.
 */
const PIPELINE_NAME = 'Visual Baseline Pipeline';

/**
 * `GET /elitea_core/application/prompt_lib/{projectId}/{applicationId}` — the
 * ONE request `useEditPipelineData` issues for this route.
 *
 * A RegExp, not a glob: Playwright's `*` does not cross `/`, and the project
 * segment has to be matched without pinning it to `1`. Anchored on the id so
 * no other application's detail is touched.
 *
 * The VERSION endpoint is deliberately NOT intercepted.
 * `needsExplicitVersionFetch` is false whenever the URL carries no `:version`
 * segment (`/pipelines/latest/{id}` does not), so that query never runs — and
 * leaving it live means a future change that starts issuing it fails here
 * rather than being silently served a second copy of this fixture.
 */
const DETAIL_ROUTE = new RegExp(
  `/api/v2/elitea_core/application/prompt_lib/[^/?#]+/${PIPELINE_ID}(?:[/?#]|$)`,
);

/**
 * The stored `ApplicationDetail` body, with `instructions` — the pipeline YAML
 * document, which is what the canvas is a rendering of — supplied per shot.
 *
 * Field for field the shape `src/pages/pipelines/EditPipeline.test.tsx`'s
 * `detail()` builds, which is in turn the generated `ApplicationDetail`. RAW
 * body, no `{data: …}` envelope: `eliteaFetch` wraps the parsed body itself
 * (`shared/api/generated/mutator.ts`), and double-wrapping it would leave every
 * field `undefined` on a 200 — the #132 shape.
 *
 * `llm_settings` is deliberately ABSENT. The model picker then renders the
 * project catalogue's own answer, exactly as it does for a pipeline that names
 * no model, instead of this file inventing a model name that may not be in the
 * seeded catalogue and pinning whatever the picker does with an unknown one.
 * `conversation_starters` is empty for the same reason: starter chips are the
 * ChatBox's, and an invented set would make this baseline a picture of a
 * fixture rather than of the pane.
 */
function pipelineDetailBody(instructions: string): Record<string, unknown> {
  return {
    id: PIPELINE_ID,
    name: PIPELINE_NAME,
    description: 'Fixed document for the @visual pipeline-editor baselines.',
    icon: '',
    owner_id: '1',
    created_at: '2026-01-01T00:00:00Z',
    versions: [
      {
        id: VERSION_ID,
        name: 'base',
        status: 'draft',
        agent_type: 'pipeline',
        created_at: '2026-01-01T00:00:00Z',
      },
    ],
    version_details: {
      id: VERSION_ID,
      application_id: PIPELINE_ID,
      name: 'base',
      status: 'draft',
      agent_type: 'pipeline',
      instructions,
      conversation_starters: [],
    },
  };
}

/**
 * A one-node pipeline the runtime admits, byte-identical to
 * `e2e/journeys/pipelines/pipelines.validation.spec.ts`'s `ADMISSIBLE_YAML`:
 * the LLM node's single data output (`summary`) is declared in `state:`, its
 * id passes `valid_graph_id` (worker `graph/yaml.rs:362`), and `entry_point`
 * names it. Shared with that journey on purpose — the same document proven
 * admissible against the real backend there is the one photographed here.
 */
const ADMISSIBLE_GRAPH =
  '{state: {input: str, messages: list, summary: str}, entry_point: LLM_1, ' +
  'nodes: [{id: LLM_1, type: llm, input: [messages], output: [summary], transition: END}]}';

/**
 * What a freshly created pipeline stores: nothing.
 *
 * `usePipelineVersionSync` loads `''` as `undefined` and `parseYaml(undefined)`
 * yields the single default `END` node — the same canvas
 * `e2e/journeys/pipelines/pipelines.lifecycle.spec.ts` asserts immediately
 * after the create form lands ("a live flow editor", `rf__node-END`). So this
 * is the real first-open state of the editor, not a contrived empty one.
 */
const EMPTY_GRAPH = '';

/**
 * Serve `instructions` for {@link PIPELINE_ID}, open the editor on it, and
 * prove the screen RESOLVED before any caller reaches for the shutter.
 *
 * The two assertions are the landmark pair from this file's header: the
 * backend-derived name (absent during load, and absent on a not-found screen)
 * and the mounted canvas (structurally unmountable while `detail` is
 * undefined).
 */
async function openEditor(page: Page, instructions: string): Promise<void> {
  await page.route(DETAIL_ROUTE, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(pipelineDetailBody(instructions)),
    }),
  );

  await page.goto(`${BASE_URL}/app/pipelines/latest/${PIPELINE_ID}`, { waitUntil: 'domcontentloaded' });
  await shellSettled(page);
  await expect(page.getByRole('heading', { name: PIPELINE_NAME, exact: true })).toBeVisible({
    timeout: 20_000,
  });
  await expect(page.getByTestId('rf__wrapper')).toBeVisible({ timeout: 20_000 });
  // The GRAPH landmark is left to each caller, deliberately: `rf__wrapper`
  // proves the canvas mounted, not that this document's nodes are on it, and
  // the two documents used here terminate differently. Each test below names
  // a node id its own document must produce.
}

/**
 * Add one node of `label` through the editor's OWN menu — no store pokes.
 *
 * The same helper shape the pipelines journeys use, and the reason it is the
 * menu rather than a seeded document is that the minted id is half of what
 * these shots are for: `Agent_1`, with an underscore, is the fix 58600cb0
 * landed, and it is legible on the node card.
 */
async function addNodeThroughMenu(page: Page, label: string): Promise<void> {
  await page.getByRole('button', { name: 'Add node' }).click();
  await page.getByRole('menuitem', { name: label, exact: true }).click();
  // The menu is a MUI `Menu` in a portal; it must be gone before the shutter,
  // or the shot photographs a closing popover. Its own transition is frozen by
  // `settle()`, but only after it has been removed from the DOM.
  await expect(page.getByRole('menu')).toHaveCount(0, { timeout: 10_000 });
  await pinScrollToTop(page);
}

/**
 * Put every scroll position back to zero, and prove it stayed there.
 *
 * NOTHING IN THIS SUITE PINNED SCROLL, and the Add-node menu moves it. MUI's
 * `Menu` returns focus to the control that opened it when it closes, and the
 * browser scrolls that control into view; with three nodes added the editor
 * body was photographed 40px up on two runs in three, which reads as a whole-
 * frame diff on `pipeline-editor-authored-graph` and says nothing about the
 * graph the shot exists for.
 *
 * `settle()` freezes MOTION; a scroll offset is not motion, it is state, so it
 * belongs here rather than there — and this file is the only one that drives a
 * portal menu before a shutter.
 *
 * ReactFlow's canvas is unaffected: it pans with a CSS transform on
 * `.react-flow__viewport`, and has no scroll offset to reset.
 */
async function pinScrollToTop(page: Page): Promise<void> {
  await page.evaluate(() => {
    (document.activeElement as HTMLElement | null)?.blur();
    window.scrollTo(0, 0);
    for (const element of document.querySelectorAll('*')) {
      const node = element as HTMLElement;
      if (node.scrollTop !== 0) node.scrollTop = 0;
      if (node.scrollLeft !== 0) node.scrollLeft = 0;
    }
  });
  // Read back rather than assumed: a container that scrolls itself again after
  // the write is the case this helper exists for, and it must fail here with a
  // name instead of as an unexplained pixel diff.
  await expect
    .poll(
      () =>
        page.evaluate(
          () =>
            window.scrollY +
            window.scrollX +
            Array.from(document.querySelectorAll('*')).filter(
              (element) => (element as HTMLElement).scrollTop !== 0 || (element as HTMLElement).scrollLeft !== 0,
            ).length,
        ),
      { timeout: 10_000, message: 'the editor kept a scroll offset the shot cannot reproduce' },
    )
    .toBe(0);
}

/**
 * The editor's first open: the default `END`-only canvas, the configuration
 * column with the model picker, and the test-chat pane LIVE.
 *
 * The chat assertion is not decoration. `ChatPanel` computes
 * `disableChat = settings.disableChat || isPipelineDirty`, so a live pane
 * proves BOTH that an application id reached it and that the editor considers
 * the loaded document clean — the state in which the pane can actually run a
 * turn. Its other state is pinned by the two authored-graph shots below.
 */
test('@visual pipeline-editor-empty', async ({ page }) => {
  await openEditor(page, EMPTY_GRAPH);
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('edit-pipeline-test-chat')).toBeVisible({ timeout: 20_000 });
  await settle(page);

  await expect(page).toHaveScreenshot('pipeline-editor-empty.png', {
    fullPage: false,
    mask: volatileRegions(page),
    ...SNAPSHOT_TOLERANCE,
  });
});

/**
 * The light counterpart. `parity/screenshot-index.json` carries one
 * (`pipeline-flow-editor.light.jpeg`) and calls out what actually differs —
 * "canvas grid and node chrome invert, add-node FAB is magenta" — which is the
 * accent-hue split the index's own `accentHue` finding describes, not a
 * recolour.
 *
 * The scheme is switched through the app's REAL control, the way
 * `routes.visual.spec.ts` does it and for the reason issue #61 gives: writing
 * `localStorage` or forcing CSS would make this shot test the stylesheet while
 * claiming to test the toggle. Duplicated here rather than imported from that
 * spec — importing a `*.spec.ts` re-registers its tests.
 */
test('@visual pipeline-editor-empty-light', async ({ page }) => {
  await page.goto(BASE_URL + '/app/settings/personalization', { waitUntil: 'domcontentloaded' });
  const light = page.getByRole('button', { name: 'Light', exact: true });
  await expect(light).toBeVisible({ timeout: 20_000 });
  await light.click();
  await expect(light).toHaveAttribute('aria-pressed', 'true');
  await expect
    .poll(() => page.evaluate(() => document.documentElement.getAttribute('data-el-scheme')), {
      timeout: 10_000,
    })
    .toBe('light');

  await openEditor(page, EMPTY_GRAPH);
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('edit-pipeline-test-chat')).toBeVisible({ timeout: 20_000 });
  // Re-asserted on the route under test: a navigation that reset the scheme
  // would otherwise produce a second dark baseline under a light name, and it
  // would look correct.
  expect(await page.evaluate(() => document.documentElement.getAttribute('data-el-scheme'))).toBe('light');
  await settle(page);

  await expect(page).toHaveScreenshot('pipeline-editor-empty-light.png', {
    fullPage: false,
    mask: volatileRegions(page),
    ...SNAPSHOT_TOLERANCE,
  });
});

/**
 * Three node families placed through the Add-node menu, each carrying the id
 * the editor minted for it.
 *
 * LLM / Router / Printer specifically: they are three of the six labels
 * `pipelines.lifecycle.spec.ts` calls `SAVEABLE_NODE_LABELS`, i.e. the ones
 * whose seeded defaults are COMPLETE the moment the node is added. That
 * journey adds all six and then saves successfully through the admission gate,
 * which is the evidence that this graph carries no admission issues — asserted
 * below rather than assumed, because a shot with an unexpected error alert in
 * it would be indistinguishable from the intended one by looking at a green
 * run.
 *
 * The chat pane is in its "Save the pipeline to test it" state here, and that
 * is correct rather than incidental: adding a node writes a new document
 * through `EditorPanel`'s `setYamlJsonObject`, which re-dumps `yamlCode`, and
 * `computeIsPipelineYamlCodeDirty` reports dirty because the document really
 * did change (a mere re-dump of the SAME document does not — that function
 * compares against a re-dumped baseline for exactly this reason). The pane
 * runs the version stored on the server, so it closes while the editor holds
 * an unsaved graph.
 */
test('@visual pipeline-editor-authored-graph', async ({ page }) => {
  await openEditor(page, EMPTY_GRAPH);
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 20_000 });

  for (const label of ['LLM', 'Router', 'Printer']) {
    await addNodeThroughMenu(page, label);
  }

  // The minted ids, on the canvas. Underscored — `Agent 1`/`LLM 1` with a
  // space is the id `valid_graph_id` (worker `graph/yaml.rs:362`) refuses and
  // 58600cb0 removed.
  for (const nodeId of ['LLM_1', 'Router_1', 'Printer_1']) {
    await expect(page.locator(`.react-flow__node[data-id="${nodeId}"]`)).toBeVisible({ timeout: 15_000 });
  }
  // A CLEAN graph: no card carries an admission alert and the save gate shows
  // no summary. Without this the shot could quietly become a second copy of
  // `pipeline-editor-node-admission` below.
  await expect(page.getByTestId('node-admission-issues')).toHaveCount(0);
  await expect(page.getByTestId('graph-admission-gate')).toHaveCount(0);
  await expect(page.getByTestId('edit-pipeline-test-chat-disabled')).toBeVisible({ timeout: 20_000 });
  // Once more after the assertions above: each one can move focus, and focus
  // moves scroll.
  await pinScrollToTop(page);
  await settle(page);

  await expect(page).toHaveScreenshot('pipeline-editor-authored-graph.png', {
    fullPage: false,
    mask: volatileRegions(page),
    ...SNAPSHOT_TOLERANCE,
  });
});

/**
 * The admission surface, both halves of it, in one screen.
 *
 * An `Agent` node is added because its runtime-required `tool` (the
 * participant alias the runtime resolves, worker `application.rs:49`) is
 * seeded EMPTY on purpose — the editor asks rather than guessing — so the node
 * is inadmissible the moment it exists. That makes this the cheapest honest
 * way to reach the state, and it is the state a real author hits first.
 *
 * What the shot pins:
 *   - `NodeAdmissionIssues` on the offending card, naming the YAML field and
 *     the identifier that offends it rather than "invalid node". It renders in
 *     `NodeCard`'s shared body, so every node family gets it;
 *   - `GraphAdmissionGate`'s summary in the configuration column, listing the
 *     document-level issues and pointing at the node ids;
 *   - the Save button DISABLED, which is the gate itself — the veto travels
 *     through react-hook-form's `root.*` channel into the `canSave` flag
 *     `EditPipeline` already computes.
 * All three are asserted, so a regression that dropped any one of them fails
 * on the assertion with a name rather than as an unexplained pixel diff.
 */
test('@visual pipeline-editor-node-admission', async ({ page }) => {
  await openEditor(page, EMPTY_GRAPH);
  await expect(page.locator('.react-flow__node[data-id="END"]')).toBeVisible({ timeout: 20_000 });
  await addNodeThroughMenu(page, 'Agent');

  const card = page.locator('.react-flow__node[data-id="Agent_1"]');
  await expect(card).toBeVisible({ timeout: 15_000 });
  const issues = card.getByTestId('node-admission-issues');
  await expect(issues).toBeVisible({ timeout: 15_000 });
  // The exact sentence, not merely "an alert is present". The panel names the
  // YAML FIELD and says what is wrong with it, and that specificity is the
  // thing worth pinning — an alert reading "invalid node" would satisfy a
  // toBeVisible() and would be a regression.
  await expect(issues).toContainText('an Agent node must name the agent it runs');
  const gate = page.getByTestId('graph-admission-gate');
  await expect(gate).toBeVisible({ timeout: 15_000 });
  await expect(gate).toContainText('Agent_1');
  await expect(page.getByTestId('pipeline-save-button')).toBeDisabled();
  await settle(page);

  await expect(page).toHaveScreenshot('pipeline-editor-node-admission.png', {
    fullPage: false,
    mask: volatileRegions(page),
    ...SNAPSHOT_TOLERANCE,
  });
});

/**
 * The two interrupt switches, disabled WITH a visible reason.
 *
 * They are disabled because the two workers disagree: the native Rust runtime
 * refuses any pipeline declaring `interrupt_before`/`interrupt_after` whole
 * (worker `compiler.rs:470-474`) and the Python SDK worker honours them, and
 * the editor cannot know which one takes a turn — so it authors the
 * intersection. Before that, flipping either switch turned a working pipeline
 * into one that would not start, with no signal in the UI at all.
 *
 * The switches were kept and disabled rather than removed, precisely so the
 * withheld capability stays VISIBLE — which makes this a screen whose whole
 * point is how it looks, and the one state in this file that a page shot is
 * the only way to pin. `CommonInterruptSettings` renders inside the node card
 * body, and `expandAll` starts `true` (`FlowEditor.tsx:164`), so the stored
 * document below shows it without any expansion step.
 *
 * A stored ADMISSIBLE document is used rather than a menu-authored one so the
 * card is clean: this shot is about the withheld control, and an admission
 * alert above it would change what the reader is looking at. The chat pane is
 * live here for the same reason it is in `pipeline-editor-empty` — a loaded
 * document that has not been edited is not dirty.
 */
test('@visual pipeline-editor-interrupts-disabled', async ({ page }) => {
  await openEditor(page, ADMISSIBLE_GRAPH);

  const card = page.locator('.react-flow__node[data-id="LLM_1"]');
  await expect(card).toBeVisible({ timeout: 15_000 });
  await expect(card.getByRole('switch', { name: 'Interrupt before' })).toBeDisabled();
  await expect(card.getByRole('switch', { name: 'Interrupt after' })).toBeDisabled();
  await expect(card.getByTestId('interrupt-withheld-reason')).toContainText('native pipeline runtime refuses');
  // The document is admissible, so neither admission surface is on screen —
  // this shot must not double as the admission one.
  await expect(page.getByTestId('node-admission-issues')).toHaveCount(0);
  await expect(page.getByTestId('graph-admission-gate')).toHaveCount(0);
  await settle(page);

  await expect(page).toHaveScreenshot('pipeline-editor-interrupts-disabled.png', {
    fullPage: false,
    mask: volatileRegions(page),
    ...SNAPSHOT_TOLERANCE,
  });
});
