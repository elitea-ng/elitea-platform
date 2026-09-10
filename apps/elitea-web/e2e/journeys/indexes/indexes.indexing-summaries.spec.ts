/**
 * The Index History tab renders a FORMATTED, categorised summary of what an
 * indexing run did — not raw JSON, not a bare chunk count.
 *
 * Ported by use case from the w1-toolkits-indexes package's
 * `indexing-summaries` cases (ELITEA-2804, ELITEA-2806, ELITEA-2808,
 * ELITEA-2809; ELITEA-2830 is a byte-for-byte duplicate of 2809's title and
 * body — recorded DUP). `IndexingReportSummary.tsx` /
 * `indexingReport.serialize.ts` are real, already-built product code
 * (`features/toolkits/indexes/lib/helpers/indexingReport.serialize.ts`'s own
 * doc comment: "the data is real and ALREADY REACHABLE" off
 * `GET /elitea_core/index_meta/prompt_lib/{project}/{toolkit}`'s `metadata`
 * blob) — this file exercises the RENDERING of that real component, not a
 * fabricated one.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE INDEX ROW ITSELF IS SYNTHETIC
 * ─────────────────────────────────────────────────────────────────────────────
 * Producing a REAL indexing run with a mixed outcome (some files indexed,
 * some skipped, some unsupported, one failed) needs a real embedding model
 * and a prepared source bucket — the chat-stream lane, not this one (see
 * `S/port/ledger-P10-admin-ops.tsv`'s STREAM-DEFERRED entries for the
 * reindex-preservation cases, which DO need that). What this file needs is
 * only ONE index row whose `metadata.history` carries a canonical `report`
 * blob — and the whole list this screen reads
 * (`GET…/index_meta/prompt_lib/{project}/{toolkitId}`) is ONE request
 * (`indexesApi.ts`'s `getIndexesList`), read once into the client store with
 * no further per-index fetch (`IndexViews.tsx`'s `index?.metadata['history']`
 * is the SAME object the list call already returned). So the whole screen is
 * exercisable by intercepting that one list read, on a REAL index-capable
 * toolkit — exactly the technique `indexes.explains.spec.ts` already uses for
 * this same route, on this same tab.
 *
 * ELITEA-2805 ("Toolkit Run History", the GENERIC per-toolkit run log —
 * `shared/ui/ViewRunHistoryButton`, `entities/run-history/ui/
 * RunHistoryPanel.tsx`) is a DIFFERENT screen from the Index-specific History
 * tab this file covers, and it lives under the toolkits area a concurrent
 * package owns this run — recorded REROUTE toolkits. ELITEA-2807
 * (source-appropriate terminology: "files"/"pages"/"issues") is NA: the
 * report's item labels are a single hardcoded default pair
 * (`DEFAULT_INDEXING_ITEM_LABELS = {singular: 'document', plural:
 * 'documents'}`, `indexingReport.constants.ts`) — there is no per-toolkit-type
 * terminology map to select between.
 */
import { expect, test } from '@playwright/test';
import type { Page } from '@playwright/test';

import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import { BASE_URL } from '../../../playwright.config';

const INDEX_META_RE = /\/elitea_core\/index_meta\/prompt_lib\//;
const COLLECTION_NAME = `${AUTOTEST_PREFIX}indexing_summary_${Date.now()}`;

const createdIds: string[] = [];

/** An index-capable toolkit, the same helper shape `indexes.explains.spec.ts` uses (type derived from the live catalogue, never hardcoded). */
async function createIndexCapableToolkit(page: Page): Promise<{ readonly id: string }> {
  const schemasResp = await page.request.get(`${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`);
  expect(schemasResp.status(), await schemasResp.text()).toBe(200);
  const schemas = (await schemasResp.json()) as Record<
    string,
    { properties?: { selected_tools?: { args_schemas?: Record<string, unknown> } } }
  >;
  const indexingType = Object.entries(schemas).find(
    ([, schema]) => schema.properties?.selected_tools?.args_schemas?.['index_data'] !== undefined,
  )?.[0];
  expect(indexingType, 'no toolkit type offers index_data').toBeTruthy();

  const createResp = await page.request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      // `Date.now()` alone is not run-unique under `--repeat-each` +
      // multiple workers: two calls across worker PROCESSES can land on the
      // same millisecond. The random suffix is what actually guarantees one
      // toolkit row per test invocation.
      name: `${AUTOTEST_PREFIX}indexing_summary_tk_${String(Date.now())}_${Math.random().toString(36).slice(2, 8)}`,
      type: indexingType,
      description: 'indexes.indexing-summaries.spec.ts fixture',
      settings: { selected_tools: ['index_data'] },
    },
  });
  expect(createResp.status(), await createResp.text()).toBe(201);
  const created = (await createResp.json()) as { id: string };
  createdIds.push(created.id);
  return { id: created.id };
}

/**
 * A canonical `report` blob, in the shape `fromCanonicalReport`
 * (`indexingReport.serialize.ts`) reads: `totals` plus one `groups` entry
 * per non-empty category, ordered `indexed → skipped → not_indexed →
 * failed` (`INDEXING_REPORT_KIND_ORDER`).
 */
const REPORT = {
  status: 'partly_indexed',
  totals: {
    indexed: 5,
    skipped: 2,
    not_indexed: 2,
    failed: 1,
    unchanged: 0,
    dependent_not_indexed: 0,
    total: 10,
  },
  categories: [
    { kind: 'indexed', count: 5, groups: [] },
    {
      kind: 'skipped',
      count: 2,
      groups: [
        {
          reason: 'empty',
          label: 'Contained no indexable content',
          count: 2,
          items: ['empty-one.pdf', 'empty-two.pdf'],
        },
      ],
    },
    {
      kind: 'not_indexed',
      count: 2,
      groups: [
        {
          reason: 'unsupported_format',
          label: 'Unsupported format',
          count: 2,
          items: ['.ai', '.raw'],
        },
      ],
    },
    {
      kind: 'failed',
      count: 1,
      groups: [
        {
          reason: 'processing_error',
          label: 'Could not be processed',
          count: 1,
          items: ['broken.docx'],
        },
      ],
    },
  ],
  errors: [],
  errors_total: 0,
};

async function mockIndexMetaList(page: Page, toolkitId: string): Promise<void> {
  await page.route(INDEX_META_RE, (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify([
        {
          id: `${toolkitId}-e2e-index`,
          stale: false,
          metadata: {
            state: 'partly_indexed',
            collection: COLLECTION_NAME,
            created_on: Math.floor(Date.now() / 1000),
            indexed: 5,
            history: [
              {
                updated_on: Math.floor(Date.now() / 1000),
                conversation_id: null,
                state: 'partly_indexed',
                report: REPORT,
              },
            ],
          },
        },
      ]),
    }),
  );
}

async function openIndexesTab(page: Page, toolkitId: string) {
  await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 20_000 });
  const indexesTab = page.getByRole('tab', { name: 'Indexes' });
  await expect(indexesTab).toBeVisible({ timeout: 20_000 });
  await indexesTab.click();
  const panel = page.getByTestId('edit-toolkit-indexes-tab-panel');
  await expect(panel).toBeVisible({ timeout: 15_000 });
  return panel;
}

test.afterAll(async ({ browser }) => {
  if (createdIds.length === 0) return;
  const context = await browser.newContext();
  const page = await context.newPage();
  for (const id of createdIds) {
    // SINGULAR `tool`, not `tools` — the item-delete route, distinct from
    // the plural collection route the create above uses
    // (`e2e/fixtures/api.ts`'s `deleteGithubToolkit` uses the same singular
    // path; measured directly: plural here answers 404 and leaves the row).
    await page.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`).catch(() => undefined);
  }
  await context.close();
});

test('ELITEA-2804 + ELITEA-2806: the History tab shows a categorised summary, not raw JSON', async ({ page }) => {
  const toolkit = await createIndexCapableToolkit(page);
  await mockIndexMetaList(page, toolkit.id);
  const panel = await openIndexesTab(page, toolkit.id);

  // `.first()`: once selected, the index's collection name is rendered
  // TWICE (the list row AND the details header). The view toggler's tabs
  // are plain text, not an ARIA `tablist`/`tab` — and its lowercase DOM text
  // (`EditViewTabsEnum.history === 'history'`) is capitalised only by CSS
  // `text-transform`, which Playwright's text engine does not undo — so this
  // is located by that lowercase text, not by role or by "History".
  await panel.getByText(COLLECTION_NAME).first().click();
  await page.getByText('history', { exact: true }).click();

  // The one history entry auto-selects on mount (`IndexHistory.tsx`'s own
  // effect selects `history[history.length - 1]`), so the report renders
  // with no further click.
  const summary = page.getByTestId('indexing-report-summary');
  await expect(summary).toBeVisible({ timeout: 15_000 });

  // ELITEA-2804: no raw JSON anywhere in the summary.
  await expect(summary).not.toContainText('"indexed":5');
  await expect(summary).not.toContainText('{"indexed"');

  // ELITEA-2806: all four categories, with their real counts.
  await expect(page.getByTestId('indexing-report-category-indexed')).toContainText('5');
  const skipped = page.getByTestId('indexing-report-category-skipped');
  await expect(skipped).toContainText('2');
  await expect(skipped).toContainText('Contained no indexable content');
  const notIndexed = page.getByTestId('indexing-report-category-not_indexed');
  await expect(notIndexed).toContainText('2');
  const failed = page.getByTestId('indexing-report-category-failed');
  await expect(failed).toContainText('1');
  await expect(failed).toContainText('Could not be processed');
});

test('ELITEA-2808: skipped items carry a human-readable reason', async ({ page }) => {
  const toolkit = await createIndexCapableToolkit(page);
  await mockIndexMetaList(page, toolkit.id);
  const panel = await openIndexesTab(page, toolkit.id);

  await panel.getByText(COLLECTION_NAME).first().click();
  await page.getByText('history', { exact: true }).click();
  await expect(page.getByTestId('indexing-report-summary')).toBeVisible({ timeout: 15_000 });

  const skipped = page.getByTestId('indexing-report-category-skipped');
  // The reason is prose, not the wire's `reason: "empty"` code.
  await expect(skipped).toContainText('Contained no indexable content');
  await expect(skipped).not.toContainText('"empty"');
  await expect(skipped).toContainText('empty-one.pdf');
});

test('ELITEA-2809: unsupported items list the actual file formats (DUP: ELITEA-2830)', async ({ page }) => {
  const toolkit = await createIndexCapableToolkit(page);
  await mockIndexMetaList(page, toolkit.id);
  const panel = await openIndexesTab(page, toolkit.id);

  await panel.getByText(COLLECTION_NAME).first().click();
  await page.getByText('history', { exact: true }).click();
  await expect(page.getByTestId('indexing-report-summary')).toBeVisible({ timeout: 15_000 });

  const notIndexed = page.getByTestId('indexing-report-category-not_indexed');
  await expect(notIndexed).toContainText('Unsupported format');
  await expect(notIndexed).toContainText('.ai');
  await expect(notIndexed).toContainText('.raw');
});
