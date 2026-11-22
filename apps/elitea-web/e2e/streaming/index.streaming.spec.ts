/**
 * Index definition-of-done journey (#93 "Surface A", validating #296).
 *
 * The behavioural gate for the OTHER product loop that runs on the execution
 * plane: open a toolkit, fill the create-index form, start a run, watch it
 * progress, and see it terminate — all through the product's own UI.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS FILE IS NOT UNDER e2e/journeys/
 * ─────────────────────────────────────────────────────────────────────────────
 * Same reason as `chat.streaming.spec.ts`: the `chromium`/`webkit` projects run
 * against `docker-compose.e2e-standalone.yml`, which has no runtime plane and
 * no agent worker, so an index run cannot happen there at all. This journey
 * gets its own Playwright project (`index-stream`), driven by
 * `scripts/index-stream-e2e.sh`, which also runs `seed-index` — the three rows
 * (vector store, embedding model, indexable toolkit) without which the start
 * route cannot resolve a toolkit.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT MAKES THESE ASSERTIONS DISCRIMINATING
 * ─────────────────────────────────────────────────────────────────────────────
 * Until commit 5f0fe33d, elitea-main served every toolkit tool's argument
 * schema as the placeholder `{"type":"object"}`. The client builds this form
 * from `properties.selected_tools.args_schemas.index_data`, so it rendered ZERO
 * inputs and `isValidForm` was pinned false (`IndexDetails.tsx`: "if
 * (!adjustedIndexDataSchema?.properties) return false") — the `Index` button
 * was permanently disabled and an index could not be started from the UI at
 * all, while every unit suite stayed green.
 *
 * §1 therefore asserts SCHEMA-DERIVED DOM ATTRIBUTES, not merely "a field
 * exists": `maxlength="32"` comes from the served `index_name.maxLength`, and
 * the slider's `min`/`max` come from `progress_step`'s 0-100 bounds. Neither
 * value can be produced by `{"type":"object"}`, and neither is hardcoded
 * anywhere in the client — so this section fails against the pre-5f0fe33d
 * backend and passes only against the real schema.
 *
 * §4's terminal state is likewise not free. The client writes an OPTIMISTIC
 * `state: in_progress` row into its own store the moment `Index` is clicked
 * (`useToolkitChat.hooks.ts`), so a run that never reaches the browser still
 * paints a rail entry. Only the SSE terminal frame flips the action bar to
 * edit mode. `the terminal state comes off the stream, not a client-side
 * guess` (below) is the negative guard that proves it: with the events stream
 * blocked, the very same click must NOT reach that terminal state.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * §7 — THE PIPELINE, NOT ONLY THE SCREEN (issue #470)
 * ─────────────────────────────────────────────────────────────────────────────
 * This journey used to index ZERO documents and still finish green, and its own
 * header recorded that as expected: the SDK listed its bucket over an
 * S3-compatible API that elitea-main did not serve, and swallowed the 404 into
 * an empty listing. Commits c951afe2, f54e3a82 and 2ef4f462 now serve that
 * surface, so the reason has gone.
 *
 * A `0 / 0` journey proved the screen works and proved nothing about the
 * 18-hop embedding path underneath it. Two real defects on that path (#468,
 * #458) were found by reading and passed every gate.
 *
 * §0 therefore PUTS A DOCUMENT IN THE BUCKET first, through the product's own
 * artifact API, and §7 then asserts three things a green run cannot give on its
 * own:
 *
 *   - the server's own index record reports at least one indexed document;
 *   - the provider was really called, read from the mock's request journal;
 *   - the call carried the expected model NAME and the caller's own project
 *     credential. A vector width identifies no model, and a 200 identifies no
 *     project.
 *
 * MEASURED, and it matters for how these assertions are read: a run over an
 * EMPTY bucket still makes 2 single-input embedding calls, from the vector
 * store's own set-up, and reports `indexed: 0` with state `completed`. So "an
 * embedding call happened" is NOT by itself evidence that a document was
 * embedded. §7a — the stored document count — is the assertion that carries
 * that, and §7b to §7d exist to identify the model and the project, not the
 * count.
 *
 * The journal is served by `deploy/mock-llm/server.py` and published on
 * STANDALONE_MOCK_PORT. `seed-llm` gives each project its own mock key, so the
 * credential the gateway sends names the project it resolved.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';

/**
 * The mock provider's request journal. Published by
 * `deploy/docker-compose.standalone-full.yml`; the port is overridable there
 * and here so two stacks can coexist. STANDALONE_MOCK_ORIGIN replaces the
 * whole origin on the native stack, which publishes nothing on localhost
 * (e2e/fixtures/api.ts MOCK_HOST).
 */
const MOCK_JOURNAL_URL = `${
  process.env.STANDALONE_MOCK_ORIGIN || `http://localhost:${process.env.STANDALONE_MOCK_PORT ?? '8090'}`
}/__journal`;

/** One recorded upstream request. See `deploy/mock-llm/server.py`. */
type JournalEntry = {
  readonly path: string;
  readonly model: string | null;
  readonly credential: string;
  readonly inputs: number;
};

/** The bucket `seed-index` points toolkit 9002 at. */
const BUCKET = 'elitea-artifacts';

/**
 * The wire model name the gateway is expected to dispatch.
 *
 * `seed-index` seeds `vllm/E2E-MOCK-EMBEDDING`. The `vllm/` prefix selects the
 * credential and is stripped before dispatch, so the provider sees the bare
 * name. Overridable for a stack seeded with another model.
 */
const EXPECTED_WIRE_MODEL = (process.env.INDEX_EMBEDDING_MODEL ?? 'vllm/E2E-MOCK-EMBEDDING').replace(/^.*\//, '');

/** The toolkit `seed-index` provisions: an `artifact` toolkit, the one indexable type needing no third-party credential. */
const TOOLKIT_ID = '9002';

/** Matched WITHOUT pinning a project id — the driver acts inside its own personal project (see `STORAGE_STATE.chat`). */
const START_RE = /\/elitea_core\/test_toolkit_tool\/prompt_lib\/(\d+)/;
const EVENTS_RE = /\/executions\/(\d+)\/[^/]+\/events/;

function uniqueIndexName(): string {
  // `index_name` is capped at 32 chars by the served schema, and §1 asserts
  // that cap — so the value this run types has to fit inside it.
  const name = `e2e${Date.now() % 100_000_000}`;
  expect(name.length, 'the index name must fit the schema’s 32-char cap').toBeLessThanOrEqual(32);
  return name;
}

/** The rail's own list request — awaited before "Add index" is clicked. See `openCreateIndexForm`. */
const INDEX_META_RE = /\/elitea_core\/index_meta\/prompt_lib\/(\d+)\/\d+/;

/**
 * Empties the mock provider's request journal, so a later read reports only the
 * calls this run made.
 */
async function resetJournal(page: Page): Promise<void> {
  const cleared = await page.request.delete(MOCK_JOURNAL_URL, { timeout: 15_000 });
  expect(
    cleared.ok(),
    `the mock provider's journal is not reachable at ${MOCK_JOURNAL_URL}. The llm-mock service must publish STANDALONE_MOCK_PORT — see deploy/docker-compose.standalone-full.yml.`,
  ).toBe(true);
}

/** Reads the mock provider's request journal. */
async function readJournal(page: Page): Promise<readonly JournalEntry[]> {
  const response = await page.request.get(MOCK_JOURNAL_URL, { timeout: 15_000 });
  expect(response.ok(), `the mock provider's journal is not readable at ${MOCK_JOURNAL_URL}`).toBe(true);
  const body = (await response.json()) as { data?: readonly JournalEntry[] };
  return body.data ?? [];
}

/**
 * Puts ONE document in the bucket the toolkit indexes, through the product's
 * own artifact API — the same two calls `seed-index` prints.
 *
 * Without this the run has nothing to embed. It reports "No new documents to
 * index.", terminates green, and makes no embedding call at all, so every
 * assertion below it would measure the screen and not the pipeline.
 *
 * The bucket may already exist from an earlier run, and 409 is the success of
 * that case. Anything else is a real failure and must not be swallowed: a
 * swallowed error here would put the journey straight back to indexing nothing.
 */
async function seedIndexableDocument(page: Page, projectId: string, body: string): Promise<string> {
  const created = await page.request.post(`${BASE_URL}/api/v2/artifacts/buckets/${projectId}`, {
    data: { name: BUCKET },
    timeout: 30_000,
  });
  expect(
    [200, 201, 409],
    `creating the bucket answered ${created.status()}: ${(await created.text()).slice(0, 300)}`,
  ).toContain(created.status());

  // The multipart FILENAME is the object key, and the extension decides whether
  // the toolkit treats the object as a document at all — an unknown extension
  // is skipped, which would leave the run at zero documents again.
  const key = `e2e-${Date.now()}.txt`;
  const uploaded = await page.request.post(
    `${BASE_URL}/api/v2/artifacts/objects/${projectId}/${BUCKET}?overwrite=true`,
    {
      multipart: { file: { name: key, mimeType: 'text/plain', buffer: Buffer.from(body, 'utf8') } },
      timeout: 30_000,
    },
  );
  expect(
    uploaded.ok(),
    `uploading ${key} answered ${uploaded.status()}: ${(await uploaded.text()).slice(0, 300)}`,
  ).toBe(true);

  // Read it back through the SAME S3-shaped listing the toolkit uses. A
  // successful upload does not prove the toolkit can see the object: the
  // listing is a separate root-mounted route, and the SDK swallows its 404 into
  // an empty listing with no error anywhere. That swallow is what kept this
  // journey at zero documents, so it is asserted here rather than trusted.
  const listed = await page.request.get(
    `${BASE_URL}/artifacts/s3/${BUCKET}?project_id=${projectId}&format=json&list-type=2`,
    { timeout: 30_000 },
  );
  expect(
    listed.ok(),
    `the toolkit's own bucket listing answered ${listed.status()}. The toolkit would read this as an empty bucket and index nothing.`,
  ).toBe(true);
  expect(
    await listed.text(),
    'the object must appear in the listing the toolkit reads, not only in the upload response',
  ).toContain(key);
  return key;
}

/**
 * Opens the toolkit's Indexes tab and clicks "Add index", leaving the create
 * form on screen. Returns the project id the rail's own list request used —
 * the driver's personal project, which is where the run happens and where the
 * gateway resolves the credential.
 */
async function openCreateIndexForm(page: Page): Promise<string> {
  await page.goto(`${BASE_URL}/app/toolkits/all/${TOOLKIT_ID}`, { waitUntil: 'domcontentloaded' });

  // The tab is not unconditional: `resolveIndexesTabVisibility` hides it for any
  // toolkit type whose schema offers no indexing tool. Its presence is already a
  // statement that `args_schemas` reached the client with `index_data` in it.
  await expect(
    page.getByRole('tab', { name: 'Indexes' }),
    'the Indexes tab is only offered when the served type schema advertises an indexing tool',
  ).toBeVisible({ timeout: 30_000 });

  // The rail's list is awaited BEFORE "Add index" is clicked, because
  // `IndexesContainer` auto-selects an index once that query resolves — a
  // selection that replaces the create form with the selected index's detail
  // view. Clicking as soon as the panel appears therefore races the list, and
  // the create form silently disappears half the time. Measured, and the race
  // gets worse as the list grows: each stored index embeds the whole default
  // `chunking_config` map, so the response was already 444 KB after a dozen
  // runs on this stack.
  const listed = page.waitForResponse((r) => INDEX_META_RE.test(r.url()), { timeout: 30_000 });
  await page.getByRole('tab', { name: 'Indexes' }).click();
  await expect(page.getByTestId('edit-toolkit-indexes-tab-panel')).toBeVisible({ timeout: 15_000 });
  const listResponse = await listed;

  await page.getByRole('button', { name: 'Add index' }).click();
  // The create form's own field, so a swallowed or overridden click fails here
  // rather than three assertions later against a detail view.
  await expect(page.getByLabel('Index Name', { exact: true })).toBeVisible({ timeout: 20_000 });

  const projectId = INDEX_META_RE.exec(listResponse.url())?.[1] ?? '';
  expect(projectId, 'the rail must list this toolkit inside a project').not.toBe('');
  return projectId;
}

test('the index loop works end to end: form, start, progress, terminal state', async ({ page }) => {
  // A whole run — start, dispatch to the worker, the tool's node events and the
  // terminal projection — plus two page loads. The individual waits below are
  // each bounded well under this, so a real hang fails on its own step.
  test.setTimeout(180_000);

  const consoleErrors: string[] = [];
  page.on('console', (message) => {
    if (message.type() === 'error') consoleErrors.push(message.text());
  });

  // ── The negative guard: count every request the Index click makes ──
  // The failure mode this kills is a button that flips the UI optimistically
  // and never reaches the server. Counting real requests — not spinners — is
  // what makes that impossible to pass.
  const runRequests: string[] = [];
  page.on('request', (request) => {
    const url = request.url();
    if (START_RE.test(url) || EVENTS_RE.test(url)) runRequests.push(`${request.method()} ${url}`);
  });

  const projectId = await openCreateIndexForm(page);

  // ── §0. Give the run something to index ───────────────────────────────────
  // One document, put in the bucket through the product's own artifact API.
  // Its body is unique to this run, so the chunk that reaches the provider
  // cannot come from an earlier run or from a fixture.
  const documentBody = `elitea index journey document ${String(Date.now())}`;
  await seedIndexableDocument(page, projectId, documentBody);
  // Bound the journal to this run alone. Every embedding call read in §7 was
  // made after this line.
  await resetJournal(page);

  // ── §1. The form draws the SERVED schema ──────────────────────────────────
  // Every control below is one property of `args_schemas.index_data`. Before
  // 5f0fe33d this section found nothing at all: `page.locator('input,textarea')`
  // returned [].
  const nameField = page.getByLabel('Index Name', { exact: true });
  await expect(nameField, 'the required index_name field must render').toBeVisible({ timeout: 20_000 });
  await expect(nameField, 'index_name is `required` in the served schema').toHaveAttribute('required', '');
  // The schema-derived attribute. `{"type":"object"}` carries no maxLength, and
  // 32 appears nowhere in the client — this is the assertion that pins #296.
  await expect(nameField, 'index_name’s maxLength: 32 must reach the DOM').toHaveAttribute('maxlength', '32');

  const cleanIndex = page.locator('input[name="clean_index"]');
  await expect(cleanIndex, 'the clean_index boolean must render as a checkbox').toHaveAttribute('type', 'checkbox');
  await expect(page.getByLabel('Folder', { exact: true }), 'the folder string must render').toBeVisible();

  // progress_step's 0-100 bounds, likewise schema-derived.
  const progressStep = page.getByLabel('Progress Step', { exact: true });
  await expect(progressStep, 'progress_step’s minimum: 0 must reach the DOM').toHaveAttribute('min', '0');
  await expect(progressStep, 'progress_step’s maximum: 100 must reach the DOM').toHaveAttribute('max', '100');

  // The three non-scalar properties render as editors rather than <input>s,
  // which is exactly why an input-count assertion alone would under-report the
  // form. Named individually so a silently-dropped property fails here.
  for (const editor of ['Chunking Config', 'Include Extensions', 'Skip Extensions']) {
    await expect(page.getByRole('textbox', { name: editor }), `the ${editor} property must render`).toBeVisible();
  }

  // The console error the placeholder schema used to produce, asserted absent.
  expect(
    consoleErrors.filter((text) => text.includes('Invalid schema object')),
    'the form must not reject the schema it was given',
  ).toEqual([]);

  // ── §2. The Index button is driven by the schema’s `required`, both ways ──
  // Pre-5f0fe33d the button was permanently disabled, so "enabled after typing"
  // is the discriminator. "Disabled while empty" is the other half: it fails a
  // button stubbed always-on, and proves the enablement tracks `required`.
  const indexButton = page.getByRole('button', { name: 'Index', exact: true });
  await expect(indexButton, 'an empty required field must keep Index disabled').toBeDisabled();

  const indexName = uniqueIndexName();
  await nameField.fill(indexName);
  await expect(indexButton, 'filling the required field must enable Index').toBeEnabled({ timeout: 10_000 });

  // ── §3. Progress observed DURING the run ──────────────────────────────────
  // The sampler starts WITH the click, not after the response assertions: the
  // in-progress window is ~2.8s (measured), and a sampler that only opens once
  // those promises resolve can report "nothing was painted" about a run it
  // never watched.
  const panel = page.getByTestId('edit-toolkit-indexes-tab-panel');
  const stopButton = panel.getByRole('button', { name: 'Stop' });
  const inProgress: string[] = [];
  const sampler = (async () => {
    const deadline = Date.now() + 60_000;
    while (Date.now() < deadline) {
      if ((await stopButton.count().catch(() => 0)) > 0) inProgress.push(`t+${String(Date.now())}`);
      // The terminal control; once it exists the run is over and sampling stops.
      if ((await panel.getByRole('button', { name: 'Reindex' }).count().catch(() => 0)) > 0) return;
      await page.waitForTimeout(25);
    }
  })();

  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', { timeout: 40_000 });
  const streamed = page.waitForResponse((r) => EVENTS_RE.test(r.url()), { timeout: 40_000 });
  await indexButton.click();

  const startResponse = await started;
  expect(startResponse.status(), 'the index run must be admitted').toBe(200);
  const startBody = (await startResponse.json()) as { task_id?: string };
  expect(startBody.task_id, 'the start response must carry the task to stream').toMatch(/^[0-9a-f]+$/);
  expect(
    START_RE.exec(startResponse.url())?.[1] ?? '',
    'the run must happen in the project the document was seeded into',
  ).toBe(projectId);

  const streamResponse = await streamed;
  expect(streamResponse.status(), 'the browser must be able to READ the stream it is meant to render').toBe(200);
  expect(streamResponse.headers()['content-type'] ?? '', 'the run stream must be served as SSE').toContain('text/event-stream');

  // The in-progress control is a real intermediate state: it exists only while
  // the run is open, and is replaced by the terminal bar below.
  await sampler;
  expect(
    inProgress.length,
    'no in-progress state was ever painted — the UI jumped straight to a terminal state, so nothing about the run was actually observed',
  ).toBeGreaterThan(0);

  // ── §4. A visible terminal state ──────────────────────────────────────────
  // The create-mode bar (Cancel/Index) is replaced by the edit-mode bar, which
  // only happens on the terminal frame. Asserted as a control the user can see
  // and click, not as an internal flag.
  await expect(
    panel.getByRole('button', { name: 'Reindex' }),
    'the run must reach a visible terminal state rather than hanging',
  ).toBeVisible({ timeout: 60_000 });
  await expect(stopButton, 'the in-progress control must be gone once the run ends').toHaveCount(0);
  await expect(panel.getByText(indexName).first(), 'the finished index must be listed in the rail').toBeVisible();

  // The negative guard, now that the whole lifecycle has been observed.
  expect(runRequests.some((entry) => entry.startsWith('POST')), `the Index click made no start request: ${JSON.stringify(runRequests)}`).toBe(true);
  expect(runRequests.some((entry) => entry.startsWith('GET')), `the run opened no event stream: ${JSON.stringify(runRequests)}`).toBe(true);

  // ── §5. Persistence ───────────────────────────────────────────────────────
  // A rail entry alone proves nothing: the client writes an optimistic row at
  // click time. Reading the server's own list is what separates "an index was
  // created" from "an index was drawn".
  await expect
    .poll(
      async () => {
        const stored = await page.request.get(`${BASE_URL}/api/v2/elitea_core/index_meta/prompt_lib/${projectId}/${TOOLKIT_ID}`);
        if (!stored.ok()) return [];
        const body = (await stored.json()) as readonly { metadata?: { collection?: string } }[];
        return Array.isArray(body) ? body.map((row) => row.metadata?.collection ?? '') : [];
      },
      { timeout: 30_000, message: 'the run finished in the UI but the index was never stored' },
    )
    .toContain(indexName);

  // ── §6. Persistence, through the UI ───────────────────────────────────────
  // A fresh load re-reads everything from the server, so this fails against a
  // stack that painted the run and stored nothing.
  await openCreateIndexForm(page);
  await expect(panel.getByText(indexName).first(), 'the index must survive a reload').toBeVisible({ timeout: 30_000 });

  // ── §7. The PIPELINE ran, not only the screen (issue #470) ────────────────
  //
  // Everything above this line passes against a run that indexed nothing. This
  // section is what separates "the index feature works" from "an embedding was
  // computed for a real document by the model the operator configured".

  // §7a. The server's own record of how many documents the run indexed. This
  // is the number the rail renders, so a `0 / 0` run fails here.
  //
  // MEASURED: an empty bucket gives `indexed: 0` and state `completed`, so this
  // assertion is what separates the two. Do not move the weight of it onto §7b.
  await expect
    .poll(
      async () => {
        const stored = await page.request.get(`${BASE_URL}/api/v2/elitea_core/index_meta/prompt_lib/${projectId}/${TOOLKIT_ID}`);
        if (!stored.ok()) return -1;
        const body = (await stored.json()) as readonly { metadata?: Record<string, unknown> }[];
        const row = Array.isArray(body) ? body.find((entry) => entry.metadata?.['collection'] === indexName) : undefined;
        return Number(row?.metadata?.['indexed'] ?? -1);
      },
      {
        timeout: 30_000,
        message:
          'the run indexed no document. An empty bucket terminates green with indexed: 0, and every section above this one passes against it — MEASURED, by disabling §0 and re-running.',
      },
    )
    .toBeGreaterThanOrEqual(1);

  // §7b. The provider was really called, and the whole 18-hop path was walked.
  // Read from the mock's own request journal — the only record of what went on
  // the wire. Neither the run's terminal state nor the stored count can show
  // this: both are written by elitea-main, upstream of the gateway.
  //
  // This is the CARRIER for §7c and §7d, not a document-count assertion. See
  // the note on §7a.
  const journal = await readJournal(page);
  const embeddingCalls = journal.filter((entry) => entry.path === '/v1/embeddings');
  expect(
    embeddingCalls.length,
    `the run reported indexed documents but made no embedding call. Journal: ${JSON.stringify(journal)}`,
  ).toBeGreaterThanOrEqual(1);

  // §7c. WHICH model. A vector width identifies no model, and that width was
  // the only thing the stack's `check` asserted. The gateway maps the
  // catalogue name onto the row's `data.name` and strips the provider prefix,
  // so this is the name the provider itself was asked for.
  expect(
    [...new Set(embeddingCalls.map((entry) => entry.model))],
    'the embedding call carried a different model name than the one seeded for this toolkit',
  ).toEqual([EXPECTED_WIRE_MODEL]);

  // §7d. WHICH project. `seed-llm` gives every project its own mock key, so
  // the credential the gateway sent names the project it resolved. A gateway
  // that resolved the public project, or another tenant's, would still return
  // a valid-looking vector.
  expect(
    [...new Set(embeddingCalls.map((entry) => entry.credential))],
    `the gateway resolved the wrong project's credential. Expected project ${projectId}.`,
  ).toEqual([`mock-key-project-${projectId}`]);
});

test('the terminal state comes off the stream, not a client-side guess', async ({ page }) => {
  test.setTimeout(120_000);

  // Block ONLY the run's event stream. Everything else — the form, the start
  // request, the optimistic rail row — behaves exactly as in the test above,
  // so the single difference between passing there and failing here is whether
  // the browser actually consumed the server's frames.
  //
  // This is what makes §4 above load-bearing: the client writes `in_progress`
  // into its own store on click, so a UI that invented its terminal state (a
  // timer, an optimistic flip, a fabricated success) would pass that test and
  // fail this one.
  await page.route(EVENTS_RE, (route) => route.abort());

  await openCreateIndexForm(page);
  const nameField = page.getByLabel('Index Name', { exact: true });
  await expect(nameField).toBeVisible({ timeout: 20_000 });
  await nameField.fill(uniqueIndexName());

  const indexButton = page.getByRole('button', { name: 'Index', exact: true });
  await expect(indexButton).toBeEnabled({ timeout: 10_000 });
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', { timeout: 40_000 });
  await indexButton.click();

  // The run really was admitted — this is a severed stream, not a rejected run.
  expect((await started).status(), 'the run must still be admitted with the stream blocked').toBe(200);

  // The run must first genuinely ENTER the in-progress state. Without this the
  // assertion below is vacuous: "Reindex is absent" is trivially true of a page
  // that never started a run, never loaded, or rendered nothing at all — it
  // would pass instantly and measure nothing.
  const panel = page.getByTestId('edit-toolkit-indexes-tab-panel');
  const stopButton = panel.getByRole('button', { name: 'Stop' });
  await expect(stopButton, 'the click must still open a run and paint its in-progress state').toBeVisible({ timeout: 30_000 });

  // Now the real assertion, over a window several times the ~3.4s the happy
  // path takes to terminate (measured). The run stays parked on its
  // in-progress control because the only producer of a terminal state — the
  // event stream — is gone.
  await page.waitForTimeout(15_000);
  await expect(
    panel.getByRole('button', { name: 'Reindex' }),
    'with no event stream the UI must NOT reach a terminal state — reaching one means it was invented client-side',
  ).toHaveCount(0);
  await expect(stopButton, 'the run must still be showing as in progress').toBeVisible();
});
