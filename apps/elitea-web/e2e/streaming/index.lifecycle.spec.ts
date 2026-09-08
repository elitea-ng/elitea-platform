/**
 * The rest of the index lifecycle: RE-INDEX, DELETE, and the admission
 * contract the SEARCH tools are refused by on the index-start route.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS FILE, BESIDE `index.streaming.spec.ts`
 * ─────────────────────────────────────────────────────────────────────────────
 * `index.streaming.spec.ts` (#93) proves an index can be CREATED: the form is
 * drawn from the served schema, the run is admitted, it streams, it reaches a
 * terminal state, and the embedding path underneath it really ran. It stops
 * there. Everything a user does with an index AFTER it exists — re-run it when
 * the source changed, search it, throw it away — was proved by nothing.
 *
 * Those three are not a smaller version of the same thing:
 *
 *   - RE-INDEX runs against an index that already has a stored row and a
 *     history, which the create path never touches. It is also the action the
 *     reference UI offers from the list card and ours offers only from the
 *     selected index's header, so it is the one most likely to be wired to
 *     nothing.
 *   - SEARCH is served now, and §3 is NOT the "search is unimplemented" note
 *     it was first written as. WP16b (`48bd2dfd`) gave every tool but
 *     `index_data` a synchronous REST contract —
 *     `POST /elitea_core/test_tool/prompt_lib/{projectID}/{toolID}`
 *     (`services/elitea-main/internal/api/router.go:2282`,
 *     `toolkits.Handler.TestTool`, mapped by
 *     `internal/api/v2/toolkitrun/response.go:151-241`) — and the Run tab
 *     dispatches `search_index` over it rather than over the socket.io
 *     `chat_predict` emit (`src/features/toolkits/lib/hooks/
 *     useToolkitChatDispatch.hooks.ts`, its "fifth case"; proved at the
 *     composition root by `src/pages/toolkits/__tests__/
 *     testToolRestWiring.test.tsx`). What survived that change, and what §3
 *     pins, is narrower: the ASYNCHRONOUS admission path
 *     (`?await_response=false`) is `index_data`'s ALONE
 *     (`start_handler.go:125-131`), and it refuses every other `tool_name`
 *     with `400` and a pydantic-shaped body (`start_handler.go:351-355`).
 *   - DELETE is the only destructive action on the tab, and its failure path
 *     was a bare `catch {}` until 2026-09-07. It is also GATED: the button is
 *     disabled unless the toolkit has `remove_index` among its selected
 *     tools, which the fixture toolkit does not — see §2's own header for the
 *     tick that satisfies the gate and why it is a real user step.
 *
 * Same project, same script, same job as #93: this file matches the
 * `index-stream` project's `testMatch` (`/streaming\/index\..+\.spec\.ts/`),
 * so `scripts/index-stream-e2e.sh` and the `index-stream` job in
 * `.github/workflows/ci-web-e2e.yml` pick it up with no wiring change. It has
 * to run there and nowhere else for the reason that file's own header gives:
 * `docker-compose.e2e-standalone.yml` has no runtime plane and no worker, so
 * an index run cannot happen on the `chromium`/`webkit` stack at all.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * TURN SETTLEMENT
 * ─────────────────────────────────────────────────────────────────────────────
 * Every wait for a run to finish polls the SERVER's own record, never a
 * spinner: the client writes an optimistic `state: in_progress` row the moment
 * the button is pressed (`useToolkitChat.hooks.ts`), so a rail entry proves
 * nothing about the run. Samplers use `count()`, never `textContent()` —
 * `textContent()` auto-waits for the element to attach, so a sampler built on
 * it reads only the settled state and reports "nothing intermediate was ever
 * painted" about a transition it never watched.
 */
import { expect, test, type Locator, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';

/** The toolkit `seed-index` provisions — see `index.streaming.spec.ts`. */
const TOOLKIT_ID = '9002';
const BUCKET = 'elitea-artifacts';

const INDEX_META_RE = /\/elitea_core\/index_meta\/prompt_lib\/(\d+)\/\d+/;
const START_RE = /\/elitea_core\/test_toolkit_tool\/prompt_lib\/(\d+)/;

/** One stored index row, as `GET index_meta` answers it. */
type StoredIndex = { readonly id?: string; readonly metadata?: Record<string, unknown> };

function uniqueIndexName(): string {
  const name = `e2l${Date.now() % 100_000_000}`;
  expect(name.length, 'the index name must fit the schema’s 32-char cap').toBeLessThanOrEqual(32);
  return name;
}

/** Reads the server's own index list. The only authority on what exists. */
async function readStoredIndexes(page: Page, projectId: string): Promise<readonly StoredIndex[]> {
  const stored = await page.request.get(`${BASE_URL}/api/v2/elitea_core/index_meta/prompt_lib/${projectId}/${TOOLKIT_ID}`);
  if (!stored.ok()) return [];
  const body = (await stored.json()) as unknown;
  return Array.isArray(body) ? (body as StoredIndex[]) : [];
}

function findStored(rows: readonly StoredIndex[], name: string): StoredIndex | undefined {
  return rows.find((row) => row.metadata?.['collection'] === name);
}

/** Puts one document in the bucket, so a run has something to embed. Same two calls as `index.streaming.spec.ts`'s §0. */
async function seedIndexableDocument(page: Page, projectId: string): Promise<void> {
  const created = await page.request.post(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}`, { data: { name: BUCKET }, timeout: 30_000 });
  expect([200, 201, 409], `creating the bucket answered ${created.status()}`).toContain(created.status());

  const key = `e2e-lifecycle-${String(Date.now())}.txt`;
  const uploaded = await page.request.post(`${BASE_URL}/api/v2/artifacts/objects/${projectId}/${BUCKET}?overwrite=true`, {
    multipart: { file: { name: key, mimeType: 'text/plain', buffer: Buffer.from(`elitea lifecycle document ${String(Date.now())}`, 'utf8') } },
    timeout: 30_000,
  });
  expect(uploaded.ok(), `uploading ${key} answered ${uploaded.status()}`).toBe(true);
}

/**
 * Loads the toolkit's edit screen and leaves it on its default Configuration
 * tab (`pages/toolkits/EditToolkit.tsx:296`, `useState(0)`).
 *
 * The Indexes tab LABEL is the readiness signal, and it is a stronger one
 * than it looks: that label is rendered only when the toolkit TYPE schema has
 * arrived and offers an indexing tool (`EditToolkit.tsx:360`,
 * `!indexesTab.hidden`, resolved by `features/toolkits/lib/helpers/
 * indexesTabVisibility.ts:141-143`). So waiting for it also waits for the
 * schema the tool chips below are drawn from.
 */
async function openToolkitScreen(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/app/toolkits/all/${TOOLKIT_ID}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('tab', { name: 'Indexes' })).toBeVisible({ timeout: 30_000 });
}

/**
 * Clicks the Indexes tab of the ALREADY-LOADED screen and returns the panel
 * plus the project the rail listed in — the driver's personal project, which
 * is where the run happens.
 *
 * Separate from `openToolkitScreen` on purpose: §2 has to act on the
 * Configuration tab BEFORE this one is opened, and a second `page.goto`
 * between the two would throw that away (`editToolDetail` is page state —
 * `EditToolkit.tsx:294`).
 *
 * The list response is awaited BEFORE the tab settles for the race
 * `index.streaming.spec.ts` documents: `IndexesContainer` auto-selects an
 * index as soon as that query resolves.
 */
async function openIndexesTab(page: Page): Promise<{ readonly panel: Locator; readonly projectId: string }> {
  const listed = page.waitForResponse((r) => INDEX_META_RE.test(r.url()), { timeout: 30_000 });
  await page.getByRole('tab', { name: 'Indexes' }).click();
  const panel = page.getByTestId('edit-toolkit-indexes-tab-panel');
  await expect(panel).toBeVisible({ timeout: 15_000 });
  const listResponse = await listed;

  const projectId = INDEX_META_RE.exec(listResponse.url())?.[1] ?? '';
  expect(projectId, 'the rail must list this toolkit inside a project').not.toBe('');
  return { panel, projectId };
}

/** Load the screen and open the Indexes tab — the two steps above, for the sections that need nothing from Configuration. */
async function gotoIndexesTab(page: Page): Promise<{ readonly panel: Locator; readonly projectId: string }> {
  await openToolkitScreen(page);
  return openIndexesTab(page);
}

/**
 * Creates one index through the UI and waits for the SERVER to record it as
 * finished. Returns its name and its stored `indexed` count.
 *
 * Terminal state is read from the stored row, not from the action bar: the
 * bar flips on an SSE frame, and this helper's callers go on to assert what
 * the STORE says changed.
 */
async function createIndexThroughUi(page: Page, panel: Locator, projectId: string): Promise<{ readonly name: string }> {
  await seedIndexableDocument(page, projectId);

  await panel.getByRole('button', { name: 'Add index' }).click();
  const nameField = page.getByLabel('Index Name', { exact: true });
  await expect(nameField).toBeVisible({ timeout: 20_000 });

  const name = uniqueIndexName();
  await nameField.fill(name);

  const indexButton = page.getByRole('button', { name: 'Index', exact: true });
  await expect(indexButton).toBeEnabled({ timeout: 10_000 });
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', { timeout: 40_000 });
  await indexButton.click();
  expect((await started).status(), 'the index run must be admitted').toBe(200);

  await expect(panel.getByRole('button', { name: 'Reindex' }), 'the run must reach a terminal state').toBeVisible({ timeout: 90_000 });

  // The server's own record. `indexed >= 1` separates a real run from a run
  // over an empty bucket, which terminates green at zero.
  await expect
    .poll(async () => Number(findStored(await readStoredIndexes(page, projectId), name)?.metadata?.['indexed'] ?? -1), {
      timeout: 30_000,
      message: 'the created index was never stored with a document count',
    })
    .toBeGreaterThanOrEqual(1);

  return { name };
}

/**
 * Ticks the toolkit's "Remove index" tool on the Configuration tab, which is
 * what makes the Indexes tab's Delete button pressable at all.
 *
 * THIS IS THE GATE, NOT A WORKAROUND. `IndexActions.tsx:180` derives
 * `isRemovingDisabled` as `!selectedIndexTools.includes('remove_index')` and
 * `IndexActionsParts.tsx:43` disables the button on it, with the tooltip
 * `'"Remove index" tool is not selected'` (`IndexActionsParts.tsx:37`). The
 * fixture toolkit `seed-index` writes carries NO `selected_tools` at all —
 * its settings are `bucket` + `embedding_model` + `pgvector_configuration`
 * and nothing else (`deploy/scripts/standalone-stack.sh:1104-1114`) — so on
 * a freshly-seeded stack that button is permanently disabled, which is what
 * this journey's first real run measured (job 101650528151: the locator
 * resolved to `<button disabled …>Delete</button>` and never became
 * actionable). Selecting the tool is the step a person performs, not a hook
 * the test reaches around; the gate itself is unit-proved in
 * `src/features/toolkits/indexes/ui/IndexDetails/IndexActions.test.tsx:121`.
 *
 * The tick is LIVE EDITOR STATE, deliberately not a save.
 * `pages/toolkits/lib/useIndexesTabState.ts:47` reads
 * `editToolDetail.settings.selected_tools`, and `editToolDetail` is
 * `EditToolkit`'s own `useState` above the tab bar (`EditToolkit.tsx:294`),
 * so the selection survives the Configuration → Indexes switch and never
 * reaches `p_<project>.elitea_tools`. That matters: a saved change would
 * leak into `index.streaming.spec.ts` and into the next run of this file.
 *
 * The chip's label is the tool name title-cased with underscores replaced —
 * `remove_index` → `Remove index` (`ToolActionsSelector.tsx:65-68`), a MUI
 * `Chip` that renders `role="button"` because it is clickable
 * (`ChipWithCheckIcon.tsx:44-50`). `artifact` — the fixture toolkit's type —
 * really does offer the tool: `remove_index` is one of the 16 keys of its
 * `selected_tools.args_schemas` in the served type schema
 * (`services/elitea-main/internal/runtimecomposition/
 * current_toolkit_schema_snapshot.json`), which is where the chip list comes
 * from (`ToolBase.render.tsx:272-277`).
 */
async function selectRemoveIndexTool(page: Page): Promise<void> {
  const chip = page.getByRole('button', { name: 'Remove index', exact: true });
  await expect(chip, 'the Configuration tab must offer the "Remove index" tool for an artifact toolkit').toBeVisible({ timeout: 30_000 });
  await chip.click();
}

/* ── §1. RE-INDEX ────────────────────────────────────────────────────────── */

test('an existing index can be re-indexed, and the server records the new run', async ({ page }) => {
  test.setTimeout(300_000);

  const { panel, projectId } = await gotoIndexesTab(page);
  const { name } = await createIndexThroughUi(page, panel, projectId);

  const before = findStored(await readStoredIndexes(page, projectId), name);
  const historyBefore = ((before?.metadata?.['history'] as readonly unknown[] | undefined) ?? []).length;

  // The index is already selected (the create run left it selected), so the
  // edit-mode action bar is on screen. Its terminal control is `Reindex`.
  const reindexButton = panel.getByRole('button', { name: 'Reindex' });
  await expect(reindexButton).toBeEnabled({ timeout: 20_000 });

  /*
   * The in-progress sampler, opened WITH the click. `count()`, not
   * `textContent()`: `textContent()` auto-waits for the element, so it would
   * block until the run finished and then report a single settled reading —
   * "no in-progress state was painted" about a window it never watched.
   */
  const stopButton = panel.getByRole('button', { name: 'Stop' });
  const inProgressSamples: number[] = [];
  const sampler = (async () => {
    const deadline = Date.now() + 90_000;
    while (Date.now() < deadline) {
      if ((await stopButton.count().catch(() => 0)) > 0) inProgressSamples.push(Date.now());
      if (inProgressSamples.length > 0 && (await stopButton.count().catch(() => 0)) === 0) return;
      await page.waitForTimeout(25);
    }
  })();

  const restarted = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', { timeout: 60_000 });
  await reindexButton.click();

  const restartResponse = await restarted;
  expect(
    restartResponse.status(),
    `Reindex must admit a NEW run, not merely repaint. Body: ${(await restartResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  const restartBody = (await restartResponse.json()) as { task_id?: string };
  expect(restartBody.task_id, 'the re-run must carry its own task id').toMatch(/^[0-9a-f]+$/);

  await sampler;
  expect(inProgressSamples.length, 'Reindex must paint an in-progress state — a click that only repaints is the defect').toBeGreaterThan(0);

  await expect(panel.getByRole('button', { name: 'Reindex' }), 'the re-run must terminate').toBeVisible({ timeout: 120_000 });

  /*
   * The assertion the UI cannot fake. A re-index appends to the stored row's
   * `history`, so a longer history is the server's own statement that a SECOND
   * run happened against this index — not that a button was pressed.
   */
  await expect
    .poll(
      async () => ((findStored(await readStoredIndexes(page, projectId), name)?.metadata?.['history'] as readonly unknown[] | undefined) ?? []).length,
      { timeout: 60_000, message: 'the re-index finished on screen but the server recorded no second run' },
    )
    .toBeGreaterThan(historyBefore);
});

/* ── §2. DELETE ──────────────────────────────────────────────────────────── */

/**
 * ORDER MATTERS HERE, and it is the whole shape of the test.
 *
 * The tool is ticked on the Configuration tab FIRST, before the Indexes tab
 * is ever opened and before the index exists — see `selectRemoveIndexTool`
 * for why the tick is needed and why it is not a save. Doing it afterwards
 * would mean switching tabs back and forth, and the Indexes panel unmounts
 * with the tab (`EditToolkit.tsx:367-385`): on remount `IndexesContainer`
 * re-runs its auto-select effect and picks the first ALREADY-INDEXED row of
 * the server's list, not the one this test just made. Deleting whatever that
 * happened to be would still go green here and would destroy a sibling
 * journey's fixture.
 */
test('an index can be deleted, and it is gone from the server, not only from the screen', async ({ page }) => {
  test.setTimeout(240_000);

  await openToolkitScreen(page);
  await selectRemoveIndexTool(page);
  const { panel, projectId } = await openIndexesTab(page);
  const { name } = await createIndexThroughUi(page, panel, projectId);

  expect(findStored(await readStoredIndexes(page, projectId), name), 'the fixture index must exist before it is deleted').toBeDefined();

  // Enabled BECAUSE of the tick above, and asserted before the click so a
  // regression in the gate reads as "Delete never became pressable" instead
  // of as a four-minute `locator.click` timeout with no explanation.
  const removeIndex = panel.getByRole('button', { name: 'Delete', exact: true });
  await expect(removeIndex, 'selecting the "remove_index" tool must enable Delete').toBeEnabled({ timeout: 20_000 });
  await removeIndex.click();

  /*
   * `DeleteEntityModal` with `shouldRequestInputName`: the confirm button
   * stays disabled until the index's own name is typed. Asserting the
   * DISABLED state first is what proves the guard is real — a modal that
   * confirmed on any input would pass the rest of this test.
   */
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible({ timeout: 15_000 });
  const confirm = dialog.getByRole('button', { name: 'Delete' });
  await expect(confirm, 'the confirm button must be gated on the typed name').toBeDisabled();

  await dialog.getByRole('textbox').fill(name);
  await expect(confirm).toBeEnabled({ timeout: 10_000 });

  const deleted = page.waitForResponse((r) => INDEX_META_RE.test(r.url()) && r.request().method() === 'DELETE', { timeout: 40_000 });
  await confirm.click();
  expect((await deleted).status(), 'the delete must be accepted').toBe(200);

  await expect(dialog, 'a successful delete closes the confirm modal').toHaveCount(0, { timeout: 20_000 });

  // The server's list, which is the only thing that can distinguish a deleted
  // index from one merely removed from a client-side store.
  await expect
    .poll(async () => findStored(await readStoredIndexes(page, projectId), name) === undefined, {
      timeout: 30_000,
      message: 'the index disappeared from the screen but is still stored',
    })
    .toBe(true);

  // And it stays gone across a reload, which re-reads everything.
  await gotoIndexesTab(page);
  await expect(panel.getByText(name), 'the deleted index must not come back on reload').toHaveCount(0);
});

/* ── §3. THE ASYNCHRONOUS ADMISSION CONTRACT ─────────────────────────────── */

/** The one refusal shape `writeValidationError` writes (`start_handler.go:345-355`). */
interface ValidationRefusal {
  readonly error?: readonly { readonly type?: string; readonly loc?: readonly string[]; readonly msg?: string }[];
}

test('the asynchronous index start refuses every tool but index_data, and admits index_data on the same route', async ({ page }) => {
  test.setTimeout(120_000);

  const { projectId } = await gotoIndexesTab(page);

  /*
   * WHAT THIS PINS, AND WHAT IT NO LONGER CLAIMS.
   *
   * It was first written as "search has no backend, and here is the refusal
   * that proves it". That is out of date and this file's header says so:
   * WP16b gave every tool but `index_data` a SYNCHRONOUS REST contract on a
   * different route (`POST /elitea_core/test_tool/prompt_lib/{projectID}/
   * {toolID}` → `toolkitrun.WriteOutcome`), and the Run tab dispatches
   * `search_index` over it. Search is served; it is simply not served HERE.
   *
   * What this route still promises is narrower, and worth a live assertion
   * because two independent clients depend on it — `indexesApi.ts:219-220`
   * (which is why it only ever sends `index_data`) and pylon's own callers:
   *
   *  a) With `await_response=false` the handler validates `tool_name` before
   *     anything else and refuses everything but `index_data`
   *     (`start_handler.go:125-131`). The refusal is `400` with a
   *     pydantic-shaped body — `writeValidationError` is the ONE writer of
   *     that shape and it writes `http.StatusBadRequest`
   *     (`start_handler.go:351-355`). It is NOT 422: 422 on this surface
   *     belongs to the synchronous route's `unsupported_toolkit`/
   *     `unknown_tool` outcomes (`toolkitrun/response.go:184-191`), and the
   *     two must stay distinguishable. Asserting the exact status AND the
   *     exact issue keeps "refused" apart from both "accepted" (a 200 that
   *     quietly indexed instead of searching, which is worse than a refusal)
   *     and "crashed" (a 500, or a 503 from a stack with no runtime).
   *  b) `index_data` on the SAME route in the SAME breath is admitted, so (a)
   *     is a statement about the TOOL and not about a broken route, an
   *     expired session or a rejected body.
   *
   * `tool_params` — NOT `tool_parameters`. That is the field name the Go
   * decoder reads (`currentStartBody`, `start_handler.go:184-192`); a body
   * that spells it the other way reaches `Validate()` with an empty object,
   * `indexNameFromToolParameters` rejects it (`application/indexing/
   * start.go:55, 137-148`) and the control below would be refused for the
   * wrong reason — a 400 that says nothing about tool names.
   */
  const searchAttempt = await page.request.post(`${BASE_URL}/api/v2/elitea_core/test_toolkit_tool/prompt_lib/${projectId}?await_response=false`, {
    data: {
      tool_name: 'search_index',
      toolkit_config: { toolkit_id: Number(TOOLKIT_ID) },
      tool_params: { query: 'anything' },
    },
    timeout: 30_000,
  });

  // (a) A validation refusal, naming the field and the only accepted value.
  expect(
    searchAttempt.status(),
    `an asynchronous non-index_data run must be REFUSED, not accepted or crashed. Body: ${(await searchAttempt.text()).slice(0, 400)}`,
  ).toBe(400);
  const refusal = (await searchAttempt.json()) as ValidationRefusal;
  expect(refusal.error?.[0], 'the refusal must name the field and the only accepted value, in the pydantic shape pylon clients parse').toEqual({
    type: 'value_error',
    loc: ['tool_name'],
    msg: "Input should be 'index_data'",
  });

  // (b) The control. Same route, same session, same toolkit — only the tool
  // name differs. A blanket 400 would fail here, which is what makes (a) a
  // statement about the tool rather than about the request.
  const indexAttempt = await page.request.post(`${BASE_URL}/api/v2/elitea_core/test_toolkit_tool/prompt_lib/${projectId}?await_response=false&execution_contract=index.ingest.v1`, {
    data: {
      tool_name: 'index_data',
      toolkit_config: { toolkit_id: Number(TOOLKIT_ID) },
      tool_params: { index_name: uniqueIndexName() },
    },
    timeout: 60_000,
  });
  expect(
    indexAttempt.status(),
    `the same route must ADMIT index_data, or the refusal above says nothing about the tool. Body: ${(await indexAttempt.text()).slice(0, 400)}`,
  ).toBe(200);
  expect(((await indexAttempt.json()) as { task_id?: string }).task_id, 'an admitted run must carry its task id').toMatch(/^[0-9a-f]+$/);
});
