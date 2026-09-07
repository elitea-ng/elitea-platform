/**
 * The rest of the index lifecycle: RE-INDEX, SEARCH, DELETE.
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
 *   - SEARCH has no Go backend at all. `start_handler.go:87-90` rejects every
 *     `tool_name` that is not `index_data`, and the only other dispatch path is
 *     a socket.io `chat_predict` emit that elitea-main does not serve. §3 is
 *     therefore not a search journey: it PINS that refusal against the running
 *     stack, with an `index_data` control in the same breath so the refusal is
 *     a statement about the tool and not about a broken route. It fails the day
 *     search becomes servable, which is when a real journey should replace it.
 *   - DELETE is the only destructive action on the tab, and its failure path
 *     was a bare `catch {}` until 2026-09-07.
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
 * Opens the Indexes tab and returns the panel plus the project the rail
 * listed in — the driver's personal project, which is where the run happens.
 *
 * The list response is awaited BEFORE the tab settles for the race
 * `index.streaming.spec.ts` documents: `IndexesContainer` auto-selects an
 * index as soon as that query resolves.
 */
async function openIndexesTab(page: Page): Promise<{ readonly panel: Locator; readonly projectId: string }> {
  await page.goto(`${BASE_URL}/app/toolkits/all/${TOOLKIT_ID}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('tab', { name: 'Indexes' })).toBeVisible({ timeout: 30_000 });

  const listed = page.waitForResponse((r) => INDEX_META_RE.test(r.url()), { timeout: 30_000 });
  await page.getByRole('tab', { name: 'Indexes' }).click();
  const panel = page.getByTestId('edit-toolkit-indexes-tab-panel');
  await expect(panel).toBeVisible({ timeout: 15_000 });
  const listResponse = await listed;

  const projectId = INDEX_META_RE.exec(listResponse.url())?.[1] ?? '';
  expect(projectId, 'the rail must list this toolkit inside a project').not.toBe('');
  return { panel, projectId };
}

/**
 * Creates one index through the UI and waits for the SERVER to record it as
 * finished. Returns its name and its stored `indexed` count.
 *
 * Terminal state is read from the stored row, not from the action bar: the
 * bar flips on an SSE frame, and this helper's callers go on to assert what
 * the STORE says changed.
 */
async function createIndexThroughUi(page: Page, panel: Locator): Promise<{ readonly name: string; readonly projectId: string }> {
  const { projectId } = await openIndexesTab(page);
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

  return { name, projectId };
}

/* ── §1. RE-INDEX ────────────────────────────────────────────────────────── */

test('an existing index can be re-indexed, and the server records the new run', async ({ page }) => {
  test.setTimeout(300_000);

  const { panel } = await openIndexesTab(page);
  const { name, projectId } = await createIndexThroughUi(page, panel);

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

test('an index can be deleted, and it is gone from the server, not only from the screen', async ({ page }) => {
  test.setTimeout(240_000);

  const { panel } = await openIndexesTab(page);
  const { name, projectId } = await createIndexThroughUi(page, panel);

  expect(findStored(await readStoredIndexes(page, projectId), name), 'the fixture index must exist before it is deleted').toBeDefined();

  await panel.getByRole('button', { name: 'Delete' }).click();

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
  await openIndexesTab(page);
  await expect(panel.getByText(name), 'the deleted index must not come back on reload').toHaveCount(0);
});

/* ── §3. SEARCH — why there is no search journey ─────────────────────────── */

test('searching a built index is refused by the server, and the refusal is the reason there is no search journey', async ({ page }) => {
  test.setTimeout(120_000);

  const { projectId } = await openIndexesTab(page);

  /*
   * THIS IS NOT A DISABLED TEST. It is the assertion that pins WHY the tab's
   * Run/search path cannot be journeyed end to end on this platform, stated
   * where it will fail the day that stops being true.
   *
   * Search against a built index has no Go backend. `start_handler.go:87-90`
   * validates `tool_name` before anything else and refuses everything but
   * `index_data`. The only other dispatch path the client has is a socket.io
   * `chat_predict` emit, and elitea-main mounts no socket.io server — with
   * `VITE_SOCKET_SERVER` unset the app is handed the NULL client, whose emit
   * is a documented no-op (`shared/api/socket/client.ts:86-113`). So the run
   * went nowhere and reported nothing.
   *
   * Two things follow, and both are asserted:
   *
   *  a) the refusal is real and specific — not a 500, not a timeout, and not a
   *     200 that quietly indexes instead of searching (which would be worse
   *     than a refusal);
   *  b) `index_data` on the SAME route in the SAME breath is accepted, so (a)
   *     is a statement about the TOOL and not about a broken route, an expired
   *     session, or a bad payload.
   *
   * The client-side half — that pressing Run now reports the missing transport
   * instead of silently doing nothing — is asserted in
   * `src/features/toolkits/lib/hooks/useToolkitChat.hooks.test.tsx`
   * ("reports a search run that has no transport"), where the socket's
   * connection state can be set directly.
   */
  const searchAttempt = await page.request.post(`${BASE_URL}/api/v2/elitea_core/test_toolkit_tool/prompt_lib/${projectId}?await_response=false`, {
    data: {
      tool_name: 'search_index_data',
      toolkit_config: { toolkit_id: Number(TOOLKIT_ID) },
      tool_parameters: { query: 'anything' },
    },
    timeout: 30_000,
  });

  // (a) A validation refusal, naming the field and the only accepted value.
  expect(
    searchAttempt.status(),
    `a search run must be REFUSED, not accepted or crashed. Body: ${(await searchAttempt.text()).slice(0, 400)}`,
  ).toBe(422);
  expect(
    await searchAttempt.text(),
    'the refusal must name the tool restriction, so a future reader knows search is unserved rather than broken',
  ).toContain('index_data');

  // (b) The control. Same route, same session, same toolkit — only the tool
  // name differs. A blanket 422 would fail here, which is what makes (a) a
  // statement about search rather than about the request.
  const indexAttempt = await page.request.post(`${BASE_URL}/api/v2/elitea_core/test_toolkit_tool/prompt_lib/${projectId}?await_response=false&execution_contract=index.ingest.v1`, {
    data: {
      tool_name: 'index_data',
      toolkit_config: { toolkit_id: Number(TOOLKIT_ID) },
      tool_parameters: { index_name: uniqueIndexName() },
    },
    timeout: 60_000,
  });
  expect(
    indexAttempt.status(),
    `the same route must ACCEPT index_data, or the refusal above says nothing about search. Body: ${(await indexAttempt.text()).slice(0, 400)}`,
  ).toBe(200);
});
