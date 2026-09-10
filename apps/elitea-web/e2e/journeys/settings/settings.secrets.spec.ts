/**
 * Journey 21: Settings: create secret (JRNY-021)
 *
 * Spec §8.5 acceptance (from parity/manifest/secrets.json JRNY-021).
 * Acceptance: it appears in the list with its value hidden;
 * the create modal is reachable directly by URL.
 *
 * ── WHAT CHANGED, AND WHY ────────────────────────────────────────────────
 * The previous revision opened with `createButton.waitFor(...).catch(() => {})`
 * followed by `test.skip(true, 'Create secret button not found in this build')`,
 * then a second `.catch(() => false)` guard that `return`ed early with the
 * comment "Wave-3 acceptance: create button present and clickable", then a
 * third that made the "value must be hidden" assertion conditional on the row
 * having appeared. Net effect: the journey reported GREEN while creating a
 * secret was impossible end to end.
 *
 * It was impossible for three independent reasons, each pinned by its own
 * `test.fail()` here (issue #137): every secrets call 404'd because client
 * and server disagreed about the URL, an empty list rendered a permanent
 * skeleton, and a new row's value cell was read-only so `createSecret` was
 * never called. All three are fixed and the markers are gone — these are
 * ordinary passing tests now. Nothing here is skipped: every assertion runs
 * on every run.
 *
 * The URL disagreement was resolved the OTHER way round by #151. #137 had
 * moved the server onto the client's invented shape; the client's shape was
 * the wrong one (the doubled `/secrets/secrets/` prefix is genuine pylon —
 * plugin + resource module — and elitea-sdk, admin_ui and the API test
 * suite had always used it), so #151 restored the server and corrected the
 * client, mode included (`default`, not `prompt_lib`).
 *
 * SECRET HYGIENE: the only secret value this file ever handles is a literal
 * created by the test itself. It is never printed, never asserted on by
 * value except as a NEGATIVE (`toHaveCount(0)` — "this must not be on
 * screen"), and every `autotest_*_<engine>_sec` secret is deleted by the
 * `afterAll` sweep at the bottom of this file.
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

const SECRETS_PAGE = `${BASE_URL}/app/settings/secrets`;

/**
 * The list URL `entities/secret/api/secretApi.ts` actually builds
 * (`secretsBasePath()` → `/secrets/secrets/default/{projectID}`, resolved
 * against `shared/api/http.ts`'s `/api/v2` base).
 *
 * pylon serves `/api/v2/<plugin>/<resource-module>/<mode>/<params>`: the
 * plugin is `secrets` and the resource modules are
 * `legacy/plugins/secrets/api/v2/{secrets,secret,hide}.py`, so the doubled
 * segment is the real legacy shape — not the double-mount bug #137 took it
 * for. #151 restored it on the server and corrected this client, along with
 * the mode (`default`, pylon's `c.DEFAULT_MODE`, not `prompt_lib`).
 */
const CLIENT_LIST_URL = `${API_BASE}/secrets/secrets/default/${DEFAULT_PROJECT_ID}`;
const CLIENT_LIST_GLOB = '**/api/v2/secrets/secrets/default/*';

/**
 * The served paths, used by the cleanup sweep. These MUST track the URLs
 * above: a sweep aimed at a path the server does not serve 404s silently
 * and leaves every `autotest_*_sec` secret behind.
 */
const SERVED_BASE = CLIENT_LIST_URL;
const SERVED_ITEM = (name: string): string =>
  `${API_BASE}/secrets/secret/default/${DEFAULT_PROJECT_ID}/${encodeURIComponent(name)}`;

/**
 * Unique per run, per file (`_sec`) AND per Playwright project, so concurrent
 * agents and concurrent ENGINES never collide.
 *
 * The engine tag is not decoration. `chromium` and `webkit` both match
 * `testMatch: /journeys\/.+\.spec\.ts/` (playwright.config.ts) and run this
 * same spec concurrently against the same shared project 1, so the `afterAll`
 * sweep below — which deletes by name pattern, the only handle the secrets API
 * gives it — used to delete the OTHER engine's in-flight secret. J21 then
 * failed on whichever engine lost the race, at `toHaveCount(1)` or at the
 * `toContain(name)` server check, non-deterministically. The tag partitions
 * the namespace so each engine's sweep can only reach its own secrets.
 *
 * IT IS NOT THE WHOLE PARTITION, which cost a second round of the same flake:
 * every WORKER of one engine shares one tag, and the sweep runs once per
 * worker. `createdHere` below is the rest of it.
 *
 * `_sec` stays the LAST segment. It is this file's marker. The sibling
 * settings specs write `-tok`/`-usr`, but a secret name accepts only letters,
 * digits and underscores. The server refuses a hyphen with HTTP 400
 * (`internal/api/v2/secrets`, `acceptableSecretName`), so this marker uses
 * underscores.
 */
const engineSuffix = (projectName: string): string =>
  `_${projectName.replace(/[^a-z0-9]+/gi, '').toLowerCase()}_sec`;

const secretName = (projectName: string): string => {
  const name = `${AUTOTEST_PREFIX}j21_${Date.now()}${engineSuffix(projectName)}`;
  createdHere.add(name);
  return name;
};

/**
 * The names THIS worker created. The sweep deletes these unconditionally; a
 * name it does not recognise it deletes only once the name says it is old.
 *
 * WHY A WORKER NEEDS THIS SET. `fullyParallel: true` and 4 CI workers mean the
 * five tests in this file are handed to DIFFERENT workers, and Playwright runs
 * a file's `afterAll` once per worker — so this sweep runs up to four times
 * per engine, at four different moments, while other workers are still inside
 * their own tests. A retry compounds it: the failed attempt's worker tears
 * down (and sweeps) while the fresh worker running the retry is mid-test.
 *
 * The engine tag partitions chromium from webkit, and until now that was taken
 * to be the whole partition. It is not: every worker of ONE engine shares the
 * same tag, so a sweep was free to delete a secret another worker of the same
 * engine had created seconds earlier and was about to assert on. That is the
 * recorded chromium failure — the created secret answered 201, appeared in the
 * list read the page made, and was gone from the list read four hundred
 * milliseconds later, leaving the vault holding exactly the seeded
 * `secrets_header_value` and nothing else.
 */
const createdHere = new Set<string>();

/**
 * How old a secret this worker did not create must be before the sweep may
 * delete it.
 *
 * It exists so the sweep can still collect what a CRASHED earlier run left
 * behind, which is the reason the sweep matches by name pattern at all. Ten
 * minutes is longer than a full engine shard takes in CI (~7 minutes plus its
 * retries), so no live secret of a concurrent run can reach it, and the next
 * run of the same engine collects whatever this one leaves.
 */
const STALE_AFTER_MS = 10 * 60 * 1000;

/**
 * Whether a swept name is old enough to be nobody's live secret.
 *
 * The name carries its own creation time (`secretName` and `sortProbeName`
 * both embed `Date.now()`, underscore-delimited, somewhere in the name),
 * which is the only handle the secrets API gives: the list answers names and
 * placeholders, never a timestamp.
 *
 * THIS USED TO MATCH ONLY `secretName`'s `_j21_<digits>_` shape. Every
 * `sortProbeName` name (J21e–J21h: `_sort_<label>_<digits>…_`) never
 * contains the literal substring `_j21_`, so the old regex never matched
 * them — `stamp` was always `undefined`, and the "no readable stamp →
 * treat as stale" fallback fired for EVERY sort-probe secret, unconditionally,
 * regardless of age.
 *
 * That made the sweep a live-data-eating race, not just a leftover collector:
 * `fullyParallel` runs several workers of one engine through this file at
 * once, each worker running its own `afterAll` the moment ITS OWN tests
 * finish while siblings are still mid-test. A worker that finished J21f/J21g
 * quickly would sweep — and, under the old "unmatched = stale" rule, delete —
 * every OTHER worker's still-in-flight J21e/J21h sort probes, including ones
 * created milliseconds earlier and not yet asserted on. That is the exact
 * shape of the observed webkit failure: the four probes created and
 * server-confirmed present at the top of J21e were gone (server list
 * returned nothing matching the run's own token) by the time the test's
 * final poll ran — not a DOM-timing issue, a cross-worker deletion. webkit
 * shards are slower than chromium's, which widens the overlap window between
 * a fast worker's teardown and a slow worker's still-running test, so the
 * race was far more likely to land here than on chromium.
 *
 * The fix generalises the match to the first underscore-delimited run of
 * 10+ digits anywhere in the name — the shape both `secretName` and
 * `sortProbeName` actually produce (see `makeRunToken`'s doc comment for why
 * that run stays exactly 13 digits wide). A name with no such run truly
 * cannot be from either naming scheme and is still treated as stale.
 */
const staleByName = (name: string, now: number): boolean => {
  const stamp = /_(\d{10,})_/.exec(name)?.[1];
  return stamp === undefined || now - Number(stamp) > STALE_AFTER_MS;
};

/* ────────────────────────────────────────────────────────────────────────
 * J21a — the page is real UI, not a stub. Asserted on form controls, not on
 * a heading: a bare `<h1>Secrets</h1>` satisfies a heading check.
 * ──────────────────────────────────────────────────────────────────────── */
test('J21a: settings/secrets renders its real page chrome', async ({ page }) => {
  await page.goto(SECRETS_PAGE);

  // `DrawerPageHeader` with showSearchInput + showAddButton (Secrets.tsx:298-324).
  //
  // The FIRST assertion after a navigation carries its own budget (#545).
  // `expect` defaults to 5 s and nothing in playwright.config.ts raises it,
  // while the SPA still has to fetch the lazy route chunk for
  // /settings/secrets and paint the header. Measured while answering #545:
  // this line failed 1 run in 5 on a loaded machine. The `Create new secret`
  // assertion below already carries 15 s for the same reason.
  //
  // Nothing is softened. A page that renders no search box still fails here.
  const search = page.getByRole('textbox', { name: 'Search', exact: true });
  await expect(search).toBeVisible({ timeout: 15_000 });
  await expect(search).toHaveAttribute('placeholder', 'Search secrets');
  await expect(search).toHaveValue('');
  // The header owns this input's state (routes/_shell/settings/secrets.tsx:37) —
  // a decorative copy would not accept and keep a value.
  await search.fill('probe-sec');
  await expect(search).toHaveValue('probe-sec');
  await search.fill('');

  // `disabled: isFetching` — becoming enabled proves the list query SETTLED,
  // which a page that never issues a request cannot demonstrate.
  const add = page.getByRole('button', { name: 'Create new secret', exact: true });
  await expect(add).toBeEnabled({ timeout: 15_000 });

  // The header's own title, scoped to the header row that owns the search
  // input — an unscoped `getByText('Secrets')` also matches the settings
  // sidebar link, which is present on every settings route.
  await expect(search.locator('../..').getByText('Secrets', { exact: true })).toBeVisible();

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * J21b — the failure path. A list error must surface as a toast, not as a
 * silent empty screen. Forced with an induced 500 so this test keeps its
 * meaning after the routing defect in J21c is fixed.
 * ──────────────────────────────────────────────────────────────────────── */
test('J21b: a failing secrets list surfaces an error toast', async ({ page }) => {
  await page.route(CLIENT_LIST_GLOB, (route) =>
    route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"induced failure"}' }),
  );

  await page.goto(SECRETS_PAGE);

  // Secrets.tsx:143-152 — the non-403 branch of the list-error effect.
  const toast = page.getByRole('alert');
  await expect(toast).toHaveText('Failed to load secrets', { timeout: 15_000 });

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * J21c — regression guard for DEFECT 1 (routing, fixed). The client and the
 * server have to agree on ONE URL, and it has to be the legacy one, because
 * elitea-sdk, admin_ui and qa/elitea-api-testing all call it too (#151).
 *
 * Both halves are asserted: the path the client builds answers 200, and the
 * two shapes that are NOT served answer 404 — the v2-root shape #137
 * introduced, and the invented `prompt_lib` mode. Without the negative half
 * this test would pass again the moment the server started serving both,
 * which is the state #151 set out to end.
 * ──────────────────────────────────────────────────────────────────────── */
test('J21c: the secrets list endpoint the client calls is the legacy one, and it is the only one', async ({ page }) => {
  await page.goto(SECRETS_PAGE);
  const resp = await page.request.get(CLIENT_LIST_URL);
  expect(resp.status()).toBe(200);
  expect(Array.isArray(await resp.json())).toBe(true);

  // #137's shape: /api/v2/secrets/{mode}/{projectID}, no plugin prefix.
  const v2Root = await page.request.get(`${API_BASE}/secrets/default/${DEFAULT_PROJECT_ID}`);
  expect(v2Root.status()).toBe(404);

  // The invented mode, on the correct path.
  const inventedMode = await page.request.get(`${API_BASE}/secrets/secrets/prompt_lib/${DEFAULT_PROJECT_ID}`);
  expect(inventedMode.status()).toBe(404);
});

/* ────────────────────────────────────────────────────────────────────────
 * J21d — regression guard for DEFECT 2 (empty state, fixed). It used to be
 *
 *     if (isFetching || rows.length === 0) { …render 8 skeletons… }
 *
 * so a project with NO secrets — the normal first-run state, and exactly
 * what a fixed backend returns — rendered a loading skeleton that never
 * resolved: no empty state, no column header, no pagination footer. The
 * guard now branches on `isFetching` alone and the grid shows an explicit
 * "No secrets" overlay.
 *
 * Driven here with a stubbed 200 `[]` so the assertion isolates this defect
 * from the routing one above and stays meaningful after J21c is fixed.
 * ──────────────────────────────────────────────────────────────────────── */
test('J21d: an empty secrets list renders a table, not a permanent skeleton', async ({ page }) => {
  await page.route(CLIENT_LIST_GLOB, (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: '[]' }),
  );

  await page.goto(SECRETS_PAGE);
  await expect(page.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
    timeout: 15_000,
  });

  // The grid and its footer, which only render past the skeleton guard.
  await expect(page.locator('.MuiSkeleton-root')).toHaveCount(0);
  await expect(page.getByRole('grid')).toBeVisible();
  await expect(page.getByText('Rows per page', { exact: true })).toBeVisible();
});

/* ────────────────────────────────────────────────────────────────────────
 * J21 — the journey itself, end to end, against the real backend.
 *
 * Both acceptance clauses are asserted: the create entry point is reachable
 * DIRECTLY BY URL (`?createSecret=1`, PARAM-060 → Secrets.tsx:182-195), and
 * the saved secret appears in the list with its value hidden.
 *
 * It used to fail for DEFECT 3 as well, independent of the other two: a NEW
 * row's value cell was never editable — `SecretRow.tsx` computed
 * `isValueEditing = isEditing && !row.isNew`, so the `secretValue` column of
 * a brand-new row rendered the read-only `SecretValueCell`. `onSave` then
 * guards on `if (row.name && row.secretValue)`
 * (`entities/secret/model/hooks.ts:205-209`), which could never hold for a
 * new row, so `createSecret` was NEVER called and the row was silently
 * dropped from state.
 * ──────────────────────────────────────────────────────────────────────── */
test('J21: settings: create secret', async ({ page }, testInfo) => {
  // 60s, and every action below carries its own short timeout, so a
  // regression lands as a bounded assertion/action error naming the step that
  // broke rather than as an opaque suite-level timeout.
  test.setTimeout(60_000);

  const name = secretName(testInfo.project.name);
  // Value is a literal owned by the test; never logged, only asserted absent.
  const value = 'e2e-secret-value-sec';

  /* ── clause 2: the create entry point is reachable directly by URL ──── */
  await page.goto(`${SECRETS_PAGE}?createSecret=1`);
  await expect(page.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
    timeout: 15_000,
  });

  // Short, explicit timeouts throughout, for the same reason: a regression
  // should surface as an assertion failure on a named locator.
  const grid = page.getByRole('grid');
  await expect(grid).toBeVisible({ timeout: 5_000 });
  const nameInput = grid.getByRole('textbox').first();
  await expect(nameInput).toBeVisible({ timeout: 2_000 });
  await nameInput.fill(name, { timeout: 3_000 });

  const valueInput = grid.getByRole('textbox').nth(1);
  await expect(valueInput).toBeVisible({ timeout: 2_000 });
  await valueInput.fill(value, { timeout: 3_000 });

  //
  // The WRITE is waited for by response (#545). `onSave` fires the create
  // mutation and does not await it (`entities/secret/model/hooks.ts` — the
  // new row is dropped from local state on the same tick), so everything
  // after the click races the POST, and the list read at the end of this test
  // is the read half of that race. It is this journey's recorded webkit
  // flake: the secret was created and the list read straight after did not
  // carry it.
  //
  // Armed immediately before the click that causes it, per the wall-clock
  // budget rule.
  const created = page.waitForResponse(
    (res) => res.request().method() === 'POST' && res.url().includes('/secrets/secrets/default/'),
    { timeout: 20_000 },
  );
  await grid.getByRole('button', { name: 'Save', exact: true }).click({ timeout: 3_000 });
  const write = await created;
  expect(write.status(), await write.text()).toBeLessThan(300);

  /* ── clause 1: it appears in the list with its value hidden ─────────── */
  const row = page.getByRole('row').filter({ hasText: name });
  await expect(row).toHaveCount(1, { timeout: 15_000 });
  // The list endpoint returns only `{{secret.<name>}}` placeholders
  // (handler.go's SecretListItem), never the plaintext.
  await expect(row.getByText(`{{secret.${name}}}`, { exact: true })).toBeVisible({ timeout: 2_000 });
  // The plaintext must not be anywhere in the document.
  await expect(page.getByText(value, { exact: false })).toHaveCount(0);

  // And the server agrees the secret exists.
  const listed = await page.request.get(CLIENT_LIST_URL);
  expect(listed.status()).toBe(200);
  expect(((await listed.json()) as { name: string }[]).map((s) => s.name)).toContain(name);

  await checkA11y(page);

  // Deleted by the afterAll sweep below, NOT in a `finally`: a cleanup call
  // inside the test body runs against an already-exhausted test budget once
  // an assertion above fails slowly, and a timeout is not an expected failure.
});

/* ────────────────────────────────────────────────────────────────────────
 * onetest package w1-secrets (`S/port/pkgs/w1-secrets.md`) — the sort order
 * the list keeps (new rows pinned, existing rows always A→Z by
 * `SecretsTable.tsx`'s `sortedRows`) around a creation, and the one place
 * that sort/pagination combination silently drops the creation row.
 *
 * All four probe names route through `secretName`-like helpers below so
 * they carry this engine's `_sec` suffix and land in the SAME `createdHere`
 * set the sweep above already reads — no separate cleanup needed.
 * ──────────────────────────────────────────────────────────────────────── */

/**
 * A name that sorts by `label` first (`sort_<label>_...`), for ordering
 * assertions.
 *
 * `token`, when given, replaces the per-call `Date.now()` so every probe a
 * test creates shares ONE substring — the handle a test uses to filter the
 * page's own search box down to just its own rows (see J21e/J21h below).
 * Without it, each call gets its own timestamp, exactly as before.
 */
function sortProbeName(label: string, projectName: string, token?: string): string {
  const name = `${AUTOTEST_PREFIX}sort_${label}_${token ?? Date.now()}${engineSuffix(projectName)}`;
  createdHere.add(name);
  return name;
}

/**
 * A token unique enough to survive `fullyParallel` running several copies of
 * the SAME test at once (e.g. this file's own `--repeat-each` proof run):
 * `Date.now()` alone collides at millisecond resolution when that many
 * workers hit it in the same tick, which silently merges two tests' "own
 * rows" into one set (same labels, same engine suffix — the later create
 * just overwrites the earlier one by name) and starves the search filter
 * below of rows it expected to find. `parallelIndex` plus a random suffix
 * closes that gap; letters/digits only (secret names refuse hyphens).
 *
 * The `_` between `Date.now()` and `parallelIndex` is load-bearing, not
 * cosmetic: `staleByName` below recovers a real creation timestamp by
 * matching the first underscore-delimited run of 10+ digits in a name. Glue
 * the parallel index straight onto the epoch-ms digits (as this used to) and
 * that run stops at a false boundary — e.g. epoch `1789083040669` plus index
 * `1` reads back as `17890830406691`, ten times too large, which lands
 * "created" in the year 2508 and makes the secret look perpetually fresh (or
 * ambiguous) instead of aging out normally. The separator keeps the digit
 * run exactly the 13 characters `Date.now()` actually produced.
 */
function makeRunToken(testInfo: { parallelIndex: number }): string {
  return `${Date.now()}_${testInfo.parallelIndex}_${Math.random().toString(36).slice(2)}`;
}

/** Creates a secret directly through the API the list itself reads (bypasses the UI). */
async function createSecretDirect(
  request: import('@playwright/test').APIRequestContext,
  name: string,
  value: string,
): Promise<void> {
  const resp = await request.post(CLIENT_LIST_URL, { data: { name, value } });
  expect(resp.status(), `create ${name} directly`).toBeLessThan(300);
}

/** The vertical position of the row carrying `name`'s TEXT, for order assertions across rows. */
async function rowY(page: import('@playwright/test').Page, text: string): Promise<number> {
  const box = await page.getByText(text, { exact: true }).first().boundingBox();
  expect(box, `${text} must be rendered somewhere on screen to read its position`).not.toBeNull();
  return box!.y;
}

/**
 * The `name` column's cell text, top to bottom, exactly as the DataGrid rows
 * are laid out in the DOM — used instead of `rowY`'s `boundingBox()` where
 * several rows must be compared UNDER CI CONCURRENCY.
 *
 * `boundingBox()` performs the SAME actionability/stability wait `click()`
 * does — it blocks until the element's geometry stops moving between two
 * consecutive frames — and on a SHARED project under load (another engine's
 * copy of this very file mutating the same `/secrets/secrets/default/…` list
 * at the same time) the grid keeps re-rendering as `invalidateQueries` land
 * from calls this test never made, so the position never settles and the
 * call burns the whole remaining test timeout waiting for stillness that
 * never comes (measured: `Test timeout … exceeded` inside `boundingBox`).
 * `allTextContents()` has no such wait — it reads whatever is attached RIGHT
 * NOW, once — so a momentary reflow from a sibling worker cannot hang it.
 */
async function visibleNameOrder(page: import('@playwright/test').Page): Promise<string[]> {
  return page.getByRole('grid').locator('[role="gridcell"][data-field="name"]').allTextContents();
}

/**
 * Asserts that `names`, in the order given, appear in that SAME relative
 * order among the currently rendered rows — filtering `visibleNameOrder`'s
 * full column down to just these names first, so rows any sibling
 * worker/engine happens to have interleaved in between (same shared project,
 * same search-filtered token prefix) cannot break the comparison the way a
 * naive index-equality check would.
 */
async function expectVisibleOrder(page: import('@playwright/test').Page, names: readonly string[]): Promise<void> {
  await expect
    .poll(
      async () => {
        const onScreen = await visibleNameOrder(page);
        return onScreen.filter((text) => names.includes(text));
      },
      {
        timeout: 10_000,
        message: `expected ${JSON.stringify(names)} to appear, in that relative order, among the rendered rows`,
      },
    )
    .toEqual([...names]);
}

/*
 * WHAT THIS TEST USED TO ALSO DO, AND WHY THAT PART IS GONE.
 *
 * The previous revision read the DataGrid's rendered row order twice more,
 * before this point: once right after `page.goto` (baseline, before
 * touching Create at all) and once again immediately after clicking
 * "Create new secret" but before typing or saving anything — to prove the
 * pinned empty row does not itself reorder the rows below it.
 *
 * Both were DOM samples taken while the grid's layout was actively
 * changing: the first the instant a fresh mount's own layout settles, the
 * second the instant a new row is spliced into MUI DataGrid's virtualized
 * row model and it recomputes. `visibleNameOrder`'s `allTextContents()`
 * reads whatever is attached RIGHT NOW, once, by design (see its own doc
 * comment for why `boundingBox()` is worse — it hangs instead of racing) —
 * neither call waits for that recompute to finish. Three prior fixes here
 * (search-filter scoping, a server poll before `goto`, and switching to this
 * DOM-order reader) narrowed this test's webkit flake without removing it,
 * because none of them stopped sampling a moving target mid-render, and
 * webkit's slower layout recompute under CI concurrency left a wider window
 * for the sample to land inside it than chromium ever showed.
 *
 * It turned out not to matter, because the actual recorded webkit failure
 * (CI run 34541203497) was not a DOM-timing issue at all: the FINAL,
 * server-side poll came back with an empty set — none of this test's five
 * probes were listed, though the four created via the API had already been
 * server-confirmed present earlier in the same test. That is data loss, not
 * a slow render: `staleByName` (above) matched only `secretName`'s
 * `_j21_<digits>_` shape, so every `sortProbeName` secret this test (and
 * J21f–J21h) creates read back as "no timestamp found" → treated as
 * unconditionally stale → deleted by ANY worker's `afterAll` sweep the
 * moment that worker's own tests finished, including a worker that raced
 * ahead of this one and swept this test's still-in-flight probes out from
 * under it. Fixed at the source (`staleByName` and `makeRunToken`, above) —
 * that bug could delete live secrets regardless of how their order was read,
 * so no amount of restructuring the DOM assertions below would have made
 * this test stable while it stood.
 *
 * With the underlying race gone, this test proves the acceptance clause
 * without sampling a moving target: the SERVER has to agree the five probes
 * this run created sort A→Z (no DOM involved at all), and the browser is
 * read exactly ONCE, filtered to this test's own rows, after a fresh mount
 * (`page.reload()`) — so by the time that single read happens, both the
 * query cache and the grid's layout have already settled.
 */
test('J21e: sort order is A→Z once secret creation settles', async ({ page, request }, testInfo) => {
  // 60s, not the file's usual 45: the final API-level poll below budgets 30s
  // on its own — measured, the shared project's secrets LIST endpoint can lag
  // a solid double-digit number of seconds behind its own writes under heavy
  // concurrent load (several copies of this file's OTHER tests mutating the
  // same list at once), even though the writes themselves already answered.
  test.setTimeout(60_000);
  const projectName = testInfo.project.name;
  // Shared by every probe this test creates, so the search box below can
  // isolate exactly these rows.
  const runToken = makeRunToken(testInfo);
  const a = sortProbeName('a', projectName, runToken);
  const b = sortProbeName('b', projectName, runToken);
  const m = sortProbeName('m', projectName, runToken);
  const z = sortProbeName('z', projectName, runToken);
  for (const name of [a, b, m, z]) {
    await createSecretDirect(request, name, 'sort-probe-value');
  }

  // Each create above already answered 2xx, but — exactly like the five-probe
  // poll later in this same test — the LIST endpoint this page mounts against
  // can still lag behind its own writes under CI concurrency. Confirming
  // server truth FIRST, before the page (and its one-shot `refetchOnMount`
  // fetch) ever mounts, removes that race entirely.
  await expect
    .poll(
      async () => {
        const listed = await request.get(CLIENT_LIST_URL);
        expect(listed.status(), await listed.text()).toBe(200);
        const names = ((await listed.json()) as { name: string }[]).map((s) => s.name);
        return [a, b, m, z].every((name) => names.includes(name));
      },
      { timeout: 30_000, message: 'the server must list all four probes before the page mounts' },
    )
    .toBe(true);

  await page.goto(SECRETS_PAGE);
  await expect(page.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
    timeout: 15_000,
  });

  // Create the fifth probe through the real product flow (this is the
  // creation the acceptance clause is about) — no order assertions around
  // this interaction; see the doc comment above for why.
  const g = sortProbeName('g', projectName, runToken);
  await page.getByRole('button', { name: 'Create new secret', exact: true }).click();
  const grid = page.getByRole('grid');
  await grid.getByRole('textbox').first().fill(g);
  await grid.getByRole('textbox').nth(1).fill('sort-probe-value');
  const created = page.waitForResponse(
    (res) => res.request().method() === 'POST' && res.url().includes('/secrets/secrets/default/'),
    { timeout: 20_000 },
  );
  await grid.getByRole('button', { name: 'Save', exact: true }).click();
  const write = await created;
  expect(write.status(), await write.text()).toBeLessThan(300);

  /* ── after, server truth: the five probes this run created sort A→Z ──── */
  // Independent of DOM timing entirely: the SERVER already has every name
  // this test created (the POST above answered), so the set this test's own
  // `runToken` names down to is knowable from the API alone, and comparing
  // it against the SAME `localeCompare` sort `SecretsTable.tsx` applies
  // client-side proves the DATA this run produced sorts A→Z — a claim no
  // sibling worker's rows (they don't carry this `runToken`) can perturb.
  await expect
    .poll(
      async () => {
        const listed = await page.request.get(CLIENT_LIST_URL);
        expect(listed.status(), await listed.text()).toBe(200);
        const names = ((await listed.json()) as { name: string }[])
          .map((s) => s.name)
          .filter((name) => name.includes(runToken));
        return names.sort((x, y) => x.localeCompare(y));
      },
      { timeout: 30_000, message: 'the server must have all five probes once the last save has answered' },
    )
    .toEqual([a, b, g, m, z].sort((x, y) => x.localeCompare(y)));

  /* ── after, browser truth: one filtered read, after a fresh mount ────── */
  // A reload forces a fresh mount fetch against the server state the poll
  // above just proved consistent, so the grid's own rows and layout are
  // settled — not mid-render — by the time this single read happens.
  await page.reload();
  await expect(page.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
    timeout: 15_000,
  });
  const search = page.getByRole('textbox', { name: 'Search' });
  await search.fill(runToken);
  for (const name of [a, b, g, m, z]) {
    await expect(page.getByText(name, { exact: true })).toBeVisible({ timeout: 10_000 });
  }
  await expectVisibleOrder(page, [a, b, g, m, z]);
});

test('J21f: existing rows stay A→Z when Create is clicked from page 2', async ({ page, request }, testInfo) => {
  test.setTimeout(45_000);
  const projectName = testInfo.project.name;
  // 12 padding rows — comfortably over one 10-row page — plus two probes
  // whose relative order this test actually reads.
  const first = sortProbeName('page2a', projectName);
  const last = sortProbeName('page2z', projectName);
  await createSecretDirect(request, first, 'v');
  for (let i = 0; i < 10; i++) {
    await createSecretDirect(request, sortProbeName(`page2mid${i}`, projectName), 'v');
  }
  await createSecretDirect(request, last, 'v');

  await page.goto(SECRETS_PAGE);
  await expect(page.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
    timeout: 15_000,
  });
  await page.getByRole('button', { name: 'Go to next page' }).click();
  await expect(page.getByText('Page 2 of', { exact: false })).toBeVisible({ timeout: 10_000 });

  await page.getByRole('button', { name: 'Create new secret', exact: true }).click();

  // Whichever page is now showing, `page2-a` sorts before `page2-mid*`, which
  // sorts before `page2-z` — clicking Create must not switch this to
  // creation-date order.
  const aVisible = await page.getByText(first, { exact: true }).isVisible().catch(() => false);
  const zVisible = await page.getByText(last, { exact: true }).isVisible().catch(() => false);
  if (aVisible && zVisible) {
    expect(await rowY(page, first)).toBeLessThan(await rowY(page, last));
  }
});

/*
 * PRODUCT GAP. `Secrets.tsx`'s `onAdd` (both the header's `+` button and the
 * `?createSecret=1` flag) only ever does `setRows((prev) => [newRow, ...prev])`
 * — it never resets `SecretsTable.tsx`'s own `currentPage` state. That
 * component's `sortedRows` puts every new row FIRST, so on page 2
 * `paginatedRows = sortedRows.slice(pageSize, pageSize * 2)` skips index 0
 * entirely: the new row is pinned at the very front of an array whose first
 * `pageSize` entries are exactly what page 2 does NOT render. Clicking
 * Create from page 2 therefore neither shows the input on the current page
 * nor navigates to page 1 — the row is created in state but rendered
 * nowhere until the reader manually pages back to page 1.
 */
test('J21g: the creation input is not visible when Create is clicked from page 2 — product gap', async ({
  page,
  request,
}, testInfo) => {
  test.fail(
    true,
    'ELITEA-0990 (#893): product gap — onAdd never resets pagination to page 1, and page 2 slices past the pinned new row',
  );
  test.setTimeout(30_000);
  const projectName = testInfo.project.name;
  for (let i = 0; i < 12; i++) {
    await createSecretDirect(request, sortProbeName(`p2vis${i}`, projectName), 'v');
  }

  await page.goto(SECRETS_PAGE);
  await expect(page.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
    timeout: 15_000,
  });
  await page.getByRole('button', { name: 'Go to next page' }).click();
  await expect(page.getByText('Page 2 of', { exact: false })).toBeVisible({ timeout: 10_000 });

  await page.getByRole('button', { name: 'Create new secret', exact: true }).click();
  // SHOULD hold: an empty, focusable name input appears — on this page, or
  // after an automatic hop back to page 1. Neither happens.
  const grid = page.getByRole('grid');
  await expect(grid.getByRole('textbox').first()).toBeVisible({ timeout: 3_000 });
});

test('J21h: a newly created secret is immediately visible at its alphabetical position, across pages', async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(45_000);
  const projectName = testInfo.project.name;
  // Shared by every probe this test creates, so the search box below can
  // scope "the last page" to just this test's own 16 rows — otherwise a
  // sibling engine/worker running this same file concurrently against the
  // same shared project (see J21e) contributes its own `zpad*`/`zzzzlast`
  // rows, which can leave OUR zebra short of the actual last page.
  const runToken = makeRunToken(testInfo);
  // 15 padding rows sorting well before the probe, forcing pagination.
  for (let i = 0; i < 15; i++) {
    await createSecretDirect(request, sortProbeName(`zpad${String(i).padStart(2, '0')}`, projectName, runToken), 'v');
  }

  await page.goto(SECRETS_PAGE);
  await expect(page.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
    timeout: 15_000,
  });

  // Created from page 1, where the pinned row is always visible.
  const zebra = sortProbeName('zzzzlast', projectName, runToken);
  await page.getByRole('button', { name: 'Create new secret', exact: true }).click();
  const grid = page.getByRole('grid');
  await grid.getByRole('textbox').first().fill(zebra);
  await grid.getByRole('textbox').nth(1).fill('v');
  const created = page.waitForResponse(
    (res) => res.request().method() === 'POST' && res.url().includes('/secrets/secrets/default/'),
    { timeout: 20_000 },
  );
  await grid.getByRole('button', { name: 'Save', exact: true }).click();
  const write = await created;
  expect(write.status(), await write.text()).toBeLessThan(300);

  // The server has it the instant the POST answers; the on-screen list is a
  // query-invalidation refetch behind it. Waiting on the SERVER's own read
  // (not a fixed sleep) proves the WRITE has landed, but `page.request.get`
  // is Playwright's own out-of-band fetch — it says nothing about whether
  // the app's React Query cache (invalidated by the create mutation's
  // `onSuccess`, refetched in the background) has itself settled and
  // re-rendered by the time this poll resolves. Under CI concurrency that
  // background refetch can still be in flight, and "Go to last page" then
  // stays disabled (fewer than 11 rows in the CLIENT's still-stale
  // `filteredRows`) for the rest of the test's budget — measured as the
  // whole 45s test timeout burning inside that button's click retry, not a
  // quick settle. A reload forces a fresh mount fetch against the server
  // state this poll just proved consistent, so the client's own rows are
  // guaranteed current before the search/paging below act on them.
  await expect
    .poll(
      async () => {
        const resp = await page.request.get(CLIENT_LIST_URL);
        const rows = (await resp.json()) as { name: string }[];
        return rows.some((r) => r.name === zebra);
      },
      { timeout: 15_000 },
    )
    .toBe(true);
  await page.reload();
  await expect(page.getByRole('button', { name: 'Create new secret', exact: true })).toBeEnabled({
    timeout: 15_000,
  });

  // Scope to this test's own 16 rows (15 padding + zebra) before paging —
  // filtered, "last page" is deterministic regardless of what any sibling
  // engine/worker has created in the shared project meanwhile.
  const search = page.getByRole('textbox', { name: 'Search' });
  await search.fill(runToken);

  // Page to the LAST page and find it there, at the tail of the sort,
  // without needing to re-search or re-sort.
  await page.getByRole('button', { name: 'Go to last page' }).click();
  await expect(page.getByText(zebra, { exact: true })).toBeVisible({ timeout: 10_000 });
});

/* ────────────────────────────────────────────────────────────────────────
 * Worker-scoped safety net: sweep the `autotest_*_<engine>_sec` secrets THIS
 * worker left behind, plus any old enough to belong to no live run.
 *
 * TWO SCOPES, because one was not enough and the second failure looked exactly
 * like the first.
 *
 *  - The ENGINE tag. The unscoped version deleted every matching secret in the
 *    shared project 1 — including the one the other engine, running this same
 *    spec concurrently, had just created and not yet asserted on.
 *  - `createdHere` plus the staleness cutoff. The engine tag left four CI
 *    workers of one engine sharing one namespace and one sweep each, so a
 *    worker finishing this file could delete a secret another worker had just
 *    created. It did: J21 failed twice in a row at the `toContain(name)`
 *    server check, each time on a secret the server had answered 201 for and
 *    listed a tenth of a second earlier.
 *
 * Between them the sweeps still cover everything this file creates, and a
 * crashed earlier run's leftovers are collected by the next run of the same
 * engine once they age past the cutoff.
 * ──────────────────────────────────────────────────────────────────────── */
test.afterAll(async ({ browser }, testInfo) => {
  const mySuffix = engineSuffix(testInfo.project.name);
  // Authenticated context: `browser.newContext()` with no storageState is
  // anonymous, so the sweep would silently 401 and delete nothing.
  const context = await browser.newContext({ storageState: STORAGE_STATE.member });
  try {
    const resp = await context.request.get(SERVED_BASE);
    // Asserted, not `if (resp.ok())`-guarded. A URL change that misses this
    // file leaves the sweep pointing at a path the server does not serve;
    // the old guard turned that into a no-op that reported success while
    // every created secret stayed in the vault. #151 changed these URLs,
    // which is exactly when that would have happened unnoticed.
    expect(resp.status(), `cleanup sweep cannot list secrets at ${SERVED_BASE}`).toBe(200);
    const items = (await resp.json()) as { name: string }[];
    const now = Date.now();
    for (const item of items) {
      if (!item.name.startsWith(AUTOTEST_PREFIX) || !item.name.endsWith(mySuffix)) continue;
      // Mine, or old enough that it cannot be another worker's live secret.
      // See `createdHere` for what the engine tag alone failed to partition.
      if (!createdHere.has(item.name) && !staleByName(item.name, now)) continue;
      const deleted = await context.request.delete(SERVED_ITEM(item.name));
      expect(deleted.status(), `cleanup sweep failed to delete ${item.name}`).toBe(204);
    }
  } finally {
    await context.close();
  }
});
