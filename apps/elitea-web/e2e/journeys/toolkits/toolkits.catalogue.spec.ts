/**
 * JRNY-017C — the toolkit CATALOGUE, one journey per served category.
 *
 * `toolkits.lifecycle.spec.ts` beside this file drives ONE type, `custom`, and
 * one indexing type derived at runtime. That was the right shape when the
 * server served eight types. It now serves fifty-six across eleven categories,
 * and a single-type journey says nothing about the other fifty-five: a category
 * whose tiles never render, or a type whose form cannot be saved, passes every
 * existing assertion.
 *
 * THE TABLE IS THE LIVE CATALOGUE, not a list in this file. The spec reads
 * `GET /elitea_core/toolkits/prompt_lib/{projectId}` once at start-up, groups
 * the offered types by `metadata.categories[0]` exactly as the chooser does,
 * and generates one test per category. A type added to the pinned SDK snapshot
 * therefore joins this suite with no edit here, and a category that stops being
 * served fails loudly instead of quietly not being tested.
 *
 * WHY A REPRESENTATIVE PER CATEGORY, not every type. Fifty-six full UI
 * create-and-read-back journeys against a real stack is twenty minutes of wall
 * time for one fact repeated fifty-six times — the form renderer is generic and
 * `servedCatalogue.perType.test.tsx` already drives every type's schema through
 * it in the unit suite. What only an E2E run can prove is that the chooser
 * renders a category's tiles from real server metadata and that the form the
 * tile opens saves against the real API. One representative per category proves
 * that for every category.
 *
 * THE TWO SAVE PATHS, and why both exist. A type whose `required` list names a
 * credential property (`$ref`/`configuration_types`) cannot be saved from an
 * empty project: the credential picker has nothing to pick. Those categories are
 * driven through the chooser and the form — which is the half this journey adds
 * — and then created through the API for the read-back, the same split
 * `toolkits.lifecycle.spec.ts`'s J17.5 already uses. The chosen representative
 * PREFERS a credential-free type so the full UI save path is exercised wherever
 * the catalogue allows it, and each test states which path it took.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, API_BASE, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import { readsPlatformFlags } from '../../fixtures/platformFlags';

/*
 * The shared half of the platform-flag lock, for the reason
 * toolkits.lifecycle.spec.ts states: `ToolBase` draws the MCP field only while
 * the platform-wide `mcp_enabled` row is on, and admin.features.spec.ts turns
 * it off and back on.
 */
readsPlatformFlags(test);

interface ServedTypeMetadata {
  readonly label?: string;
  readonly hidden?: boolean;
  readonly categories?: readonly string[];
  readonly application?: boolean;
}

interface ServedType {
  readonly metadata?: ServedTypeMetadata;
  readonly required?: readonly string[];
  readonly properties?: Readonly<Record<string, Record<string, unknown> | undefined>>;
}

interface Representative {
  readonly category: string;
  readonly type: string;
  readonly label: string;
  /** Required settings this journey can fill from the form. */
  readonly fillableRequired: readonly string[];
  /** Required settings only a saved credential can satisfy. */
  readonly credentialRequired: readonly string[];
}

/**
 * A name unique to THIS file and THIS run, and short enough to survive the form.
 *
 * The toolkit name input carries `maxLength={MAX_NAME_LENGTH}` — 32 characters
 * (`shared/lib/limits.ts`). A longer name is TRUNCATED as it is typed, the save
 * stores the truncation, and the read-back then looks for a name that exists
 * nowhere: measured on the real stack as "did not appear in the list" for a
 * card that was plainly on screen under a name 4 characters shorter.
 *
 * So: the prefix, at most eight characters of the type, and six digits of the
 * clock — 24 characters at the longest type name, unique per type and per run.
 * `assertNameSurvivedTheField` below fails loudly at the FILL if the limit ever
 * moves, rather than twenty seconds later at the list.
 */
const MAX_TOOLKIT_NAME_LENGTH = 32;
const toolkitName = (suffix: string): string =>
  `${AUTOTEST_PREFIX}${suffix.slice(0, 8)}_${String(Date.now()).slice(-6)}`;

const createdIds: string[] = [];

/** The chooser's own grouping rule (features/toolkits/lib/hooks/useToolMenuItems.ts). */
function categoryOf(metadata: ServedTypeMetadata | undefined): string {
  const first = metadata?.categories?.[0];
  return first === undefined || first.length === 0 ? 'other' : first;
}

/**
 * Whether the create page draws a tile for one served type.
 *
 * This is `entities/toolkit`'s `toolkitTypeMenuEntries` filter plus the
 * `nonMcpSchemas` one the chooser hook applies before it — restated here rather
 * than imported, because `e2e/` deliberately imports no application source.
 * Getting it wrong is not theoretical: the first run of this journey waited
 * eleven minutes for a "Python Sandbox" tile that the product correctly never
 * draws, because `sandbox` carries the `internal_tool` category.
 *
 * A type EXCLUDED here is still served; it is simply not creatable from this
 * page, which is what the unit suite asserts type by type.
 */
function isOffered(type: string, metadata: ServedTypeMetadata | undefined): boolean {
  if (metadata?.hidden === true || metadata?.application === true) return false;
  if ((metadata?.categories ?? []).includes('internal_tool')) return false;
  const key = type.toLowerCase();
  const label = (metadata?.label ?? '').toLowerCase();
  if (['agent', 'application'].includes(key) || ['agent', 'application'].includes(label)) return false;
  // The MCP types have their own tab; the toolkit chooser filters them out.
  return key !== 'mcp' && !key.endsWith('mcp');
}

function isCredentialProperty(property: Record<string, unknown> | undefined): boolean {
  if (property === undefined) return false;
  return Object.hasOwn(property, '$ref') || Object.hasOwn(property, 'configuration_types');
}

/** A required property this journey can type a value into. */
function isFillable(property: Record<string, unknown> | undefined): boolean {
  if (property === undefined || isCredentialProperty(property)) return false;
  if (property['ui_component'] !== undefined) return false;
  const branches = Array.isArray(property['anyOf']) ? (property['anyOf'] as Record<string, unknown>[]) : [];
  const types = [property['type'], ...branches.map((branch) => branch['type'])];
  return types.includes('string');
}

/**
 * One representative per category, chosen deterministically.
 *
 * Deterministic on purpose: a random pick makes a failure unreproducible, and a
 * pick that changes between runs turns a real regression in one type into an
 * intermittent failure nobody can attribute. Credential-free first, then
 * alphabetical.
 */
function chooseRepresentatives(catalogue: Record<string, ServedType>): readonly Representative[] {
  const byCategory = new Map<string, Representative[]>();
  for (const type of Object.keys(catalogue).sort()) {
    const served = catalogue[type] as ServedType;
    const metadata = served.metadata;
    if (!isOffered(type, metadata)) continue;
    // A tile's label falls back to the humanised key when the server sends
    // none, so a type with no label still has a tile — but this journey clicks
    // BY LABEL, so it only represents a category with a type that has one.
    const label = metadata?.label;
    if (label === undefined || label.length === 0) continue;
    const required = served.required ?? [];
    const properties = served.properties ?? {};
    const candidate: Representative = {
      category: categoryOf(metadata),
      type,
      label,
      fillableRequired: required.filter((key) => isFillable(properties[key])),
      credentialRequired: required.filter((key) => isCredentialProperty(properties[key])),
    };
    const bucket = byCategory.get(candidate.category) ?? [];
    bucket.push(candidate);
    byCategory.set(candidate.category, bucket);
  }

  const chosen: Representative[] = [];
  for (const category of [...byCategory.keys()].sort()) {
    const bucket = byCategory.get(category) as Representative[];
    const credentialFree = bucket.filter((entry) => entry.credentialRequired.length === 0);
    chosen.push((credentialFree[0] ?? bucket[0]) as Representative);
  }
  return chosen;
}

async function readCatalogue(request: APIRequestContext): Promise<Record<string, ServedType>> {
  const response = await request.get(
    `${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`,
  );
  expect(response.status(), await response.text()).toBe(200);
  return (await response.json()) as Record<string, ServedType>;
}

function typeSearchBox(page: Page) {
  return page.getByPlaceholder('Search toolkits');
}

/**
 * The chooser tile for one type, found by the label the SCREEN shows.
 *
 * Case-INSENSITIVE, and that is the whole point of this helper. The client
 * keeps its own `ToolTypes` label map and it wins over the server's
 * `metadata.label`; for every type the chooser offers, the two differ only in
 * case — the server says `ADO repos`, `Sharepoint`, `Testrail`, `XRAY cloud`,
 * and the screen says `ADO Repos`, `SharePoint`, `TestRail`, `XRAY Cloud`. A
 * journey that matched the server's string exactly waited out its whole
 * timeout on five perfectly correct tiles (measured, twice).
 *
 * Copying that map here would be a second source of truth for a label; the
 * unit suite pins the mapping itself, against the client's own function. What
 * this journey is about is that a tile EXISTS for the type and opens its form.
 *
 * Anchored, so `Jira` does not also match a future `Jira Service Desk`.
 */
function tileFor(page: Page, label: string) {
  const escaped = label.replaceAll(/[.*+?^${}()|[\]\\]/g, String.raw`\$&`);
  return page.getByRole('button', { name: new RegExp(`^${escaped}$`, 'i') });
}

test.afterAll(async ({ browser }) => {
  if (createdIds.length === 0) return;
  const context = await browser.newContext();
  for (const id of createdIds) {
    await context.request.delete(
      `${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`,
    );
  }
  await context.close();
});

test.describe('JRNY-017C: the served toolkit catalogue', () => {
  test('J17C.0: every served category is offered as a heading with real tiles', async ({ page }) => {
    const catalogue = await readCatalogue(page.request);
    const representatives = chooseRepresentatives(catalogue);

    // A stack serving the eight hand-written types would group into one or two
    // categories. Asserting the FLOOR rather than an exact number keeps this
    // from breaking every time the SDK adds a toolkit.
    expect(
      representatives.length,
      `the catalogue served ${Object.keys(catalogue).length} types in`
        + ` ${representatives.length} categories: ${representatives.map((r) => r.category).join(', ')}`,
    ).toBeGreaterThanOrEqual(8);

    await page.goto(`${BASE_URL}/app/toolkits/create`);
    await expect(typeSearchBox(page)).toBeVisible({ timeout: 15_000 });

    for (const representative of representatives) {
      // The tile, by its server-supplied label. A catalogue that lost its
      // metadata renders a humanised key instead and fails here.
      await expect(
        tileFor(page, representative.label),
        `no tile for ${representative.type} (${representative.label}) in ${representative.category}`,
      ).toHaveCount(1);
    }

    await checkA11y(page);
  });

  /*
   * ONE test that loops the categories, not one test per category.
   *
   * Playwright fixes the test list before the run starts, so a test-per-category
   * would need the catalogue at module load — a network read at import time,
   * with no browser context and no way to report a failure as a test. The loop
   * keeps the table data-driven and pays for it with a longer single test; each
   * iteration names the category and the type in every assertion message, so a
   * failure still says which category broke.
   */
  test('J17C.1: one toolkit per category, created through the chooser and read back', async ({
    page,
  }) => {
    // Eleven categories, each a create page + a form + a save + a list reload.
    // The default 30 s is a per-TEST budget, not per step.
    test.setTimeout(11 * 60_000);

    const catalogue = await readCatalogue(page.request);
    const representatives = chooseRepresentatives(catalogue);
    const uiSaved: string[] = [];
    const apiSaved: string[] = [];

    for (const representative of representatives) {
      const name = toolkitName(representative.type);

      // ── the chooser
      await page.goto(`${BASE_URL}/app/toolkits/create`);
      await expect(typeSearchBox(page)).toBeVisible({ timeout: 15_000 });
      await typeSearchBox(page).fill(representative.label);
      await tileFor(page, representative.label).click();

      // ── the form really opened, and it is THIS type's form: the name field
      // and the save affordance both belong to the create form, not the chooser.
      const nameField = page.getByRole('textbox', { name: 'Toolkit Name' });
      await expect(nameField).toBeVisible({ timeout: 20_000 });
      await expect(page.getByRole('button', { name: 'Save', exact: true })).toBeVisible();
      await nameField.fill(name);
      // The field did not truncate what this journey typed. Asserted HERE so a
      // changed length limit reports as "the form shortened the name" rather
      // than as a missing card further down.
      expect(name.length).toBeLessThanOrEqual(MAX_TOOLKIT_NAME_LENGTH);
      await expect(nameField).toHaveValue(name);

      // ── the fields the type's own schema declares required
      for (const key of representative.fillableRequired) {
        // The form labels a field with its schema `title`, falling back to a
        // humanised key. Both are tried rather than reimplementing the
        // humanisation here: this journey is about the save, not the label.
        const title = catalogue[representative.type]?.properties?.[key]?.['title'];
        const label = typeof title === 'string' && title.length > 0 ? title : key;
        const field = page.getByRole('textbox', { name: label, exact: false }).first();
        if ((await field.count()) === 0) continue;
        await field.fill(`autotest-${key}`);
      }

      if (representative.credentialRequired.length > 0) {
        // Credential-gated: the save cannot complete from an empty project, so
        // the row is created over the API and the read-back below still runs
        // through the UI. Which types land here is data, not a decision in this
        // file — a category that gains a credential-free type moves to the UI
        // path on the next run.
        const created = await page.request.post(
          `${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`,
          { data: { name, type: representative.type, settings: { selected_tools: [] } } },
        );
        expect(created.status(), await created.text()).toBe(201);
        createdIds.push(((await created.json()) as { id: string }).id);
        apiSaved.push(representative.type);
      } else {
        const [response] = await Promise.all([
          page.waitForResponse(
            (r) =>
              r.request().method() === 'POST' && /\/elitea_core\/tools\/prompt_lib\//.test(r.url()),
            { timeout: 20_000 },
          ),
          page.getByRole('button', { name: 'Save', exact: true }).click(),
        ]);
        expect(
          response.status(),
          `saving a ${representative.type} answered ${response.status()}: ${await response.text()}`,
        ).toBe(201);
        createdIds.push(((await response.json()) as { id: string }).id);
        uiSaved.push(representative.type);
        await expect(page.getByRole('alert')).toHaveCount(0);
      }

      // ── read it back through a FULL reload, so the list cache cannot answer
      await page.goto(`${BASE_URL}/app/toolkits/all`);
      const card = page
        .getByTestId('toolkits-list-panel')
        .getByTestId('toolkit-card')
        .filter({ hasText: name });
      await expect(
        card,
        `the ${representative.category} representative ${representative.type} did not appear in the list`,
      ).toBeVisible({ timeout: 20_000 });
      await card.click();
      await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 20_000 });
      await expect(page.getByRole('textbox', { name: 'Toolkit Name' })).toHaveValue(name, {
        timeout: 20_000,
      });
    }

    /*
     * EVERY category was exercised, and enough of them to be a catalogue.
     *
     * A loop is a shape that can pass by running zero times, and this one runs
     * over data read at run time — so the count is asserted rather than
     * assumed. Without this the whole test would go green against a catalogue
     * that served nothing, in about a second, and read as a pass.
     */
    expect(uiSaved.length + apiSaved.length).toBe(representatives.length);
    expect(
      representatives.length,
      `only ${representatives.length} categories were exercised: `
        + representatives.map((entry) => entry.category).join(', '),
    ).toBeGreaterThanOrEqual(8);

    // At least one category must have gone the whole way through the UI. If
    // every representative were credential-gated this journey would prove only
    // that the API works, which the API tests already do — and it would report
    // that as a pass.
    expect(
      uiSaved.length,
      `no category could be saved from the create form; API-saved: ${apiSaved.join(', ')}`,
    ).toBeGreaterThan(0);
  });

  test('J17C.2: an index-capable type opens the real Indexes panel', async ({ page }) => {
    // Budget, not behaviour: a catalogue read, a create POST, a page load and
    // then four waits of 20 s each (the test pane, the Indexes tab, the two
    // panel controls) plus the 20 s `waitForResponse` and `checkA11y` — the
    // waits this test already declares sum to well over the 30 s file
    // default, so it passed only while the stack was fast. It timed out on
    // webkit in the first CI read of the ported suite with no failing
    // assertion of its own, which is the shape an exhausted budget takes.
    test.setTimeout(120_000);

    // The type is DERIVED from the live catalogue, never hardcoded: if the
    // served schemas stop offering index_data the assertion fails loudly
    // instead of silently testing a type that no longer indexes.
    const catalogue = await readCatalogue(page.request);
    const indexingType = Object.keys(catalogue)
      .sort()
      .find((type) => {
        const selectedTools = catalogue[type]?.properties?.['selected_tools'] as
          | { args_schemas?: Record<string, unknown> }
          | undefined;
        return selectedTools?.args_schemas?.['index_data'] !== undefined;
      });
    expect(
      indexingType,
      `no served type offers index_data; served: ${Object.keys(catalogue).join(', ')}`,
    ).toBeTruthy();

    const name = toolkitName('index');
    const created = await page.request.post(
      `${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`,
      {
        data: {
          name,
          type: indexingType,
          description: 'JRNY-017C indexes-tab fixture',
          settings: { selected_tools: ['index_data'] },
        },
      },
    );
    expect(created.status(), await created.text()).toBe(201);
    const id = ((await created.json()) as { id: string }).id;
    createdIds.push(id);

    const indexListRequest = page.waitForResponse(
      (r) => /\/elitea_core\/index_meta\/prompt_lib\//.test(r.url()),
      { timeout: 20_000 },
    );

    await page.goto(`${BASE_URL}/app/toolkits/all/${id}`, { waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 20_000 });

    const indexesTab = page.getByRole('tab', { name: 'Indexes' });
    await expect(indexesTab).toBeVisible({ timeout: 20_000 });
    await indexesTab.click();

    const panel = page.getByTestId('edit-toolkit-indexes-tab-panel');
    await expect(panel).toBeVisible();
    // The three assertions that tell the REAL container from the empty Box the
    // placeholder used to be — the same three J17.5 uses, for the same reason.
    await expect(panel.getByRole('button', { name: 'Add index' })).toBeVisible({ timeout: 20_000 });
    await expect(panel.getByText('Still no indexes created')).toBeVisible();
    const indexList = await indexListRequest;
    expect(indexList.status(), await indexList.text()).toBe(200);
    expect(indexList.url()).toContain(`/${id}`);

    await checkA11y(page);
  });
});
