/**
 * JRNY-INDEX-SAVE — the Indexes tab's "Save" vs "Save & Reindex" split.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT WAS MISSING, AND WHY IT MATTERED
 * ─────────────────────────────────────────────────────────────────────────────
 * The index Configuration tab had NO save. `IndexActionsParts.tsx`'s edit-mode
 * bar rendered "Reindex" and "Delete" and nothing else, and the ONLY writer of
 * an index's stored `index_configuration` was an indexing RUN. So changing
 * `progress_step` on an index holding thousands of documents could be
 * persisted only by re-indexing all of them, and a SCHEDULED reindex — which
 * reads that same stored field
 * (`internal/runtimecomposition/index_schedule_inspector.go`) — kept using the
 * configuration of the last run rather than the one on screen.
 *
 * This file covers the eight `indexing/save-reindex` cases: the button pair
 * and its dirty-state gating, what each button sends, what each reports, and
 * the unsaved-changes guard on navigation.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE INDEX ROW IS INJECTED, AND WHY THAT IS NOT A WEAKER TEST
 * ─────────────────────────────────────────────────────────────────────────────
 * An index metadata row lives in the PROJECT'S OWN PgVector store, written by
 * a real indexing run through the index-ingest plane. This stack composes
 * neither: `deploy/docker-compose.e2e-standalone.yml` sets no index-ingest
 * flag, so `GET .../index_meta/...` is answered by the compatibility handler
 * over `p_<project>.index_meta` (measured: `200 []`), and the reviewed
 * PgVector-backed routes — the list, the delete, the schedule writes and this
 * change's configuration PUT — are not mounted at all. Enabling that plane on
 * the shared journeys stack would change what every other indexes journey
 * reads, mid-wave.
 *
 * So the LIST response is injected, exactly as `indexes.explains.spec.ts`
 * beside this file injects its two, and the PUT is observed at the network
 * layer. Everything else is real: a real toolkit of a real index-capable type,
 * the real served `index_data` argument schema driving the real form, the real
 * bundle, the real router. What is under test here is entirely client-side —
 * which buttons exist in which state, what each one sends, in what order, and
 * what the screen says afterwards — and every one of those is a property of
 * the code this change adds.
 *
 * The SERVER half is proved where it can be proved for real:
 * `internal/application/indexmeta/configuration_test.go` (resolution, refusals,
 * redaction), `internal/api/v2/indexing/index_meta_configuration_test.go`
 * (path, method, RBAC, every refusal's status) and
 * `internal/infra/pgvector/index_meta_configuration_integration_test.go`,
 * which runs the real UPDATE against a real PostgreSQL and asserts that the
 * run-owned fields — `state`, `task_id`, `history`, the counters — are left
 * exactly as they were.
 */
import { expect, test } from '@playwright/test';
import type { Page, Route } from '@playwright/test';

import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import { BASE_URL } from '../../../playwright.config';

/** The rail's list request. */
const INDEX_META_LIST_RE = /\/elitea_core\/index_meta\/prompt_lib\/\d+\/[^/]+$/;
/** The configuration PUT this change adds. */
const INDEX_CONFIG_PUT_RE = /\/elitea_core\/index_meta\/prompt_lib\/\d+\/[^/]+\/[^/]+\/configuration$/;
/** The run dispatch — what "Reindex" and the reindex half of "Save & Reindex" fire. */
const INDEX_RUN_RE = /\/elitea_core\/test_toolkit_tool\/prompt_lib\//;

const INDEX_NAME = 'autotest_saved_index';
const createdIds: string[] = [];

/** The one stored value the dirty comparison is against. */
const STORED_VALUE = 'autotest stored value';

interface IndexFixture {
  readonly toolkitId: string;
  /** The `index_data` argument whose field this journey edits — a REQUIRED string, so clearing it is also the reachable validation failure. */
  readonly editableField: string;
  /** Its rendered label. */
  readonly editableLabel: string;
}

function storedIndexRow(field: string) {
  return [
    {
      id: 'autotest-index-1',
      stale: false,
      metadata: {
        type: 'index_meta',
        collection: INDEX_NAME,
        state: 'completed',
        created_on: 1_750_000_000,
        indexed: 10,
        index_configuration: { index_name: INDEX_NAME, [field]: STORED_VALUE },
      },
    },
  ];
}

/**
 * A toolkit of whichever served type actually offers `index_data` WITH a
 * required string argument beside `index_name`, derived from the LIVE
 * catalogue rather than hardcoded — the rule `indexes.explains.spec.ts`
 * follows, and for the same reason: a literal here would keep passing after
 * the schema changed under it.
 *
 * Required, and a string, on purpose. The same field then serves both
 * gestures this file needs: giving it a new value is the "one configuration
 * value changed" case, and clearing it is the only validation failure the
 * edit form can actually reach (`index_name`, the other required argument, is
 * HIDDEN on an existing index — `IndexDetails.tsx`'s `adjustIndexDataSchema`
 * — so it can never be emptied by hand).
 */
async function createIndexCapableToolkit(page: Page): Promise<IndexFixture> {
  const schemasResp = await page.request.get(`${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`);
  expect(schemasResp.status(), await schemasResp.text()).toBe(200);
  const schemas = (await schemasResp.json()) as Record<string, ToolkitTypeSchema>;

  let picked: { readonly type: string; readonly field: string; readonly label: string } | undefined;
  for (const [type, schema] of Object.entries(schemas)) {
    const indexData = schema.properties?.selected_tools?.args_schemas?.['index_data'];
    const required = (indexData?.required ?? []).filter((name) => name !== 'index_name');
    for (const name of required) {
      const property = indexData?.properties?.[name];
      const isString = property?.type === 'string' || (property?.anyOf ?? []).some((branch) => branch.type === 'string');
      if (isString) {
        picked = { type, field: name, label: property?.title ?? name };
        break;
      }
    }
    if (picked !== undefined) break;
  }
  expect(
    picked,
    `no toolkit type offers index_data with a required string argument; measured types: ${Object.keys(schemas).join(', ')}`,
  ).toBeTruthy();
  const chosen = picked as { readonly type: string; readonly field: string; readonly label: string };

  const createResp = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name: `${AUTOTEST_PREFIX}index_save_${String(Date.now())}`,
      type: chosen.type,
      description: 'JRNY-INDEX-SAVE fixture',
      settings: { selected_tools: ['index_data', 'remove_index'] },
    },
  });
  expect(createResp.status(), await createResp.text()).toBe(201);
  const created = (await createResp.json()) as { id: string };
  createdIds.push(created.id);
  return { toolkitId: created.id, editableField: chosen.field, editableLabel: chosen.label };
}

interface ToolkitTypeSchema {
  readonly properties?: {
    readonly selected_tools?: {
      readonly args_schemas?: Record<
        string,
        {
          readonly required?: readonly string[];
          readonly properties?: Record<string, { readonly type?: string; readonly title?: string; readonly anyOf?: readonly { readonly type?: string }[] }>;
        }
      >;
    };
  };
}

/** Opens the toolkit's Indexes tab with the stored index injected, selects it, and switches to Configuration. */
async function openStoredIndexConfiguration(page: Page) {
  const fixture = await createIndexCapableToolkit(page);
  await page.route(INDEX_META_LIST_RE, (route) =>
    route.request().method() === 'GET'
      ? route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(storedIndexRow(fixture.editableField)) })
      : route.fallback(),
  );

  await page.goto(`${BASE_URL}/app/toolkits/all/${fixture.toolkitId}`, { waitUntil: 'domcontentloaded' });
  await page.getByRole('tab', { name: 'Indexes' }).click();
  const panel = page.getByTestId('edit-toolkit-indexes-tab-panel');
  await expect(panel).toBeVisible({ timeout: 20_000 });

  await panel.getByText(INDEX_NAME).first().click();
  await panel.getByRole('button', { name: 'configuration', exact: true }).click();

  const field = panel.getByRole('textbox', { name: fixture.editableLabel, exact: true }).first();
  await expect(field, `the real index_data schema must render its ${fixture.editableLabel} field`).toBeVisible({ timeout: 20_000 });
  await expect(field, 'the Configuration tab has to be EDITABLE — a read-only form has nothing to save').toBeEditable();
  await expect(field).toHaveValue(STORED_VALUE);
  return { panel, field, fixture };
}

const reindex = (panel: ReturnType<Page['getByTestId']>) => panel.getByRole('button', { name: 'Reindex', exact: true });
const save = (panel: ReturnType<Page['getByTestId']>) => panel.getByTestId('index-config-save');
const saveAndReindex = (panel: ReturnType<Page['getByTestId']>) => panel.getByTestId('index-config-save-reindex');

/*
 * Every test here creates a real toolkit over the API, loads the full toolkit
 * editor, waits for the served type catalogue and then drives the index
 * Configuration form. That is comfortably past the 30s default on a busy
 * shared stack — measured.
 */
test.beforeEach(() => {
  test.setTimeout(150_000);
});

test.afterAll(async ({ browser }) => {
  if (createdIds.length === 0) return;
  const context = await browser.newContext();
  const page = await context.newPage();
  for (const id of createdIds) {
    await page.request.delete(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`).catch(() => undefined);
  }
  await context.close();
});

/* onetest: ELITEA-2883 — Button states and form state management. Ported BY USE CASE: the case drives the Progress Step slider; this journey edits the served `index_data` schema's own required STRING argument, which is the same "one configuration value changes" gesture with a value a journey can set exactly (and, in the validation test below, clear). The rule under test is the case's own: clean form -> Reindex only; any change -> Save + Save & Reindex; reverting the change -> back to Reindex only. */
test('ELITEA-2883: the buttons follow the form — Reindex when clean, Save + Save & Reindex when dirty, and back again on revert', async ({ page }) => {
  const { panel, field } = await openStoredIndexConfiguration(page);

  // Clean.
  await expect(reindex(panel), 'a clean configuration offers only Reindex').toBeVisible({ timeout: 15_000 });
  await expect(save(panel)).toHaveCount(0);
  await expect(saveAndReindex(panel)).toHaveCount(0);

  // Dirty.
  await field.fill('autotest changed value');
  await expect(save(panel), 'a changed configuration offers Save').toBeVisible();
  await expect(saveAndReindex(panel)).toBeVisible();
  await expect(reindex(panel), 'Reindex is withdrawn while there are unsaved edits — it would run the LAST SAVED configuration').toHaveCount(0);

  // Reverted to the stored value -> clean again.
  await field.fill(STORED_VALUE);
  await expect(reindex(panel), 'reverting the change clears the dirty state').toBeVisible();
  await expect(save(panel)).toHaveCount(0);
});

/* onetest: ELITEA-2880 — Save button persists configuration without triggering reindex. */
/* onetest: ELITEA-2886 — Notifications for save operations (the success half; the refusal half is the next test). */
test('ELITEA-2880/2886: Save stores the configuration, says so, and starts no indexing', async ({ page }) => {
  const { panel, field, fixture } = await openStoredIndexConfiguration(page);

  const savedBodies: { readonly index_configuration?: Record<string, unknown> }[] = [];
  await page.route(INDEX_CONFIG_PUT_RE, async (route: Route) => {
    savedBodies.push(route.request().postDataJSON() as { index_configuration?: Record<string, unknown> });
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true }) });
  });
  let runs = 0;
  await page.route(INDEX_RUN_RE, async (route: Route) => {
    runs += 1;
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ task_id: 'never-expected' }) });
  });

  await field.fill('autotest changed value');
  await save(panel).click();

  // The stored configuration is the whole form, with the change in it.
  await expect.poll(() => savedBodies.length, { message: 'Save must PUT the configuration' }).toBe(1);
  expect(savedBodies[0]?.index_configuration?.[fixture.editableField]).toBe('autotest changed value');
  expect(savedBodies[0]?.index_configuration?.['index_name'], 'the whole configuration is stored, not only the field that changed').toBe(INDEX_NAME);

  // It says so.
  await expect(page.getByText('Configuration saved successfully')).toBeVisible({ timeout: 10_000 });

  // And NOTHING was indexed. This is the half the case is actually about.
  expect(runs, 'Save must not dispatch an indexing run').toBe(0);

  // The form is clean again, in the same commit as the save.
  await expect(reindex(panel)).toBeVisible();
  await expect(save(panel)).toHaveCount(0);
});

/* onetest: ELITEA-2881 — Save & Reindex validates and saves config first then starts reindexing. */
/* onetest: ELITEA-2887 — Configuration persistence and reindex usage. Ported BY USE CASE: the case's "manual reindex uses the latest SAVED configuration" is asserted as what the two requests carry — the save is sent first and the run that follows carries the same values — because this stack composes no index-ingest plane to run a real index against (see this file's header). */
test('ELITEA-2881/2887: Save & Reindex saves FIRST, then runs — and the run carries what was just saved', async ({ page }) => {
  const { panel, field, fixture } = await openStoredIndexConfiguration(page);

  const order: string[] = [];
  let savedConfiguration: Record<string, unknown> | undefined;
  let runParams: Record<string, unknown> | undefined;
  await page.route(INDEX_CONFIG_PUT_RE, async (route: Route) => {
    order.push('save');
    savedConfiguration = (route.request().postDataJSON() as { index_configuration: Record<string, unknown> }).index_configuration;
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true }) });
  });
  await page.route(INDEX_RUN_RE, async (route: Route) => {
    order.push('run');
    runParams = (route.request().postDataJSON() as { tool_params?: Record<string, unknown> }).tool_params;
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ task_id: 'autotest-task' }) });
  });

  await field.fill('autotest changed value');
  await saveAndReindex(panel).click();

  await expect.poll(() => order.join(','), { message: 'the save has to commit before the run reads it', timeout: 20_000 }).toBe('save,run');
  expect(savedConfiguration?.[fixture.editableField]).toBe('autotest changed value');
  expect(runParams?.[fixture.editableField], 'the reindex runs the configuration that was just saved').toBe('autotest changed value');
  await expect(page.getByText('Configuration saved, reindexing started')).toBeVisible({ timeout: 10_000 });
});

/* onetest: ELITEA-2882 — Save & Reindex does not start reindex when validation fails. */
test('ELITEA-2882: an invalid configuration blocks BOTH the save and the reindex, and says why', async ({ page }) => {
  const { panel, field } = await openStoredIndexConfiguration(page);

  let saves = 0;
  let runs = 0;
  await page.route(INDEX_CONFIG_PUT_RE, async (route: Route) => {
    saves += 1;
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true }) });
  });
  await page.route(INDEX_RUN_RE, async (route: Route) => {
    runs += 1;
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ task_id: 'never-expected' }) });
  });

  /*
   * PORTED BY USE CASE. The case types malformed JSON into the Chunking
   * Config editor. This app's JSON field coerces unparsable text to `{}`
   * before it ever reaches the form (`shared/ui/CommonObjectField`, a
   * pre-existing shared primitive this package does not own) — which is what
   * the case itself observed as "JSON disappeared" — so that gesture produces
   * a VALID empty object rather than a validation failure. The reachable
   * validation failure on this form is the required `index_name`, and it
   * exercises exactly the rule the case is about: the refusal covers both
   * halves and nothing is dispatched.
   */
  await field.fill('');

  await saveAndReindex(panel).click();
  await expect(page.getByText(/configuration is not valid/i)).toBeVisible({ timeout: 10_000 });
  expect(runs, 'no reindex may start from an invalid configuration').toBe(0);
  expect(saves, 'no configuration may be stored that the form itself rejects').toBe(0);

  // The edits survive the refusal, so the user can correct and retry.
  await expect(save(panel), 'the form stays dirty after a refused save').toBeVisible();
  await expect(field).toHaveValue('');
});

/* onetest: ELITEA-2886 — Notifications for save operations (the ERROR half). */
test('ELITEA-2886: a refused save is reported as a failure and the edits are kept', async ({ page }) => {
  const { panel, field } = await openStoredIndexConfiguration(page);

  await page.route(INDEX_CONFIG_PUT_RE, (route) =>
    route.fulfill({
      status: 400,
      contentType: 'application/json',
      body: JSON.stringify({ ok: false, error: 'index_configuration must be a JSON object' }),
    }),
  );

  await field.fill('autotest changed value');
  await save(panel).click();

  await expect(page.getByText('The configuration could not be saved')).toBeVisible({ timeout: 10_000 });
  await expect(page.getByText('Configuration saved successfully')).toHaveCount(0);
  await expect(save(panel), 'a refused save must leave the form dirty — the edits are the user\'s only copy').toBeVisible();
  await expect(field).toHaveValue('autotest changed value');
});

/* onetest: ELITEA-2885 — Unsaved changes warning on navigation with dismiss and confirm options. */
/* onetest: ELITEA-2884 — Scheduled reindexing uses latest saved config not unsaved changes. Ported BY USE CASE: no scheduler runs on this stack (the index-ingest plane is not composed — see this file's header), and the property the case is really asserting is that UNSAVED edits never leave the browser. That is what this test proves: navigating away with the guard, discarding, and returning shows the STORED values, and no configuration request was ever sent. */
test('ELITEA-2884/2885: leaving with unsaved edits is guarded, and discarded edits never reach the server', async ({ page }) => {
  const { panel, field } = await openStoredIndexConfiguration(page);

  let saves = 0;
  await page.route(INDEX_CONFIG_PUT_RE, async (route: Route) => {
    saves += 1;
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ ok: true }) });
  });

  await field.fill('autotest changed value');
  await expect(save(panel)).toBeVisible();

  // Sidebar navigation is blocked while there are unsaved edits.
  await page.getByRole('link', { name: 'Agents', exact: true }).first().click();
  const guard = page.getByRole('dialog');
  await expect(guard, 'navigating away from unsaved edits must be guarded').toBeVisible({ timeout: 10_000 });
  await expect(guard).toContainText(/unsaved changes/i);

  // Staying keeps the page AND the edits.
  await guard.getByRole('button', { name: /cancel|stay/i }).first().click();
  await expect(guard).toHaveCount(0);
  expect(page.url(), 'cancelling the guard must not navigate').toContain('/app/toolkits/all/');
  await expect(field).toHaveValue('autotest changed value');

  // Leaving discards them.
  await page.getByRole('link', { name: 'Agents', exact: true }).first().click();
  await expect(page.getByRole('dialog')).toBeVisible({ timeout: 10_000 });
  await page.getByRole('dialog').getByRole('button', { name: /confirm|leave|discard|ok/i }).first().click();
  await page.waitForURL(/\/app\/agents/, { timeout: 15_000 });

  // And nothing was ever sent: an unsaved edit is not a stored configuration,
  // which is the whole premise of "the schedule uses the last SAVED one".
  expect(saves, 'a discarded edit must never reach the server').toBe(0);
});
