/**
 * A toolkit whose STORED credential the platform will not certify: what the
 * credential row says about it, and what the toolkit form lets the user do
 * next.
 *
 * Ported by use case from the legacy suite: "opening a toolkit whose stored
 * credential fails its check shows a warning naming the reason, and the form
 * says so where the user is about to save". The live-credential half of that
 * case already exists as an opt-in journey against a real provider
 * (`e2e/live/toolkits.indicators.spec.ts`, LIVE-TK-IND-1, which needs a real
 * Jira tenant and never runs in CI). This file is the half that CAN run on the
 * hermetic stack.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY NO 401 STUB, AND WHY THAT COSTS THIS JOURNEY NOTHING
 * ─────────────────────────────────────────────────────────────────────────────
 * The obvious way to make the check REFUSE would be to point
 * `ELITEA_TOOLKIT_CHECK_ALLOWLIST` at a service inside the stack that answers
 * 401. It does not work here, and the reason is upstream of the allowlist: the
 * picker checks a SAVED row, so the request goes to
 * `POST /configurations/check_stored_connections/{projectId}`, and
 * `checkStoredToolkitRow` (`services/elitea-main/internal/api/v2/
 * configurations/stored_check.go`) refuses before it dials anything unless a
 * `StoredConfigurationResolver` is composed. That resolver composes only under
 * `ELITEA_CONFIGURATIONS_ENABLED`, which `deploy/docker-compose.e2e-standalone.yml`
 * deliberately does not set (`e2e/fixtures/api.ts`'s `createGithubToolkit`
 * records the same fact for the settings-validation route). A stub would sit
 * there unvisited: the probe in `toolkit_check.go` is never reached, so no
 * allowlist entry can change the answer. Turning the whole Configurations
 * runtime on for this one journey means a vault master key and a public
 * project id on the e2e stack — a stack change with its own failure modes, far
 * outside this journey's subject.
 *
 * So the verdict is NOT hardcoded here, exactly as `credentials.stored-check.
 * spec.ts` does not hardcode its own. Everything below is derived from the
 * SERVER'S OWN ANSWER for this row, captured off the wire:
 *
 *   - the row must come back REFUSED (`success !== true`) — the credential
 *     this journey seeds points at an unroutable host with a placeholder
 *     token, so no stack may ever certify it;
 *   - it must NOT come back `unsupported` / `unsupported_type` — `github` is a
 *     type this build carries a probe for, and "we cannot check this kind of
 *     credential" is not the same statement as "this credential is refused";
 *   - the row on screen must carry THE SERVER'S OWN MESSAGE, not wording the
 *     browser invented for it.
 *
 * On the stack this runs against today that message is the honest
 * "not composed" refusal; on a stack that composes the resolver and gets a 401
 * it is "Authentication failed…". Every line below holds unchanged in both,
 * which is the point: this journey measures the SURFACE — does a credential
 * the platform will not certify reach the user, and does the form act on it —
 * not one deployment's reason string.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE ROW IS FOUND BY NAME AND NOT BY POSITION
 * ─────────────────────────────────────────────────────────────────────────────
 * The picker lists every `github` credential the project holds, and two
 * browser projects plus four workers author their own at the same time
 * (`credentials.toolkit-types.spec.ts` seeds five types per run). A
 * `getByTestId('credential-status-indicator')` taken at page scope would
 * therefore read SOMEBODY ELSE'S row about half the time. Every assertion here
 * is scoped to the option carrying this run's own unique credential title.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext, Locator, Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

/** The saved-row check the picker uses. Named once so the wait and the prose cannot drift. */
const STORED_BATCH_CHECK_PATH = '/configurations/check_stored_connections/';

/**
 * The endpoint the seeded credential names.
 *
 * `.invalid` is reserved by RFC 2606 and resolves nowhere, so this journey
 * cannot reach the internet even on a deployment whose allowlist is wide open.
 */
const UNROUTABLE_BASE_URL = 'https://autotest.invalid/api';

/**
 * The stored token.
 *
 * A placeholder, and shaped so nothing can mistake it for a real one: no
 * provider prefix, no entropy, and the word "placeholder" in the middle of it.
 */
const PLACEHOLDER_TOKEN = 'autotest-placeholder-token-not-a-credential';

/**
 * The toolkit probe's closed vocabulary for a verdict ABOUT THE CREDENTIAL
 * (`internal/api/v2/configurations/toolkit_check.go`). Every other refusal
 * shape carries no reason at all, and says only that the check could not be
 * made — see the second test below.
 */
const REFUSAL_REASONS: readonly string[] = ['auth_failed', 'unreachable'];

interface SeededToolkit {
  readonly toolkitId: string;
  /**
   * Every id the picker could address this credential by.
   *
   * `useCredentialRows` keys a row as `uid ?? id`, and the batch check echoes
   * back whatever it was sent. This build's configuration routes publish
   * `uuid` and no `uid`, so the numeric id is what travels today — but a row
   * matched on one spelling alone would go silently unfound the day the other
   * one starts being served, and an unfound row reads exactly like a check
   * that never ran.
   */
  readonly credentialIds: readonly string[];
  readonly credentialTitle: string;
}

/** One row of the batch stored-check answer (`storedConnectionCheckBody` plus its `id`). */
interface StoredCheckRow {
  readonly id?: string | number;
  readonly success?: boolean;
  readonly message?: string;
  readonly reason?: string;
  readonly unsupported?: boolean;
}

/**
 * Seeds a `github` toolkit that references a `github` credential of its own.
 *
 * Not `e2e/fixtures/api.ts`'s `createGithubToolkit`: that one stores a base URL
 * and NO token, which a stack that can really probe would answer
 * `unsupported_type` for ("this credential carries an authentication method we
 * cannot test") rather than as a refusal. This journey is about the refusal, so
 * the seeded row carries a token the probe can actually present.
 */
async function seedToolkitWithCredential(
  request: APIRequestContext,
  tag: string,
): Promise<SeededToolkit> {
  const credentialTitle = `${AUTOTEST_PREFIX}cred_status_${tag}`;
  const credential = await request.post(
    `${API_BASE}/configurations/configurations/${DEFAULT_PROJECT_ID}`,
    {
      data: {
        type: 'github',
        elitea_title: credentialTitle,
        label: credentialTitle,
        shared: false,
        data: { base_url: UNROUTABLE_BASE_URL, access_token: PLACEHOLDER_TOKEN },
      },
    },
  );
  expect(
    credential.status(),
    `seeding the credential answered ${credential.status()}: ${(await credential.text()).slice(0, 300)}`,
  ).toBe(201);
  const created = (await credential.json()) as { id?: string | number; uuid?: string };
  const credentialIds = [created.id, created.uuid]
    .map((value) => String(value ?? ''))
    .filter((value) => value !== '');
  expect(credentialIds.length, 'the seeded credential must carry an id').toBeGreaterThan(0);

  const toolkit = await request.post(
    `${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`,
    {
      data: {
        name: `${AUTOTEST_PREFIX}tk_status_${tag}`,
        type: 'github',
        settings: {
          // Both required fields, in the shape the stored row keeps — the
          // reference is an OBJECT, never the bare string that
          // `CredentialFormFields.tsx` records as a real defect class here.
          repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
          github_configuration: { elitea_title: credentialTitle, private: false },
          selected_tools: [],
        },
      },
    },
  );
  expect(
    toolkit.status(),
    `seeding the toolkit answered ${toolkit.status()}: ${(await toolkit.text()).slice(0, 300)}`,
  ).toBe(201);
  const toolkitId = String(((await toolkit.json()) as { id?: string | number }).id ?? '');
  expect(toolkitId, 'the seeded toolkit must carry an id').not.toBe('');

  return { toolkitId, credentialIds, credentialTitle };
}

/** Removes what `seedToolkitWithCredential` made, in dependency order. Never throws. */
async function removeSeed(request: APIRequestContext, seed: SeededToolkit | undefined): Promise<void> {
  if (seed === undefined) return;
  await request
    .delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${seed.toolkitId}`)
    .catch(() => {});
  await request
    .delete(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${seed.credentialIds[0] ?? ''}`)
    .catch(() => {});
}

/**
 * Opens the toolkit and returns the SERVER's verdict for its credential.
 *
 * The wait is armed BEFORE the navigation: the picker fires its batch check as
 * soon as the credential list resolves, which is well inside the page load.
 */
async function openToolkitAndReadVerdict(page: Page, seed: SeededToolkit): Promise<StoredCheckRow> {
  /*
   * The batch that carries THIS credential, not merely the first batch.
   *
   * The picker checks the selected project's rows and the caller's personal
   * ones as two separate requests (`batchValidateStoredCredentials` groups by
   * owning project), so "the first POST to this path" is not necessarily the
   * one holding the seeded row — and a run that read the wrong batch would
   * fail as "the picker never asked about this credential", which is a
   * different and wrong story.
   */
  const checked = page.waitForResponse(async (response) => {
    if (!response.url().includes(STORED_BATCH_CHECK_PATH)) return false;
    if (response.request().method() !== 'POST') return false;
    if (!response.ok()) return true;
    const body = (await response.json().catch(() => null)) as readonly StoredCheckRow[] | null;
    if (body === null) return true;
    return body.some((candidate) => seed.credentialIds.includes(String(candidate.id ?? '')));
  }, { timeout: 60_000 });
  await page.goto(`${BASE_URL}/app/toolkits/all/${seed.toolkitId}`, { waitUntil: 'domcontentloaded' });
  // The route landmark unique to the toolkit editor (`toolkits.lifecycle.spec.ts`
  // J17.3 uses the same one, and it is `toBeAttached` because the slot is an
  // intentionally empty Box on this build).
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 60_000 });

  const response = await checked;
  // ALWAYS 200: the batch route answers per-row results and never a status,
  // because the web app marks every credential invalid when the request itself
  // fails — a 4xx here would paint a healthy project red.
  expect(response.status(), 'the batch stored check answers 200 with per-row results').toBe(200);
  const rows = (await response.json()) as readonly StoredCheckRow[];
  const row = rows.find((candidate) => seed.credentialIds.includes(String(candidate.id ?? '')));
  expect(row, 'the picker must have asked the server about the credential this toolkit references').toBeDefined();
  return row as StoredCheckRow;
}

/**
 * Asserts the verdict is the one this journey is about — a REFUSAL of a
 * checkable type — and returns the message the row must render.
 */
function refusalMessageOf(row: StoredCheckRow): string {
  expect(
    row.unsupported,
    'github is a type this build carries a probe for, so the batch must not report the whole type as unknown',
  ).not.toBe(true);
  expect(
    row.reason,
    'a credential the platform cannot CHECK is not a credential the platform REFUSED, and the row must not conflate them',
  ).not.toBe('unsupported_type');
  expect(
    row.success,
    'a placeholder token pointing at an unroutable host must never come back certified',
  ).not.toBe(true);
  const message = String(row.message ?? '');
  expect(
    message,
    'a refusal with no message leaves the row with nothing true to say, which is the whole failure this surface exists to prevent',
  ).not.toBe('');
  return message;
}

/** Opens the toolkit's credential picker and returns the option row for this run's own credential. */
async function openPickerRow(page: Page, credentialTitle: string): Promise<Locator> {
  const picker = page.getByRole('combobox').first();
  await expect(
    picker,
    'the toolkit configuration form must render a credential picker — ' +
      'pages/toolkits/lib/credentialPickerSlots.tsx is what composes it',
  ).toBeVisible({ timeout: 30_000 });
  await picker.click();

  const option = page.getByRole('option').filter({ hasText: credentialTitle });
  await expect(option, 'the picker must offer the credential this toolkit references').toHaveCount(1, {
    timeout: 30_000,
  });
  return option;
}

test('a toolkit credential the platform will not certify carries the failure on its own row, in the server’s words', async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(180_000);

  // Unique per run AND per browser project: `elitea_title` is UNIQUE per
  // project and chromium and webkit drive this file against one database at
  // the same time.
  const tag = `${testInfo.project.name}_${Date.now()}`;
  let seed: SeededToolkit | undefined;
  try {
    seed = await seedToolkitWithCredential(request, tag);

    const verdict = await openToolkitAndReadVerdict(page, seed);
    const expectedMessage = refusalMessageOf(verdict);

    const option = await openPickerRow(page, seed.credentialTitle);

    // ── the attention indicator, on THIS row ────────────────────────────
    const indicator = option.getByTestId('credential-status-indicator');
    await expect(
      indicator,
      'a credential the server refused must carry the attention indicator on its own row',
    ).toBeVisible({ timeout: 30_000 });

    // The discriminating assertion. The indicator's accessible name is the
    // only place the REASON reaches the user, and `CredentialOptionLabel`
    // falls back to a generic "Credential is unavailable or misconfigured"
    // when it is handed no message — so a row that lost the server's answer
    // still looks identical on screen. Comparing against the captured body is
    // what tells the two apart.
    await expect(
      indicator,
      'the row must name the reason the SERVER gave, not a generic line the browser invented',
    ).toHaveAttribute('aria-label', expectedMessage);

    // The action beside it: the user fixes the credential and re-checks it
    // from here, without a page reload.
    await expect(option.getByTestId('credential-reload-button')).toBeVisible();
    // …and this one is visible whatever the status, which is the half a
    // "hide everything on failure" regression would break.
    await expect(option.getByTestId('credential-open-in-new-tab-button')).toBeVisible();
  } finally {
    await removeSeed(request, seed);
  }
});

/*
 * The second half of the legacy case: what the FORM does about it.
 *
 * The legacy behaviour (itself a fix for a legacy bug) is that Save is
 * refused, with the reason stated, so a toolkit that cannot authenticate
 * cannot be saved in ignorance. This journey used to record the OPPOSITE as
 * measured state — this route had no Save control at all, so no save-time
 * gate could exist. That gap is closed, and the two assertions that recorded
 * it are inverted here rather than deleted: the control must exist, and the
 * gate must act on the SERVER'S OWN verdict.
 *
 * THE GATE IS KEYED ON THE PROBE'S REASON, NOT ON "the check failed". The
 * header comment above explains why this stack answers every stored check
 * with the honest "not composed" refusal: `success: false` and NO `reason`.
 * That is the deployment saying it could not ask, and it must never cost a
 * user their edit — so on THIS stack the correct behaviour is Save ENABLED.
 * On a stack that composes the resolver and gets a 401 the same row comes
 * back `reason: "auth_failed"` and the correct behaviour is Save REFUSED with
 * the reason on screen. Both are asserted below, chosen by what the wire
 * actually said, exactly as the first test chooses its expected message.
 */
test('the toolkit form gates Save on the server’s own verdict about the credential', async ({
  page,
  request,
}, testInfo) => {
  test.setTimeout(180_000);

  const tag = `${testInfo.project.name}_gate_${Date.now()}`;
  let seed: SeededToolkit | undefined;
  try {
    seed = await seedToolkitWithCredential(request, tag);

    // Asserted first, so what follows is a statement about a REFUSED
    // credential rather than about any credential at all.
    const verdict = await openToolkitAndReadVerdict(page, seed);
    refusalMessageOf(verdict);

    // The form is fully editable: the refusal freezes no field.
    const nameField = page.getByRole('textbox', { name: 'Toolkit Name' });
    await expect(nameField).toBeVisible({ timeout: 30_000 });
    await expect(nameField, 'a refused credential must not silently freeze the form').toBeEditable();

    // ── INVERTED. This used to assert `toHaveCount(0)` for "this route offers
    // no Save control". It offers one now, and that is the whole fix.
    const saveButton = page.getByTestId('toolkit-save-button');
    await expect(
      saveButton,
      'the toolkit edit page must offer a Save control — without one a saved toolkit cannot be edited from its own page',
    ).toHaveCount(1);

    // Dirty the form, so what the control reports afterwards is about the
    // credential and not about "nothing has changed".
    await nameField.fill(`${AUTOTEST_PREFIX}renamed_${tag}`);
    const reason = page.getByTestId('toolkit-save-disabled-reason');

    if (REFUSAL_REASONS.includes(String(verdict.reason ?? ''))) {
      // A verdict ABOUT THE CREDENTIAL: refuse the save and say why.
      await expect(
        saveButton,
        'a credential the provider refused must refuse the save that would ship it',
      ).toBeDisabled({ timeout: 30_000 });
      await expect(reason, 'a refused Save must state its reason').toHaveCount(1);
      await expect(reason).toContainText(String(verdict.message ?? ''));
      await expect(saveButton).toHaveAttribute('aria-describedby', 'toolkit-save-disabled-reason');
    } else {
      // The check could not be made. That is a fact about the deployment, not
      // about the credential, and the rule must not turn it into one.
      await expect(
        saveButton,
        `a check that could not be made (reason ${JSON.stringify(verdict.reason ?? null)}) must never block a save`,
      ).toBeEnabled({ timeout: 30_000 });
      await expect(reason).toHaveCount(0);
    }
  } finally {
    await removeSeed(request, seed);
  }
});
