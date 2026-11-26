/**
 * The ADMIN surface behind chat's model connections: publishing a platform
 * provider credential, watching it stop resolving, and repairing it with
 * **Re-validate** rather than by re-saving the row.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY A `chat.` FILE FOR AN ADMIN SCREEN
 * ─────────────────────────────────────────────────────────────────────────────
 * The file name is a ROUTING fact, not a claim about the subject. The
 * `chat-stream` project matches `streaming/chat.*.spec.ts`
 * (`playwright.config.ts`), and this journey needs the stack that project
 * brings up. It is not an arbitrary neighbour either: every model a chat turn
 * can address resolves through one of these rows, and `status_ok` is the column
 * that decides whether the gateway will use it at all. A credential that stops
 * resolving here is a chat that answers "model not found" three services away.
 *
 * It cannot run in `journeys/**`. That project drives
 * `docker-compose.e2e-standalone.yml`, where the revalidate route does not
 * compose: re-running ADMISSION needs the Configurations runtime
 * (`ELITEA_CONFIGURATIONS_ENABLED`) — it owns the reference expander and the
 * project vault the decision reads — together with production auth, and that
 * stack has one without the other. The full standalone stack this project uses
 * has both, so the route answers with a decision instead of a 503.
 *
 * IDENTITY: the ADMIN persona, not this project's default chat driver. The
 * whole `/admin/gateway` group takes the central `configuration.governance`
 * permission (`services/elitea-main/internal/api/router.go`), which the seed
 * grants to `e2e-admin@autotest.local` alone. `auth.setup.ts` authenticates all
 * three personas on every run, so the state is already on disk — this file only
 * has to name which one.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT IT ASSERTS, AND WHY EACH ONE IS SHAPED THIS WAY
 * ─────────────────────────────────────────────────────────────────────────────
 *
 * 1. **A freshly created provider does not break the listing.** This is the F1
 *    regression, measured through this exact panel: `POST /providers` answered
 *    201 and every subsequent `GET /admin/gateway/providers` answered 500
 *    `{"error":"list failed"}` —
 *
 *        can't scan into dest[9] (col: updated_at): cannot scan NULL into *time.Time
 *
 *    `updated_at` is nullable and Create writes only `created_at`, so a
 *    brand-new row HAS a NULL there; the listing scanned it into a `time.Time`
 *    and pgx refused the row. A listing raises that per ROW, so one new
 *    credential took the whole screen down for every operator — publishing a
 *    provider bricked the surface for publishing providers. The assertion is
 *    the row RENDERING after its own create, plus `updated_at: ''` on the wire,
 *    which is the never-updated state the crash needed.
 *
 * 2. **A dead credential reference is reported as `status_ok: false`.** Nothing
 *    else on this platform displays that column, and the gateway admits
 *    `status_ok = true` and nothing else.
 *
 * 3. **Repairing the WORLD does not repair the ROW — Re-validate does.** The
 *    reference is fixed by publishing the credential it names, through the
 *    panel. The broken row is not touched, and is asserted to be STILL false
 *    before the button is pressed. Without that middle assertion this journey
 *    would pass against a build where revalidate did nothing at all: the row
 *    would already have been true by the time it was read.
 *
 * 4. **The live check renders the answer the server gave.** `check` dials the
 *    provider and writes nothing, so its verdict is whatever the network says
 *    at that instant — it is asserted against the CAPTURED response body rather
 *    than against a fixed word, which would make this journey a test of whether
 *    a mock happens to be reachable.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE ONE STEP THAT IS NOT DRIVEN THROUGH THE UI, AND WHY
 * ─────────────────────────────────────────────────────────────────────────────
 * Breaking the reference goes through the admin API. The provider FORM cannot
 * express it: `providerDataFor` (`src/pages/admin/llmProviderForm.ts`) emits a
 * flat `Record<string, string>` of the type's declared fields, and a credential
 * reference is a NESTED object — `{"ai_credentials": {"elitea_title": "…"}}` —
 * which the expander walks and fails to resolve. There is no control on this
 * screen that can author one, so authoring it by hand is the honest way to
 * reach the state; everything the panel CAN do (create, revalidate, check,
 * delete) is done by clicking. The state itself is not exotic: an operator
 * reaches it by renaming or deleting a credential another row names.
 */
import { randomBytes } from 'node:crypto';

import { expect, test, type APIRequestContext, type Locator, type Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../playwright.config';
import { AUTOTEST_PREFIX } from '../fixtures/api';

test.use({ storageState: STORAGE_STATE.admin });

/** `GET|POST /admin/gateway/providers`, the surface under test. */
const PROVIDERS_URL = BASE_URL + '/api/v2/admin/gateway/providers';

/**
 * One platform provider as the listing reports it. Only the fields this
 * journey reads — the route publishes more.
 */
interface ProviderRow {
  readonly id: number;
  readonly elitea_title: string;
  readonly status_ok: boolean;
  readonly created_at: string;
  readonly updated_at: string;
}

/**
 * A run-unique suffix, because THE STACK IS SHARED. Two agents drive this
 * deployment at once, and a fixed title would collide on the configuration
 * table's `elitea_title` UNIQUE constraint — which surfaces as a refused create
 * and reads as "the form is broken".
 */
/*
 * `randomBytes`, not `Math.random()`. The suffix is only for uniqueness across
 * concurrent runs, but it is interpolated into the fake `sk-…` api_key this
 * journey writes, so CodeQL reads it as a secret built from a
 * cryptographically insecure source (`js/insecure-randomness`, high). The
 * value is not a real credential — it names a provider that is deleted at the
 * end of the run — but a test is a poor place to teach the pattern, and a
 * secure source costs nothing here.
 */
const RUN = `${Date.now().toString(36)}${randomBytes(3).toString('hex')}`;
const PROVIDER_NAME = `${AUTOTEST_PREFIX}platform_provider_${RUN}`;
/** The credential the broken reference names. It does not exist until step 3. */
const TARGET_NAME = `${AUTOTEST_PREFIX}platform_target_${RUN}`;

/**
 * One request, retried ONCE on a 429.
 *
 * Not a general retry: the shared stack admits a bounded number of concurrent
 * streams per principal and answers 429 when another run is mid-turn, and that
 * is interference rather than a verdict about this route. Every other status is
 * returned as-is, so a real refusal still fails the assertion that reads it.
 */
async function requestOnce(
  request: APIRequestContext,
  method: 'get' | 'post' | 'put' | 'delete',
  url: string,
  data?: unknown,
): Promise<{ status: number; body: string }> {
  for (let attempt = 0; attempt < 2; attempt += 1) {
    const response = await request.fetch(url, {
      method: method.toUpperCase(),
      ...(data === undefined ? {} : { data, headers: { 'Content-Type': 'application/json' } }),
    });
    const status = response.status();
    if (status !== 429 || attempt === 1) {
      return { status, body: await response.text() };
    }
    await new Promise((resolve) => setTimeout(resolve, 2_000));
  }
  throw new Error('unreachable');
}

/** The provider listing, as the panel reads it. Fails loudly on a non-200. */
async function readProviders(request: APIRequestContext): Promise<readonly ProviderRow[]> {
  const { status, body } = await requestOnce(request, 'get', PROVIDERS_URL);
  expect(
    status,
    `GET /admin/gateway/providers must answer 200; a 500 here is the F1 scan failure ` +
      `(one row with a NULL updated_at refuses the whole listing). Body: ${body.slice(0, 300)}`,
  ).toBe(200);
  const parsed = JSON.parse(body) as { items?: readonly ProviderRow[] };
  return parsed.items ?? [];
}

/** One row by title, or `undefined`. */
async function readProvider(
  request: APIRequestContext,
  title: string,
): Promise<ProviderRow | undefined> {
  return (await readProviders(request)).find((row) => row.elitea_title === title);
}

/** `readProvider`, asserted present — the listing is the only place an id comes from. */
async function requireProvider(request: APIRequestContext, title: string): Promise<ProviderRow> {
  const row = await readProvider(request, title);
  if (row === undefined) {
    throw new Error(`the platform provider ${title} is not in the listing`);
  }
  return row;
}

/**
 * Opens Admin › Models and spend › Providers and models.
 *
 * Waits for a POSITIVE terminal state — the table or the empty notice — rather
 * than for the absence of an error, which is also true while the request is
 * still in flight.
 */
async function openProvidersPanel(page: Page): Promise<void> {
  // The Providers & models tab of the gateway editor is a page of Models and
  // spend since ADR-0059 R3.5a (issue 2535).
  const response = await page.goto(BASE_URL + '/admin/app/models/providers', {
    waitUntil: 'domcontentloaded',
  });
  expect(
    response?.status(),
    'the admin SPA must serve the providers route, not 404',
  ).toBeLessThan(400);
  await expect(page.getByTestId('admin-llm-providers-page')).toBeVisible({ timeout: 30_000 });

  await expect(
    page.getByTestId('llm-providers-table').or(page.getByTestId('llm-providers-empty')),
  ).toBeVisible({ timeout: 20_000 });
  // The panel's own load failure is the state that would make every assertion
  // below fail for the wrong reason, so it is ruled out once, here.
  await expect(page.getByTestId('llm-providers-load-error')).toHaveCount(0);
}

/** Authors one credential through the panel's add flow and waits for its 201. */
async function createProviderThroughPanel(page: Page, title: string): Promise<void> {
  await page.getByTestId('llm-providers-add').click();
  await page.getByTestId('llm-provider-name').fill(title);
  // The type select is left alone deliberately: it opens on the first type the
  // SERVER admits, and `open_ai` is that type on every deployment whose
  // catalogue this panel reads. Driving the select would assert MUI, not this.
  await page.getByTestId('llm-provider-api_key').fill(`sk-${RUN}-not-a-real-key`);
  // The mock the rest of this stack streams from. It never has to answer for
  // the row to be ADMITTED — admission expands references and redeems secrets,
  // and contacts no provider — but it makes the live check in step 5 a real
  // round trip to a real host rather than to an invented one.
  await page.getByTestId('llm-provider-api_base').fill('http://llm-mock:8090/v1');

  const created = page.waitForResponse(
    (response) =>
      response.url().endsWith('/admin/gateway/providers') && response.request().method() === 'POST',
  );
  await page.getByTestId('llm-provider-save').click();
  const response = await created;
  expect(response.status(), `POST /providers for ${title} was refused`).toBe(201);

  // The panel refetches on success, so the row appearing is what proves the
  // LISTING survived the create — the F1 defect made exactly this step fail.
  await expect(page.getByRole('row').filter({ hasText: title })).toBeVisible({ timeout: 20_000 });
}

/** The status chip's row, scoped by title. */
function providerRowLocator(page: Page, title: string): Locator {
  return page.getByRole('row').filter({ hasText: title });
}

/**
 * Removes every row this run authored, whatever state the test ended in.
 *
 * Idempotent: a 404 means the panel already deleted it, which is the happy
 * path's own last step. The stack is shared, so a leaked platform credential is
 * visible to every other run on this deployment.
 */
async function sweep(request: APIRequestContext): Promise<void> {
  const { status, body } = await requestOnce(request, 'get', PROVIDERS_URL);
  if (status !== 200) return;
  const items = (JSON.parse(body) as { items?: readonly ProviderRow[] }).items ?? [];
  for (const row of items) {
    if (row.elitea_title !== PROVIDER_NAME && row.elitea_title !== TARGET_NAME) continue;
    await requestOnce(request, 'delete', `${PROVIDERS_URL}/${String(row.id)}`);
  }
}

test('a platform provider is published, breaks, and is repaired by Re-validate', async ({
  page,
}) => {
  // Six page interactions and four server round trips, on a stack shared with
  // other runs.
  test.setTimeout(180_000);

  try {
    await openProvidersPanel(page);

    /* ── 1. Publish one, and READ THE LISTING BACK ────────────────────────── */
    await createProviderThroughPanel(page, PROVIDER_NAME);

    const created = await requireProvider(page.request, PROVIDER_NAME);
    // The F1 state, stated: this row has never been updated. That is what made
    // the listing refuse it, and it is asserted so a future change that starts
    // stamping `updated_at` on create cannot silently retire this journey's
    // coverage of the scan.
    expect(
      created.updated_at,
      'a freshly created provider must report NO update time — this is the NULL ' +
        'the listing used to refuse the whole page over',
    ).toBe('');
    expect(created.created_at, 'the row must report its creation time').not.toBe('');
    expect(
      created.status_ok,
      'a credential with a resolvable endpoint and a sealed key is admitted on write',
    ).toBe(true);
    await expect(providerRowLocator(page, PROVIDER_NAME)).toContainText('In use');

    /* ── 2. Break its credential reference ────────────────────────────────── */
    // Through the API, not the form: see this file's header — the form emits
    // flat strings and a reference is a nested object, so no control on this
    // screen can author one.
    // The outer `data` is the request BODY's field of that name — a partial
    // update writes only the fields it carries — and the inner map is the
    // row's provider data.
    const broken = await requestOnce(page.request, 'put', `${PROVIDERS_URL}/${String(created.id)}`, {
      data: {
        api_base: 'http://llm-mock:8090/v1',
        api_key: `sk-${RUN}-not-a-real-key`,
        ai_credentials: { elitea_title: TARGET_NAME },
      },
    });
    expect(broken.status, `PUT /providers/${created.id} failed: ${broken.body.slice(0, 300)}`).toBe(
      200,
    );

    const afterBreak = await requireProvider(page.request, PROVIDER_NAME);
    expect(
      afterBreak.status_ok,
      'a credential naming a provider this platform does not publish must not be admitted',
    ).toBe(false);

    // …and the PANEL says so. The column is the only display of this state
    // anywhere in the product, so asserting it on the wire alone would leave
    // the screen free to render every row as healthy.
    await openProvidersPanel(page);
    await expect(providerRowLocator(page, PROVIDER_NAME)).toContainText('Not resolving');

    /* ── 3. Repair the WORLD: publish the credential the reference names ──── */
    await createProviderThroughPanel(page, TARGET_NAME);

    // THE DISCRIMINATOR. The reference now resolves, and the broken row is
    // still false — nothing re-ran its admission. Without this assertion the
    // step below would pass against a build where Re-validate did nothing.
    const beforeRevalidate = await requireProvider(page.request, PROVIDER_NAME);
    expect(
      beforeRevalidate.status_ok,
      'publishing the missing credential must not silently re-admit the row that names it',
    ).toBe(false);

    /* ── 4. Re-validate, and read the decision back from the server ───────── */
    const revalidated = page.waitForResponse(
      (response) =>
        response.url().includes(`/admin/gateway/providers/${String(created.id)}/revalidate`) &&
        response.request().method() === 'POST',
    );
    await page.getByTestId(`admin-provider-revalidate-${String(created.id)}`).click();
    expect((await revalidated).status(), 'POST /{id}/revalidate must answer').toBe(200);

    const afterRevalidate = await requireProvider(page.request, PROVIDER_NAME);
    expect(
      afterRevalidate.status_ok,
      'Re-validate must PERSIST the new decision — a status only the browser believes ' +
        'is one the gateway will never read',
    ).toBe(true);
    await expect(providerRowLocator(page, PROVIDER_NAME)).toContainText('In use');

    /* ── 5. The live check renders the answer the server gave ─────────────── */
    const checked = page.waitForResponse(
      (response) =>
        response.url().includes(`/admin/gateway/providers/${String(created.id)}/check`) &&
        response.request().method() === 'POST',
    );
    await page.getByTestId(`admin-provider-check-${String(created.id)}`).click();
    const checkResponse = await checked;
    // 200 for a proven round trip, 400 for a failed one — both carry
    // `{"success":…}` and both are a real verdict this panel must render. A
    // status outside that pair is a transport failure the row cannot report.
    expect(
      [200, 400].includes(checkResponse.status()),
      `POST /{id}/check answered ${checkResponse.status()}, which is neither verdict`,
    ).toBe(true);
    const verdict = (await checkResponse.json()) as { success?: boolean; message?: string };
    expect(typeof verdict.success, 'the check response must carry a success boolean').toBe(
      'boolean',
    );

    const result = page.getByTestId(`admin-provider-check-result-${String(created.id)}`);
    await expect(result).toBeVisible({ timeout: 30_000 });
    // Derived from the CAPTURED body, never from a fixed word: `check` dials a
    // real provider, so pinning "connected" here would make this journey a
    // test of whether the mock happened to be reachable.
    if (verdict.success === true) {
      await expect(result).toContainText('Live check: connected');
    } else {
      await expect(result).toContainText(verdict.message ?? 'Live check failed');
    }

    /* ── 6. Withdraw it through the panel ─────────────────────────────────── */
    await providerRowLocator(page, PROVIDER_NAME)
      .getByRole('button', { name: 'Delete' })
      .click();
    const deleted = page.waitForResponse(
      (response) =>
        response.url().includes(`/admin/gateway/providers/${String(created.id)}`) &&
        response.request().method() === 'DELETE',
    );
    await page.getByTestId('llm-providers-confirm-delete-button').click();
    expect((await deleted).status(), 'DELETE /providers/{id} must answer 204').toBe(204);

    await expect(providerRowLocator(page, PROVIDER_NAME)).toHaveCount(0, { timeout: 20_000 });
    expect(
      await readProvider(page.request, PROVIDER_NAME),
      'the deleted credential must be gone from the listing, not merely from the table',
    ).toBeUndefined();
  } finally {
    // The stack is shared. A row left behind is visible to every other run on
    // this deployment, and its title would collide with nothing — but its
    // presence would change what the next operator sees.
    await sweep(page.request);
  }
});
