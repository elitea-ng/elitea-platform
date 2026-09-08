/**
 * The SAVED credential's "Test connection" button: it must check the stored
 * row WITHOUT the browser resending the secret.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS IS A SEPARATE JOURNEY FROM `credentials.lifecycle.spec.ts`
 * ─────────────────────────────────────────────────────────────────────────────
 * J19b next door creates an open_ai credential and never types a secret into
 * it, so nothing in this suite has ever exercised the two facts this file is
 * about:
 *
 *   1. A schema-declared password field is SEALED on the way in. The row that
 *      comes back — from the create response and from every later read — holds
 *      `{{secret.<32 hex>}}` and not the key. Until a secret is typed, that
 *      path never runs.
 *   2. Because of (1), the Test button on the EDIT screen cannot use the
 *      payload check. `POST /configurations/check_connection/{p}/{type}` tests
 *      what the CLIENT sends, and what the client has after loading a saved row
 *      is the sealed reference — testing it asks the provider to authenticate
 *      the literal string `{{secret.…}}` and reports a working credential as
 *      broken. The saved row's control therefore posts
 *      `/check_stored_connection/{p}/{id}` with NO BODY AT ALL, and the server
 *      redeems the reference through the project vault itself.
 *
 * (2) is invisible on screen: both routes render the same line in the same
 * place, so a form that quietly went back to posting the payload would look
 * identical. The discriminator is therefore the WIRE — every request the edit
 * screen makes is recorded and searched for the typed key and for the sealed
 * marker — plus a server READ-BACK of the row afterwards, which a check that
 * had (say) re-sealed or rewritten the credential would move.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS STACK CANNOT REACH, AND WHY NOTHING BELOW RESTS ON IT
 * ─────────────────────────────────────────────────────────────────────────────
 * This stack composes the stored resolver — `deploy/docker-compose.e2e-standalone.yml`
 * sets `ELITEA_CONFIGURATIONS_ENABLED` — but no gateway connection checker,
 * because that needs `LLM_GATEWAY_URL` and this stack runs no gateway. The
 * credential under test is an `open_ai` one, and the LLM branch of
 * `checkStoredRow` needs BOTH, so it still takes its "not composed" branch and
 * answers HTTP 400 `{"success":false,"message":"Connection checking is not
 * available right now."}` — an honest refusal, measured, and NOT a product
 * verdict. (A TOOLKIT credential takes a different branch on this stack now,
 * and really is probed: see `e2e/journeys/toolkits/toolkits.credential-status.spec.ts`.)
 *
 * Nothing here asserts a particular verdict because of that. What is asserted
 * is the contract that holds on ANY stack: the route is mounted and authorised
 * (never 401/403/404), it answers a `success` boolean, the form renders THE
 * SERVER'S OWN WORDS for whatever it answered, and no secret ever leaves the
 * browser. A stack that CAN dial a provider satisfies every line below
 * unchanged.
 */
import { test, expect, request as apiRequest } from '@playwright/test';
import type { Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

/** The saved-row route. Named once so the assertions and the negatives cannot drift apart. */
const STORED_CHECK_PATH = '/configurations/check_stored_connection/';
/**
 * The payload route the edit screen must NOT use.
 *
 * The two paths are disjoint strings — `check_stored_connection` does not
 * contain `check_connection` — so a substring match on this one cannot be
 * satisfied by a stored check, and the negative below means what it says.
 */
const UNSAVED_CHECK_PATH = '/configurations/check_connection/';

/** One request as this spec judges it. `body` is `''` for a request that carried none. */
interface RecordedRequest {
  readonly url: string;
  readonly method: string;
  readonly body: string;
}

/**
 * Records every request the page makes from now on.
 *
 * Attached BEFORE the edit screen is opened, so it covers the form's own
 * seeding read as well as the check: neither may carry the credential.
 */
function recordRequests(page: Page): RecordedRequest[] {
  const recorded: RecordedRequest[] = [];
  page.on('request', (request) => {
    recorded.push({ url: request.url(), method: request.method(), body: request.postData() ?? '' });
  });
  return recorded;
}

/** Every recorded call to the PAYLOAD route. On a saved row this list must be empty. */
function unsavedCheckCalls(recorded: readonly RecordedRequest[]): readonly RecordedRequest[] {
  return recorded.filter((entry) => entry.url.includes(UNSAVED_CHECK_PATH));
}

/**
 * The line the form must render, derived from the response the test captured.
 *
 * Not a fixed string: on a stack that can dial the provider the answer is a
 * success and the form says so, and on one that cannot it is the server's own
 * message. Deriving the expectation from the body is what makes this assertion
 * catch a client-invented message — the failure this control's whole purpose
 * depends on not having.
 */
function expectedResultLine(body: { success?: boolean; message?: string }): string {
  if (body.success === true) return 'Connection successful';
  expect(
    body.message,
    'a refused stored check must carry the server\'s own reason, or the form has nothing true to render',
  ).toBeTruthy();
  return String(body.message);
}

/** Ids this run created, so a run that fails mid-way still cleans up after itself. */
const createdConfigurationIds: string[] = [];

test.afterAll(async () => {
  if (createdConfigurationIds.length === 0) return;
  const api = await apiRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.member });
  try {
    for (const id of createdConfigurationIds) {
      const removed = await api.delete(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`);
      if (!removed.ok() && removed.status() !== 404) {
        // eslint-disable-next-line no-console -- a silent sweep is how a UNIQUE elitea_title collides on the next run
        console.warn(`stored-check teardown: configuration ${id} survives (${String(removed.status())})`);
      }
    }
  } finally {
    await api.dispose();
  }
});

test('J19c: testing a SAVED credential sends no secret, and leaves the sealed row untouched', async ({ page }, testInfo) => {
  // `elitea_title` is UNIQUE per project and the create path submits the typed
  // name as both `label` and `elitea_title`, so the name has to be unique per
  // run AND per browser project — chromium and webkit drive this file against
  // one database at the same time.
  const unique = `${AUTOTEST_PREFIX}cred_stored_${testInfo.project.name}_${Date.now()}`;
  // Distinctive enough that finding it anywhere in a request body is proof,
  // and not a substring of anything else this page sends.
  const typedSecret = `sk-e2e-stored-check-${testInfo.project.name}-${Date.now()}`;

  /* ── create it through the UI, WITH a secret ───────────────────────────── */
  await page.goto(BASE_URL + '/app/settings/model-configuration');
  const createButton = page.getByRole('button', { name: 'Create configuration', exact: true });
  await expect(createButton).toBeEnabled({ timeout: 30_000 });
  await createButton.click();

  // open_ai, because the type has to declare `has_test_connection` for the
  // control under test to render at all — the seeded `vllm`/`llm_model` types
  // do not, and their forms carry no Test button.
  await page.getByRole('button', { name: 'OpenAI', exact: true }).click();
  await expect(page).toHaveURL(/\/settings\/create-configuration\/open_ai/);

  // `exact`: see credentials.toolkit-types.spec.ts — a schema-declared
  // "Username" field also matches the unanchored accessible name.
  const nameInput = page.getByRole('textbox', { name: 'Name', exact: true });
  await expect(nameInput).toBeVisible({ timeout: 20_000 });
  await nameInput.fill(unique);
  await page.getByRole('textbox', { name: 'Api Base' }).fill('http://localhost/mock');
  // The field that makes this journey different from J19b: a schema-declared
  // password, which is what the sealing path keys on.
  await page.getByLabel('Api Key').fill(typedSecret);

  const save = page.getByRole('button', { name: 'Save', exact: true });
  await expect(save).toBeEnabled();
  const [createResponse] = await Promise.all([
    page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' && response.url().includes('/configurations/configurations/'),
      { timeout: 20_000 },
    ),
    save.click(),
  ]);
  expect(
    createResponse.status(),
    'a credential carrying a secret must be STORED, not refused: a 503 here means the vault sealer is not composed',
  ).toBe(201);
  const created = (await createResponse.json()) as { id?: string | number; data?: Record<string, unknown> };
  const configId = String(created.id ?? '');
  expect(configId).not.toBe('');
  createdConfigurationIds.push(configId);

  // The CREATE RESPONSE already carries the reference rather than the key —
  // the seal happens before the row is written, so there is no window in which
  // a read could return the plaintext.
  const sealedOnCreate = String((created.data ?? {})['api_key'] ?? '');
  expect(sealedOnCreate, 'the stored api_key must be a hidden-secret reference').toMatch(
    /^\{\{secret\.[0-9a-f]{32}\}\}$/,
  );
  expect(sealedOnCreate).not.toContain(typedSecret);

  await expect(page).toHaveURL(/\/settings\/model-configuration(\?|$)/, { timeout: 15_000 });

  /* ── reopen the saved row, and watch the wire ─────────────────────────── */
  const recorded = recordRequests(page);
  await page.goto(`${BASE_URL}/app/settings/edit-configuration/${configId}`);

  // Seeding is finished when the server's values are in the fields. That
  // matters for more than timing: it is what puts the SEALED reference into the
  // form's own state, which is the value a payload check would send.
  await expect(nameInput).toHaveValue(unique, { timeout: 20_000 });
  await expect(page.getByRole('textbox', { name: 'Api Base' })).toHaveValue('http://localhost/mock');

  await checkA11y(page);

  const testButton = page.getByRole('button', { name: 'Test connection' });
  await expect(testButton).toBeVisible();

  const [checkResponse] = await Promise.all([
    page.waitForResponse((response) => response.url().includes(STORED_CHECK_PATH), { timeout: 30_000 }),
    testButton.click(),
  ]);

  /* ── the route, and who may call it ───────────────────────────────────── */
  // 404 would mean the route is not mounted; 401/403 that this persona cannot
  // reach it. Neither is a verdict about the credential, and both are what an
  // unwired build answers. The verdict ITSELF is deliberately not asserted —
  // see this file's header for what this stack can and cannot resolve.
  expect(checkResponse.status(), 'the stored check must be mounted').not.toBe(404);
  expect(checkResponse.status(), 'the caller holds configurations.configuration.update').not.toBe(403);
  expect(checkResponse.status()).not.toBe(401);
  expect(checkResponse.request().method()).toBe('POST');
  expect(new URL(checkResponse.url()).pathname).toBe(
    `/api/v2/configurations/check_stored_connection/${DEFAULT_PROJECT_ID}/${configId}`,
  );
  const checkBody = (await checkResponse.json()) as { success?: boolean; message?: string };
  expect(
    typeof checkBody.success,
    'the stored check answers the same {success, message} contract the payload check does',
  ).toBe('boolean');

  /* ── the discriminating assertion: nothing secret was sent ────────────── */
  const storedCalls = recorded.filter((entry) => entry.url.includes(STORED_CHECK_PATH));
  expect(storedCalls, 'exactly one stored check, from one press').toHaveLength(1);
  expect(
    storedCalls[0]?.body,
    'the stored check carries NO body — there is deliberately no parameter through which form data could reach it',
  ).toBe('');

  // The payload route must not have been used at all. This is the assertion a
  // form that fell back to `useTestConfigurationConnection` on a saved row
  // fails, and the only one that can tell the two halves apart.
  expect(
    unsavedCheckCalls(recorded).map((entry) => entry.url),
    'a SAVED row must never be tested through the payload route',
  ).toEqual([]);

  const bodiesWithSecret = recorded.filter((entry) => entry.body.includes(typedSecret));
  expect(
    bodiesWithSecret.map((entry) => `${entry.method} ${entry.url}`),
    'the typed key must not appear in ANY request the edit screen makes',
  ).toEqual([]);

  // The sealed reference must not travel either. It is not the key, but sending
  // it is the exact defect the stored route exists to prevent: the provider is
  // asked to authenticate a template string and a working credential is
  // reported broken.
  const bodiesWithMarker = recorded.filter((entry) => entry.body.includes('{{secret.'));
  expect(
    bodiesWithMarker.map((entry) => `${entry.method} ${entry.url}`),
    'the sealed reference must not be sent as though it were the credential',
  ).toEqual([]);

  /* ── the form renders the SERVER's answer, not one of its own ─────────── */
  await expect(page.getByText(expectedResultLine(checkBody), { exact: false })).toBeVisible({
    timeout: 15_000,
  });

  /* ── server read-back: the row did not move ──────────────────────────── */
  // A check that writes is the failure `stored_check.go` is written not to have:
  // status_ok is an admission decision and a provider round trip is not one.
  const after = await page.request.get(
    `${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${configId}`,
  );
  expect(after.status(), 'the row must still be readable after a check').toBe(200);
  const afterBody = (await after.json()) as { data?: Record<string, unknown> };
  expect(
    String((afterBody.data ?? {})['api_key'] ?? ''),
    'the sealed reference is unchanged: the check redeemed it server-side and rewrote nothing',
  ).toBe(sealedOnCreate);

  /* ── remove it, so the run is repeatable against a stack that persists ── */
  await page.getByRole('button', { name: 'Credential actions' }).click();
  await page.getByRole('menuitem', { name: 'Delete' }).click();
  const confirm = page.getByRole('button', { name: 'Delete', exact: true });
  await expect(confirm).toBeDisabled();
  await page.locator('#delete-entity-modal-input-name').fill(unique);
  await expect(confirm).toBeEnabled();
  const [deleteResponse] = await Promise.all([
    page.waitForResponse(
      (response) =>
        response.request().method() === 'DELETE' && response.url().includes('/configurations/configuration/'),
      { timeout: 20_000 },
    ),
    confirm.click(),
  ]);
  expect(deleteResponse.ok()).toBe(true);
  await expect(page).toHaveURL(/\/settings\/model-configuration(\?|$)/, { timeout: 15_000 });
  await expect(page.getByText(unique)).toHaveCount(0);
});
