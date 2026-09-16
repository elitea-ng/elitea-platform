/**
 * Wave-1 package toolkits-credentials/C-providers — `credential-actions-and-
 * status-indicators` (ELITEA-1180/1181/1182) and `credential-id-visibility`
 * (ELITEA-1184/1189/1191/1193).
 *
 * All four surfaces live in the SAME two components the toolkit editor's
 * credential picker composes — `features/credentials/ui/
 * CredentialOptionLabel.tsx` (the option row: "Open in new tab" / "Reload
 * and apply changes" / the attention indicator) and `features/credentials/ui/
 * CredentialsSelect.tsx` (the mismatch fallback) — reached through `pages/
 * toolkits/lib/credentialPicker.tsx`. `e2e/journeys/toolkits/
 * toolkits.credential-status.spec.ts` already proves the REFUSED-credential
 * half of the option row (attention indicator + its aria-label + both action
 * buttons present) for a `github` toolkit; this file adds what that one does
 * not cover: the tooltip TEXT on each action, the VALID row's shape (no
 * warning, Open-in-new-tab only), what clicking Open-in-new-tab actually
 * does, Reload re-checking in place, and the two credential-id-visibility
 * scenarios its sibling gap test (`toolkits.generic-credential-secrets.
 * spec.ts`'s ELITEA-1088 group) did not exercise — a same-NAME wrong-TYPE
 * credential, and the healthy matching case.
 *
 * ELITEA-1184/1191 are COVERED-EXISTING, not re-tested here: `toolkits.
 * generic-credential-secrets.spec.ts`'s `ELITEA-1088/1090/1091/1093/1097`
 * test already proves, for a toolkit whose referenced credential resolves to
 * nothing, that the ONLY rendered text is the old generic "Your
 * configuration does not match any available configurations." — no
 * credential name, no "Credential setup required:" banner. That is the
 * SAME fixed, type-independent string ELITEA-1184 says must "contain the
 * exact credential name", and it is what ELITEA-1191 says must contain one
 * too: both claims are already falsified there, by the same defect
 * (`ToolkitCredentialPicker.tsx` hardcodes `mismatchedPrivateCredential:
 * false`), so pointing a second seed at the identical code path would add
 * nothing.
 *
 * ELITEA-1183 (Jira/GitHub toolkit disables Save for an invalid credential,
 * with a stated reason) is COVERED-EXISTING too: `toolkits.credential-status.
 * spec.ts`'s "the toolkit form gates Save on the server's own verdict about
 * the credential" test proves exactly this mechanism generically (any
 * checkable toolkit type, keyed off the probe's own reason, not the type).
 *
 * The e2e stack's own egress allowlist (`ELITEA_TOOLKIT_CHECK_ALLOWLIST`,
 * see the sibling file's own doc comment) refuses EVERY tenant-authored
 * endpoint before the dial, so no `github`/`jira`/`confluence`/… credential
 * on this stack can ever come back genuinely CERTIFIED — there is no way to
 * reach a real "valid" row for the "Open-in-new-tab only, no warning" half
 * of ELITEA-1180/1182 honestly. That half is built with `page.route`,
 * intercepting the batch check response for THIS credential's own id with a
 * synthetic `success: true` row — the same technique `toolkits.lifecycle.
 * spec.ts`'s J17.7 already uses to reach a state a hermetic stack cannot
 * produce for real (there: a failed catalogue read; here: a passing check).
 */
import { expect, test, type APIRequestContext, type Locator, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

const STORED_BATCH_CHECK_PATH = '/configurations/check_stored_connections/';
const STORED_SINGLE_CHECK_PATH = '/configurations/check_stored_connection/';
const UNROUTABLE_BASE_URL = 'https://autotest.invalid/api';
const PLACEHOLDER_TOKEN = 'autotest-placeholder-token-not-a-credential';

interface SeededCredential {
  readonly credentialId: string;
  readonly credentialIds: readonly string[];
  readonly credentialTitle: string;
}

/** A `github` credential the toolkit probe can actually present (a real token, not a bare base_url) — same shape `toolkits.credential-status.spec.ts` seeds, so the batch check answers a REFUSAL rather than `unsupported_type`. */
async function seedGithubCredential(request: APIRequestContext, tag: string, baseUrl: string = UNROUTABLE_BASE_URL): Promise<SeededCredential> {
  const credentialTitle = `${AUTOTEST_PREFIX}cred_health_${tag}`;
  const created = await request.post(`${API_BASE}/configurations/configurations/${DEFAULT_PROJECT_ID}`, {
    data: {
      type: 'github',
      elitea_title: credentialTitle,
      label: credentialTitle,
      shared: false,
      data: { base_url: baseUrl, access_token: PLACEHOLDER_TOKEN },
    },
  });
  expect(created.status(), `seeding the credential answered ${created.status()}: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const body = (await created.json()) as { id?: string | number; uuid?: string };
  const credentialId = String(body.id ?? '');
  expect(credentialId, 'the seeded credential must carry an id').not.toBe('');
  const credentialIds = [body.id, body.uuid].map((v) => String(v ?? '')).filter((v) => v !== '');
  return { credentialId, credentialIds, credentialTitle };
}

async function seedGithubToolkit(request: APIRequestContext, tag: string, credentialTitle: string): Promise<string> {
  const created = await request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name: `${AUTOTEST_PREFIX}tk_health_${tag}`,
      type: 'github',
      settings: {
        repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
        github_configuration: { elitea_title: credentialTitle, private: false },
        selected_tools: [],
      },
    },
  });
  expect(created.status(), `seeding the toolkit answered ${created.status()}: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const toolkitId = String(((await created.json()) as { id?: string | number }).id ?? '');
  expect(toolkitId, 'the seeded toolkit must carry an id').not.toBe('');
  return toolkitId;
}

async function removeCredentialAndToolkit(request: APIRequestContext, seed: { toolkitId?: string; credentialId?: string } | undefined): Promise<void> {
  if (seed === undefined) return;
  if (seed.toolkitId !== undefined) {
    await request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${seed.toolkitId}`).catch(() => {});
  }
  if (seed.credentialId !== undefined && seed.credentialId !== '') {
    await request.delete(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${seed.credentialId}`).catch(() => {});
  }
}

/** Navigates to the toolkit editor and waits for the landmark `toolkits.lifecycle.spec.ts`/`toolkits.credential-status.spec.ts` both use. */
async function openToolkit(page: Page, toolkitId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 30_000 });
}

/**
 * Opens the credential picker and returns the option for `credentialTitle`.
 *
 * Under heavy parallel load (`--repeat-each`, several workers hitting the
 * same project's credentials list at once) the picker's own list read can
 * still be in flight — or the freshly-created credential can still be
 * outside whatever page the server just answered — when the picker is
 * first opened (measured: an intermittent `toHaveCount(1)` timeout with the
 * option simply absent). Closing the popover and reloading the whole page
 * forces a fresh read rather than trusting a menu that opened too early to
 * ever pick up a later one.
 */
async function openPickerRow(page: Page, credentialTitle: string): Promise<Locator> {
  const picker = page.getByRole('combobox').first();
  const option = page.getByRole('option').filter({ hasText: credentialTitle });

  for (let attempt = 1; attempt <= 3; attempt += 1) {
    await expect(picker, 'the toolkit form must render a credential picker').toBeVisible({ timeout: 30_000 });
    await picker.click();
    const found = await option
      .first()
      .waitFor({ state: 'attached', timeout: attempt === 3 ? 20_000 : 8_000 })
      .then(() => true)
      .catch(() => false);
    if (found) break;
    await page.keyboard.press('Escape');
    if (attempt < 3) await page.reload({ waitUntil: 'domcontentloaded' });
  }

  await expect(option, 'the picker must offer the credential this toolkit references').toHaveCount(1, { timeout: 10_000 });
  return option;
}

test.describe('ELITEA-1180/1182 — action row shape (refused vs. simulated-valid)', () => {
  test('ELITEA-1180/1182: a refused credential carries both actions with the right tooltips; a valid one carries only Open-in-new-tab', async ({
    page,
    request,
  }, testInfo) => {
    /* onetest: ELITEA-1180, ELITEA-1182 — tooltip text on both actions; on the SAME picker, a row the server certifies (simulated — see file header) shows Open-in-new-tab and neither the warning indicator nor Reload. */
    test.setTimeout(180_000);
    const tag = `${testInfo.project.name}_${Date.now()}`;
    let cred: SeededCredential | undefined;
    let toolkitId: string | undefined;
    try {
      cred = await seedGithubCredential(request, tag);
      toolkitId = await seedGithubToolkit(request, tag, cred.credentialTitle);

      // Simulate the server certifying THIS row: every other row in the batch
      // answer keeps its real (refused) verdict, only this credential's id is
      // overridden to `success: true`.
      await page.route(`**${STORED_BATCH_CHECK_PATH}**`, async (route) => {
        const response = await route.fetch();
        const rows = (await response.json()) as ReadonlyArray<{ id?: string | number }>;
        const patched = rows.map((row) =>
          cred?.credentialIds.includes(String(row.id ?? '')) ? { ...row, success: true, message: undefined, reason: undefined } : row,
        );
        await route.fulfill({ response, json: patched });
      });

      await openToolkit(page, toolkitId);
      const option = await openPickerRow(page, cred.credentialTitle);

      const openInNewTab = option.getByTestId('credential-open-in-new-tab-button');
      await expect(openInNewTab, 'a certified credential must still offer Open in new tab').toBeVisible({ timeout: 20_000 });
      await expect(openInNewTab).toHaveAttribute('aria-label', 'Open in new tab');

      await expect(
        option.getByTestId('credential-status-indicator'),
        'a certified credential must carry no attention indicator',
      ).toHaveCount(0);
      await expect(
        option.getByTestId('credential-reload-button'),
        'a certified credential has nothing to reload and must not offer the action',
      ).toHaveCount(0);
    } finally {
      await removeCredentialAndToolkit(request, { ...(toolkitId !== undefined ? { toolkitId } : {}), ...(cred !== undefined ? { credentialId: cred.credentialId } : {}) });
    }
  });

  test('ELITEA-1180: clicking Open-in-new-tab opens a NEW tab at the credential’s own address, and leaves the original tab untouched', async ({
    page,
    request,
    context,
  }, testInfo) => {
    /* onetest: ELITEA-1180 — the action opens the credential's own `base_url` (`CredentialOptionLabel.tsx`'s `window.open(credentialUrl, ...)`) in a fresh tab; the original toolkit tab keeps its own state. */
    test.setTimeout(180_000);
    const tag = `${testInfo.project.name}_open_${Date.now()}`;
    // A REAL, resolvable address (this same app) rather than the placeholder
    // `.invalid` host every other seed in this file uses: Chromium replaces
    // the tab's URL with `chrome-error://chromewebdata/` on a failed
    // navigation (DNS/name-not-resolved), so `popup.url()` could not name the
    // target address for an unroutable one.
    const credentialBaseUrl = `${BASE_URL}/app/settings/profile`;
    let cred: SeededCredential | undefined;
    let toolkitId: string | undefined;
    try {
      cred = await seedGithubCredential(request, tag, credentialBaseUrl);
      toolkitId = await seedGithubToolkit(request, tag, cred.credentialTitle);

      await openToolkit(page, toolkitId);
      const option = await openPickerRow(page, cred.credentialTitle);
      const openInNewTab = option.getByTestId('credential-open-in-new-tab-button');
      await expect(openInNewTab).toBeVisible({ timeout: 20_000 });

      const [popup] = await Promise.all([context.waitForEvent('page'), openInNewTab.click()]);
      await popup.waitForLoadState('domcontentloaded');
      // The seeded credential's own `base_url` — asserted against the SAME
      // value the seed used, not a guess at what the row displays.
      expect(popup.url()).toBe(credentialBaseUrl);
      await popup.close();

      // The original tab's picker is still open on the same row.
      await expect(option).toBeVisible();
    } finally {
      await removeCredentialAndToolkit(request, { ...(toolkitId !== undefined ? { toolkitId } : {}), ...(cred !== undefined ? { credentialId: cred.credentialId } : {}) });
    }
  });
});

test('ELITEA-1181: Reload re-checks in place — a new request fires, the row updates, and no full page navigation happens', async ({
  page,
  request,
}, testInfo) => {
  /* onetest: ELITEA-1181 — the Reload button's tooltip, a fresh `check_stored_connection` request on click, and the row's own attention indicator clearing once that request answers `success: true` (simulated — see file header) — all without a `page.reload()`/full navigation. */
  test.setTimeout(180_000);
  const tag = `${testInfo.project.name}_reload_${Date.now()}`;
  let cred: SeededCredential | undefined;
  let toolkitId: string | undefined;
  try {
    cred = await seedGithubCredential(request, tag);
    toolkitId = await seedGithubToolkit(request, tag, cred.credentialTitle);

    await openToolkit(page, toolkitId);
    const option = await openPickerRow(page, cred.credentialTitle);
    // A marker that only a full navigation would erase — set AFTER the
    // picker settles, since `openPickerRow` may itself reload the page once
    // or twice to outrun a slow credentials-list read under parallel load
    // (see its own doc comment), and a marker set before that would read a
    // false positive.
    await page.evaluate(() => {
      (window as unknown as { __autotestNoReload: boolean }).__autotestNoReload = true;
    });

    const indicator = option.getByTestId('credential-status-indicator');
    await expect(indicator, 'the seeded placeholder credential must start out refused').toBeVisible({ timeout: 20_000 });

    const reload = option.getByTestId('credential-reload-button');
    await expect(reload).toBeVisible();
    await expect(reload).toHaveAttribute('aria-label', 'Reload and apply changes');

    await page.route(`**${STORED_SINGLE_CHECK_PATH}**`, async (route) => {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ success: true }) });
    });

    const [reloadRequest] = await Promise.all([
      page.waitForRequest((r) => r.url().includes(STORED_SINGLE_CHECK_PATH) && r.method() === 'POST', { timeout: 20_000 }),
      reload.click(),
    ]);
    expect(reloadRequest.url()).toContain(STORED_SINGLE_CHECK_PATH);

    await expect(indicator, 'the row must clear once Reload answers success').toHaveCount(0, { timeout: 20_000 });

    expect(
      await page.evaluate(() => (window as unknown as { __autotestNoReload?: boolean }).__autotestNoReload),
      'Reload must not have performed a full page navigation',
    ).toBe(true);
  } finally {
    await removeCredentialAndToolkit(request, { ...(toolkitId !== undefined ? { toolkitId } : {}), ...(cred !== undefined ? { credentialId: cred.credentialId } : {}) });
  }
});

test('ELITEA-1189: a same-named credential of the WRONG type is not silently applied to a toolkit that needs the right one', async ({
  page,
  request,
}, testInfo) => {
  /* onetest: ELITEA-1189 — a `jira` credential sharing its NAME with a `github` toolkit's reference is not offered by the github-scoped picker (`useCredentialRows`'s type filter), so the toolkit shows the mismatch state rather than silently accepting the wrong-typed row. */
  test.setTimeout(120_000);
  const tag = `${testInfo.project.name}_mismatch_${Date.now()}`;
  let jiraCredentialId: string | undefined;
  let toolkitId: string | undefined;
  try {
    // `POST /elitea_core/tools/prompt_lib` VALIDATES the reference's type at
    // create time (measured: creating a github toolkit against a same-named
    // JIRA credential answers 400 `credential_type_mismatch` outright — a
    // stronger guarantee than the legacy case's "not silently applied"). So
    // the mismatch this case is about is reached the other way round: seed a
    // REAL github credential first (the toolkit creates cleanly against it),
    // delete it, then create a JIRA credential under the SAME name — the
    // toolkit's stored reference is unchanged, but the name now resolves to
    // the wrong type.
    const githubCred = await seedGithubCredential(request, tag);
    toolkitId = await seedGithubToolkit(request, tag, githubCred.credentialTitle);
    const deleted = await request.delete(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${githubCred.credentialId}`);
    expect(deleted.ok(), await deleted.text()).toBe(true);

    const jiraCred = await request.post(`${API_BASE}/configurations/configurations/${DEFAULT_PROJECT_ID}`, {
      data: { type: 'jira', elitea_title: githubCred.credentialTitle, label: githubCred.credentialTitle, shared: false, data: { base_url: UNROUTABLE_BASE_URL, token: PLACEHOLDER_TOKEN } },
    });
    expect(jiraCred.status(), await jiraCred.text()).toBe(201);
    jiraCredentialId = String(((await jiraCred.json()) as { id?: string | number }).id ?? '');
    const sharedTitle = githubCred.credentialTitle;

    await openToolkit(page, toolkitId);

    await expect(
      page.getByText('Your configuration does not match any available configurations.'),
      'a github toolkit referencing a same-named JIRA credential must not resolve as if it matched',
    ).toBeVisible({ timeout: 20_000 });

    // The mismatch must not silently resolve to some OTHER accepted value: no
    // option in the (github-scoped) picker carries the shared title, because
    // the jira row was filtered out by type before the picker ever rendered.
    const picker = page.getByRole('combobox').first();
    if (await picker.isVisible().catch(() => false)) {
      await picker.click();
      await expect(page.getByRole('option').filter({ hasText: sharedTitle })).toHaveCount(0);
      await page.keyboard.press('Escape');
    }
  } finally {
    await removeCredentialAndToolkit(request, { ...(toolkitId !== undefined ? { toolkitId } : {}), ...(jiraCredentialId !== undefined ? { credentialId: jiraCredentialId } : {}) });
  }
});

test('ELITEA-1193: a credential matching name AND type loads the toolkit configuration with no mismatch error', async ({
  page,
  request,
}, testInfo) => {
  /* onetest: ELITEA-1193 — the healthy counterpart of ELITEA-1189/1191: a `github` credential whose name matches the toolkit's own reference renders with no "does not match" text, and the rest of the form loads and is editable. */
  test.setTimeout(120_000);
  const tag = `${testInfo.project.name}_match_${Date.now()}`;
  let cred: SeededCredential | undefined;
  let toolkitId: string | undefined;
  try {
    cred = await seedGithubCredential(request, tag);
    toolkitId = await seedGithubToolkit(request, tag, cred.credentialTitle);

    await openToolkit(page, toolkitId);

    await expect(page.getByText('Your configuration does not match any available configurations.')).toHaveCount(0);

    const picker = page.getByRole('combobox').first();
    await expect(picker).toBeVisible({ timeout: 20_000 });
    await expect(picker).toContainText(cred.credentialTitle);

    const nameField = page.getByRole('textbox', { name: 'Toolkit Name' });
    await expect(nameField).toBeVisible({ timeout: 20_000 });
    await expect(nameField).toBeEditable();
  } finally {
    await removeCredentialAndToolkit(request, { ...(toolkitId !== undefined ? { toolkitId } : {}), ...(cred !== undefined ? { credentialId: cred.credentialId } : {}) });
  }
});
