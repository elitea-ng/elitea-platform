/**
 * Journey 18: Create MCP → OAuth callback round trip (JRNY-018)
 *
 * Spec §8.5 acceptance (from parity/manifest/mcps.json JRNY-018):
 * the OAuth result reaches the app and the MCP becomes usable; OAuth errors
 * are shown on the callback screen.
 *
 * ── Rewrite notes (escape-hatch removal) ────────────────────────────────
 * The previous version of this file could not fail. It navigated to
 * `/app/mcp/my` (NOT a route — the real list is `/app/mcps/$tab`), then
 * asserted `getByText(/error|failed|invalid|callback|auth/i)`, which the
 * SUCCESS copy "Authorization successful! Closing window..." satisfies just
 * as happily as a stub page containing the word "auth"; then took one of two
 * `.catch(() => false)` early returns before any create assertion ran.
 *
 * Every assertion below now targets something a stub cannot produce:
 *  - the three mutually exclusive callback states by their EXACT copy, with
 *    the other two asserted absent;
 *  - the relay payload the page writes to `localStorage` under
 *    `el.mcp-auth-result-<state>` — the actual product behaviour of this
 *    page (it relays, it does not exchange tokens: see
 *    `src/pages/mcps/McpAuthCallbackPage.tsx:1-35`);
 *  - the MCP list row count reconciled against the live
 *    `GET /elitea_core/tools/prompt_lib/{projectId}` response;
 *  - the MCP type catalogue reconciled against the live
 *    `GET /elitea_core/toolkits/prompt_lib/{projectId}` response.
 *
 * ── Known app defects this file now surfaces instead of hiding ──────────
 * D1 — FIXED (#129), re-measured 2026-08-09 against the standalone e2e
 *    stack. It used to read: `POST /elitea_core/tools/prompt_lib/{projectId}`
 *    ALWAYS returns 500, because `pgRepo.CreateToolkit` never INSERTed the
 *    NOT-NULL `owner_id`, so no toolkit or MCP could be created at all. That
 *    is no longer true: the POST returns 201 with the minted row, the row
 *    comes back from `GET /elitea_core/tools/prompt_lib/{projectId}`, and
 *    `GET /elitea_core/tool/prompt_lib/{projectId}/{id}` serves its detail.
 *    The seed test below now gets all the way to the Indexes tab (see D3).
 * D2 — FIXED, same measurement. It used to read: `GET /elitea_core/toolkits/
 *    prompt_lib/{projectID}` is routed to the *instance* list and answers
 *    `{"rows":[],"total":0}`, so the type→schema map the create page needs
 *    was unreachable and no MCP type could ever be offered. That endpoint now
 *    returns the real type→schema map (`application`, `artifact`, … each with
 *    their `properties`/`args_schemas`). The catalogue test below was written
 *    against the backend-DERIVED set rather than against "empty", so it kept
 *    its teeth across the fix instead of cementing the bug — which is the
 *    only reason this correction was cheap to make.
 * D3 — FIXED (#149). It used to read: `pages/toolkits/EditToolkit.tsx:
 *    313-314` renders the Indexes tab as `{tab === 1 && <Box data-testid=
 *    "edit-toolkit-indexes-tab-panel" />}` — a real, clickable tab label in
 *    front of an empty Box, while `features/toolkits/indexes/ui/
 *    IndexesContainer.tsx` sat fully ported with ZERO production importers.
 *    Mounting it (`features/toolkits/ui/IndexesTab.tsx`) turned up a SECOND
 *    defect underneath: the port had also dropped the baseline's
 *    `shouldHideIndexesTab`, so the tab was being offered on screens the
 *    baseline never offers it on — MCP detail screens first among them. Both
 *    are fixed; the corrected assertion in the seed test below is what pins
 *    the second one, and `toolkits.lifecycle.spec.ts` J17.5 pins the first.
 *
 * ── Genuinely untestable here ──────────────────────────────────────────
 * The end-to-end "MCP becomes usable after authorization" half of JRNY-018
 * still cannot be exercised — but NOT for the reason recorded here before.
 * The old reason ("no MCP toolkit can exist while D1 stands") died with D1.
 * The real one, re-grepped: the receiving end of the relay is
 * `features/mcps`' `McpAuthModal`/`useMcpAuthModal`, and neither has any
 * production call site — every hit outside `features/mcps/ui/McpAuthModal.*`
 * is a doc comment citing it as a known gap (`ToolActionsSelector.tsx:28-33`,
 * `ToolBase.tsx:123`, `TestTools.tsx:48-54`). `OAuthFormFields.tsx` is
 * reachable only from that unmounted modal. So there is no OAuth-configuring
 * form to drive. Stated here and in the run report rather than hidden behind
 * a `test.skip()`.
 */
import { expect, test } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import { readsPlatformFlags } from '../../fixtures/platformFlags';

/** Exact copy from src/shared/i18n/en.json:1081-1084 — the page's three mutually exclusive states. */
const COPY = {
  processing: 'Processing authorization...',
  success: 'Authorization successful! Closing window...',
  failed: 'Authorization failed',
  invalidResponse: 'Invalid authorization response',
} as const;

/** `shared/lib/storage.ts`'s `STORAGE_NAMESPACE` + `McpAuthCallbackPage`'s `crossTabKey`. */
function relayKey(state: string): string {
  return `el.mcp-auth-result-${state}`;
}

/**
 * Reads the relay payload the callback page wrote for `state`.
 * The page removes the key 5 s after writing it, so callers must read
 * promptly — a `null` here means the page never relayed, not that it expired.
 */
async function readRelay(page: import('@playwright/test').Page, state: string): Promise<unknown> {
  const raw = await page.evaluate((key) => window.localStorage.getItem(key), relayKey(state));
  return raw === null ? null : JSON.parse(raw);
}

/*
 * Every MCP surface this file drives is gated on the platform-wide
 * `mcp_enabled` row, which `admin.features.spec.ts` turns OFF and back on to
 * prove the platform obeys it. `useIsMcpVisible()` is false for the length of
 * that window, so `ToolkitTypeSelector` returns null and the `/mcps` route is
 * closed — and these journeys then failed on an absent search box and an empty
 * catalogue, in three separate CI runs of `E2E (webkit)` (issue #519).
 *
 * The shared half of the platform-flag lock keeps them out of that window.
 */
readsPlatformFlags(test);

test.describe('JRNY-018 — MCP OAuth callback round trip', () => {
  test('J18: callback with an authorization code renders success AND relays the code', async ({ page }) => {
    const state = 'e2e-j18-success-mcp';
    const code = 'e2e-authcode-mcp';

    await page.goto(`${BASE_URL}/app/mcp-auth-callback?code=${code}&state=${state}`, {
      waitUntil: 'domcontentloaded',
    });

    // Exact success copy — not a regex that the processing/error copy also matches.
    await expect(page.getByText(COPY.success, { exact: true })).toBeVisible({ timeout: 10_000 });
    // The other two states must be absent; asserting only the positive lets a
    // page that renders all three (or a stub echoing the query string) pass.
    await expect(page.getByText(COPY.processing, { exact: true })).toHaveCount(0);
    await expect(page.getByText(COPY.failed, { exact: true })).toHaveCount(0);

    // The page's actual product behaviour: relay the result to the opener.
    // A heading-only stub cannot produce this payload.
    expect(await readRelay(page, state)).toEqual({
      type: 'mcp-auth-result',
      state,
      success: true,
      code,
    });

    await checkA11y(page);
  });

  test('J18: callback with an OAuth error renders the failure AND relays the error', async ({ page }) => {
    const state = 'e2e-j18-error-mcp';
    const errorCode = 'access_denied';
    const description = 'The MCP server owner denied the request';

    await page.goto(
      `${BASE_URL}/app/mcp-auth-callback?error=${errorCode}&error_description=${encodeURIComponent(description)}&state=${state}`,
      { waitUntil: 'domcontentloaded' },
    );

    await expect(page.getByText(COPY.failed, { exact: true })).toBeVisible({ timeout: 10_000 });
    // The provider's own description must be surfaced verbatim — this is the
    // "OAuth errors are shown on the callback screen" half of the acceptance.
    await expect(page.getByText(description, { exact: true })).toBeVisible();
    await expect(page.getByText(COPY.success, { exact: true })).toHaveCount(0);
    await expect(page.getByText(COPY.processing, { exact: true })).toHaveCount(0);

    expect(await readRelay(page, state)).toEqual({
      type: 'mcp-auth-result',
      state,
      error: errorCode,
      error_description: description,
    });

    await checkA11y(page);
  });

  test('J18: callback with neither code nor error is rejected as an invalid response', async ({ page }) => {
    const state = 'e2e-j18-invalid-mcp';

    await page.goto(`${BASE_URL}/app/mcp-auth-callback?state=${state}`, { waitUntil: 'domcontentloaded' });

    await expect(page.getByText(COPY.failed, { exact: true })).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(COPY.invalidResponse, { exact: true })).toBeVisible();
    await expect(page.getByText(COPY.success, { exact: true })).toHaveCount(0);

    // `invalid_request` is synthesised by the page itself (line 159) — it is in
    // no query parameter, so an echo-the-URL stub cannot produce it.
    expect(await readRelay(page, state)).toEqual({
      type: 'mcp-auth-result',
      state,
      error: 'invalid_request',
      error_description: 'Missing authorization code',
    });

    await checkA11y(page);
  });

  test('J18: the MCP list renders exactly the MCP-typed toolkits the API returns', async ({ page }, testInfo) => {
    /*
     * THIS TEST OWNS A ROW, AND IT TAKES ONE SAMPLE (issue #519).
     *
     * ## Why it owns a row
     *
     * `/app/mcps/all` renders no list when the project holds no MCP.
     * `shouldRedirectToCreatePage` (pages/toolkits/Toolkits.tsx) sends the
     * browser to the create page when `scopedItemCount === 0`, so
     * `mcps-list-panel` never appears and there is nothing to compare. The E2E
     * seed puts no MCP in project 1.
     *
     * So on a clean stack this test proved nothing — its expectation was the
     * empty set — and it became a real measurement only when a DIFFERENT test
     * had a fixture in flight. Measured on the corrected tree: 2 failures in
     * 12 runs, both on the absent list panel, both because no sibling row
     * existed at that moment.
     *
     * It therefore creates its own MCP, one name per browser project, and
     * removes it again. The expectation is never empty, the list panel always
     * renders, and the comparison never depends on another journey.
     *
     * ## Why it takes one sample
     *
     *
     * The claim is an EQUALITY between two surfaces, and it used to be
     * measured with three reads spread over several seconds: the API, then
     * the page load, then the rendered cards. The MCP set of project 1 is not
     * private to this test — the sibling test below creates
     * `autotest_j18-oauth-mcp` in that project and removes it again, and
     * `fullyParallel` runs the two at once — so the set could change between
     * the reads and the test then compared two different states of the
     * platform. Measured here: 3 local runs of 3, all failing with the
     * sibling's row on the screen and not in the API answer.
     *
     * Nothing is asserted more weakly. The comparison is still exact, in both
     * directions, against the API's own answer. What changed is that the
     * expectation and the observation now come from ONE window: the API is
     * read on both sides of the page load, and a sample whose two API reads
     * disagree is discarded rather than compared. A product that renders the
     * wrong set never produces a consistent sample, so it still fails — see
     * the negative control in this unit's pull request.
     */
    const apiMcpNames = async (): Promise<readonly string[]> => {
      const resp = await page.request.get(
        `${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}?limit=100&offset=0`,
      );
      expect(resp.status(), 'GET /elitea_core/tools/prompt_lib is the MCP list feed').toBe(200);
      const body = (await resp.json()) as { rows?: readonly { name: string; type: string; meta?: { mcp?: boolean } }[] };
      // `isMcpToolkit` (src/entities/toolkit/model/selectors.ts:30-33).
      return (body.rows ?? [])
        .filter((row) => row.type === 'mcp' || row.type.startsWith('mcp_') || row.meta?.mcp === true)
        .map((row) => row.name)
        .sort();
    };

    // One name per browser project: both engines read project 1 when the suite
    // runs locally with both, and a shared name would collide.
    const ownName = `${AUTOTEST_PREFIX}j18-list-${testInfo.project.name}`;
    const createResp = await page.request.post(
      `${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`,
      { data: { name: ownName, type: 'mcp', description: 'JRNY-018 list fixture' } },
    );
    expect(
      createResp.status(),
      `the list fixture must be created; got ${createResp.status()} ${(await createResp.text()).slice(0, 300)}`,
    ).toBe(201);
    const own = (await createResp.json()) as { id: string };

    try {
      let expectedMcpNames: readonly string[] = [];
      await expect
        .poll(
          async () => {
            const before = await apiMcpNames();
            // The row this test owns must be in the API answer, or the sample
            // says nothing about a list.
            if (!before.includes(ownName)) {
              return 'the API does not answer with the row this test created';
            }
            await page.goto(`${BASE_URL}/app/mcps/all`, { waitUntil: 'domcontentloaded' });

            // The MCP branch of the list panel testid (Toolkits.tsx:365) — the
            // toolkits branch renders `toolkits-list-panel`, so this also
            // proves the page was mounted with `isMCP`.
            const panelRendered = await page
              .getByTestId('mcps-list-panel')
              .waitFor({ state: 'visible', timeout: 5_000 })
              .then(
                () => true,
                () => false,
              );
            if (!panelRendered) {
              return `the MCP list panel is not on the screen; the browser is at ${page.url()}`;
            }

            const rendered = (await page.getByTestId('toolkit-card').allInnerTexts())
              .map((textOfCard) => textOfCard.split('\n')[0])
              .sort();
            const after = await apiMcpNames();

            if (before.join(' ') !== after.join(' ')) {
              return 'the MCP set of the project changed while the page loaded';
            }
            expectedMcpNames = before;
            return rendered.join(' ') === before.join(' ')
              ? 'the screen and the API agree'
              : `the screen shows ${JSON.stringify(rendered)} and the API answers ${JSON.stringify(before)}`;
          },
          // Under the 30 s test budget, so the poll reports its own last
          // answer instead of the test dying on a timeout with nothing to read.
          { timeout: 20_000, intervals: [500, 1_000, 2_000] },
        )
        .toBe('the screen and the API agree');

      // Not a formality: it fails a sample that agreed on the empty set, which
      // is what this test used to pass on.
      expect(
        expectedMcpNames.length,
        'the comparison must have had a row to compare',
      ).toBeGreaterThan(0);
      // The card count is part of the equality above; asserted again on the
      // settled page so a reader sees the number the sample agreed on.
      await expect(page.getByTestId('toolkit-card')).toHaveCount(expectedMcpNames.length);

      await checkA11y(page);
    } finally {
      await page.request.delete(
        `${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${own.id}`,
      );
    }
  });

  test('J18: the MCP create page offers exactly the MCP types the catalogue endpoint returns', async ({ page }) => {
    // D2 is fixed (see the module comment): this endpoint now returns the real
    // type→schema map, not the instance envelope, so `mcpTypeKeys` below is
    // derived from live schemas rather than from an empty list. Because the
    // assertion was always written against the derived set — never against
    // "empty" — it survived the backend fix without editing, and now asserts
    // the real catalogue. Whichever side is empty, the two must agree.
    const resp = await page.request.get(`${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`);
    expect(resp.status(), 'GET /elitea_core/toolkits/prompt_lib feeds the MCP type selector').toBe(200);
    const schemas = (await resp.json()) as Record<string, { type?: string } | undefined>;
    // `isMcpFlavouredKey` (src/features/toolkits/lib/hooks/useGetCurrentMCPSchemas.hooks.ts:53-55).
    const mcpTypeKeys = Object.entries(schemas)
      .filter(([key, value]) => key.toLowerCase() === 'mcp' || value?.type === 'mcp' || key.toLowerCase().endsWith('mcp'))
      .map(([key]) => key);

    await page.goto(`${BASE_URL}/app/mcps/create`, { waitUntil: 'domcontentloaded' });

    // Two controls a stub page cannot fake: the MCP-specific selector title and
    // the MCP-specific search field (accessible name from the placeholder,
    // ToolkitTypeSelector.tsx:170 → CategoryFilter.tsx:67). Their presence also
    // proves `useIsMcpVisible()` resolved true — the selector returns null
    // outright when `mcp_enabled === false` (ToolkitTypeSelector.tsx:238).
    await expect(page.getByText('Choose the MCP type', { exact: true })).toBeVisible({ timeout: 15_000 });
    const search = page.getByRole('textbox', { name: 'Search MCPs' });
    await expect(search).toBeVisible();
    await expect(search).toBeEditable();

    // The catalogue must match the backend exactly: every returned type is
    // offered as a tile.
    for (const key of mcpTypeKeys) {
      await expect(page.getByRole('button', { name: key === 'mcp' ? 'Remote MCP' : key })).toBeVisible();
    }

    // CORRECTED. This used to read `toHaveCount(mcpTypeKeys.length === 0 ? 1 : 0)`
    // — "the empty state appears if and only if the backend offered nothing" —
    // which was right only while the backend offered nothing at all.
    //
    // The two halves of this page are independent, and production shows both at
    // once (`/app/mcps/create`, category chips Local and Remote): LOCAL lists
    // the MCP servers a user registered on their own machine, and its empty
    // state is a pointer at the docs that explain how to make one; REMOTE
    // offers the `mcp` type. `ToolkitTypeSelector`'s `localGroupForMCP` pins
    // the empty Local section open for exactly this reason. So the Remote MCP
    // tile arriving does NOT retire the Local guidance, and asserting it did
    // would fail the moment the catalogue started serving `mcp` — which is what
    // the projection in `internal/api/v2/toolkits/mcp_projection.go` now does.
    //
    // What still holds in both directions is the LOCAL half: the guidance is
    // shown when no local server is registered, and only then.
    const emptyState = page.getByText('Still no local MCP available. Follow creation guides in our', { exact: false });
    const localTypeKeys = mcpTypeKeys.filter((key) => key !== 'mcp');
    await expect(emptyState).toHaveCount(localTypeKeys.length === 0 ? 1 : 0);

    // The Remote half is a real section with a real tile whenever the catalogue
    // serves the generic type. Asserting the heading as well as the tile is
    // what keeps a tile that landed in the wrong category from passing.
    // `Remote` renders twice by design — the filter chip and the section
    // heading — so this counts both rather than tripping strict mode on one.
    if (mcpTypeKeys.includes('mcp')) {
      await expect(page.getByText('Remote', { exact: true })).toHaveCount(2);
      await expect(page.getByRole('button', { name: 'Remote MCP' })).toBeEnabled();
    }

    await checkA11y(page);
  });

  test('J18: an MCP created through the API is listed and opens on its own detail screen', async ({ page }) => {
    /*
     * NO LONGER EXPECTED-FAIL. This carried `test.fail()` through two
     * different causes; both are now fixed and the annotation is gone.
     *
     * Cause 1 (D1/#129): the seed 500'd because `owner_id` was never
     * INSERTed, so no MCP could be created and every assertion after the
     * seed was unreachable.
     * Cause 2 (D3/#149): the Indexes tab panel was an empty `<Box/>` with no
     * production importer for `IndexesContainer`. Measured then as
     * `14 x locator resolved to <div class="MuiBox-root css-0"
     * data-testid="edit-toolkit-indexes-tab-panel"></div>`.
     *
     * #149's fix showed cause 2 was really two defects — see the corrected
     * assertion at the end of this test. Every assertion below now runs and
     * passes on both chromium and webkit.
     */
    const name = `${AUTOTEST_PREFIX}j18-oauth-mcp`;
    const createResp = await page.request.post(
      `${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`,
      { data: { name, type: 'mcp', description: 'JRNY-018 OAuth round trip fixture' } },
    );
    expect(
      createResp.status(),
      `POST /elitea_core/tools/prompt_lib must create an MCP; got ${createResp.status()} ${(await createResp.text()).slice(0, 300)}`,
    ).toBe(201);
    const created = (await createResp.json()) as { id: string };

    try {
      await page.goto(`${BASE_URL}/app/mcps/all`, { waitUntil: 'domcontentloaded' });

      const card = page.getByTestId('toolkit-card').filter({ hasText: name });
      await expect(card).toHaveCount(1, { timeout: 15_000 });

      await card.click();

      // The URL must carry the id the BACKEND minted, not a placeholder.
      await expect(page).toHaveURL(new RegExp(`/app/mcps/all/${created.id}(\\?|$)`), { timeout: 15_000 });

      // EditToolkit titles itself from the fetched detail (`detail?.name ??
      // 'Edit MCP'`, EditToolkit.tsx:250-251), so the seeded name appearing
      // here proves GET /tool/prompt_lib/{project}/{id} round-tripped.
      await expect(page.getByText(name, { exact: true })).toBeVisible({ timeout: 15_000 });
      await expect(page.getByText('Edit MCP', { exact: true })).toHaveCount(0);
      await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeVisible();

      /*
       * CORRECTED ASSERTION (#149). Until 2026-08-09 this read:
       *
       *   await page.getByRole('tab', {name: 'Indexes'}).click();
       *   await expect(page.getByTestId('edit-toolkit-indexes-tab-panel')).toBeVisible();
       *
       * It was written against unmounted code and encoded a GUESS: that an
       * MCP detail screen offers an Indexes tab at all. The baseline says it
       * does not. `apps/elitea-ui/src/pages/Toolkits/EditToolkit.jsx:205-217`
       * — `shouldHideIndexesTab` — opens with `if (mcpId) return true`, and
       * the route that supplies `mcpId` is `/mcps/:tab/:mcpId`
       * (`apps/elitea-ui/src/routes.js:48`), i.e. exactly this screen. A tab
       * marked `display: 'none'` is not rendered at all:
       * `components/StyledTabs.jsx:241` applies it to the tab button via
       * `sx={[styles.tab, {display: tab.display}]}`.
       *
       * So the empty `<Box data-testid="edit-toolkit-indexes-tab-panel"/>`
       * was TWO defects, not one: a slice with no importer, AND a gate the
       * port had dropped. The fix restores the gate
       * (`features/toolkits/lib/helpers/indexesTabVisibility.ts`) and mounts
       * the slice (`features/toolkits/ui/IndexesTab.tsx`) — and this journey
       * is the one that proves the gate, because an MCP is precisely where
       * the tab must NOT appear.
       *
       * The replacement is STRICTLY STRONGER than what it replaces. The old
       * pair could be satisfied by any visible element carrying that testid,
       * including the empty Box it was written against — it never
       * discriminated a real panel from a placeholder. These three forbid the
       * tab, forbid the panel, and pin the tab strip to its exact contents,
       * so neither an empty placeholder nor a spurious extra tab can pass.
       * The positive case — a toolkit type that DOES offer indexing renders
       * the real container — is asserted in
       * `e2e/journeys/toolkits/toolkits.lifecycle.spec.ts` (J17.5), against
       * an `artifact` toolkit, the type measured to carry `index_data`.
       */
      await expect(page.getByRole('tab', { name: 'Indexes' })).toHaveCount(0);
      await expect(page.getByTestId('edit-toolkit-indexes-tab-panel')).toHaveCount(0);
      await expect(page.getByRole('tab')).toHaveText(['Configuration']);

      await checkA11y(page);
    } finally {
      await page.request.delete(
        `${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${created.id}`,
      );
    }
  });
});
