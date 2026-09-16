/**
 * Wave-1 package toolkits-credentials/B — the credential form's secret
 * field (the "New Project/Private Secret" shortcut group) and the
 * shared-toolkit credential-mismatch banner/modal group.
 *
 * Both surfaces are `shared/ui/SecretField.tsx` / `features/credentials/ui/
 * CredentialsSelect.tsx`, reached from the CREDENTIAL create form
 * (`/credentials/create-credential`) and the toolkit editor's Credentials
 * picker respectively. See `S/port/ledger-P9-toolkits-B.tsv` for the full
 * per-case verdict; this file's own doc comments explain each defect where
 * one is asserted.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

const RUN_ID = String(Date.now()).slice(-6);
const CREDENTIAL_CREATE_ROUTE = 'credentials/create-credential';

const createdCredentialIds: string[] = [];
const createdToolkitIds: string[] = [];

test.afterAll(async ({ browser }) => {
  const ctx = await browser.newContext();
  for (const id of createdToolkitIds) {
    await ctx.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`).catch(() => {});
  }
  for (const id of createdCredentialIds) {
    await ctx.request.delete(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`).catch(() => {});
  }
  await ctx.close();
});

/** Opens the GitHub credential CREATE form through the type chooser, fills Name, and returns to just after that — the caller drives auth type/secret from here. */
async function openGithubCredentialForm(page: Page, name: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/${CREDENTIAL_CREATE_ROUTE}`, { waitUntil: 'domcontentloaded' });
  const search = page.getByPlaceholder('Search credentials');
  await expect(search).toBeVisible({ timeout: 30_000 });
  await search.fill('GitHub');
  const tile = page.getByRole('button', { name: /^GitHub$/i });
  await expect(tile).toHaveCount(1);
  await tile.click();
  await expect(page).toHaveURL(new RegExp(`/${CREDENTIAL_CREATE_ROUTE}/github`), { timeout: 20_000 });

  const nameInput = page.getByRole('textbox', { name: 'Name', exact: true });
  await expect(nameInput).toBeVisible({ timeout: 20_000 });
  await nameInput.fill(name);

  // `base_url` is github's one required field, and arrives prefilled from
  // the schema's own `prefill_value` (`credentials.toolkit-types.spec.ts`'s
  // own C-1 note) — nothing to fill here.

  // `auth` is NOT required for github (`required: false`, measured against
  // `GET /configurations/available/`), so the initial selection is
  // "Anonymous" and the Access Token field is not rendered until "Token" is
  // picked from the radio group `CredentialFormSection.tsx` draws.
  await page.getByRole('radio', { name: 'Token', exact: true }).click();
  await expect(page.getByLabel('Access Token')).toBeVisible({ timeout: 10_000 });
}

/** Switches the Access Token field to Secret mode and opens its saved-secret dropdown. */
async function openSecretDropdown(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Secret', exact: true }).click();
  await page.getByRole('combobox').click();
  await expect(page.getByRole('option', { name: 'Create new secret' })).toBeVisible({ timeout: 10_000 });
}

test('ELITEA-1073/1070: the "Create new secret" shortcut opens Settings → Secrets in a new tab with the creation row already active', async ({ page, context }) => {
  /* onetest: ELITEA-1073, ELITEA-1070 — new tab, correct `?createSecret=1` URL, creation row auto-open; back on the original tab the credential form is unchanged, and refreshing the dropdown surfaces the new secret. ELITEA-1071's own claim (the dropdown stays open across the shortcut) is demonstrably false here — see the PRODUCT GAP note below — so this test does not additionally assert it as passing. */
  test.setTimeout(120_000);
  const credentialName = `${AUTOTEST_PREFIX}cred_ghsecret_${RUN_ID}`;
  await openGithubCredentialForm(page, credentialName);
  await openSecretDropdown(page);

  const [secretsPage] = await Promise.all([context.waitForEvent('page'), page.getByRole('option', { name: 'Create new secret' }).click()]);
  await secretsPage.waitForLoadState('domcontentloaded');
  expect(secretsPage.url()).toContain('/settings/secrets');
  expect(secretsPage.url()).toContain('createSecret=1');

  // The creation row is already active: a grid with two textboxes (name, value) and a Save button, with no manual "+" click.
  const grid = secretsPage.getByRole('grid');
  await expect(grid).toBeVisible({ timeout: 20_000 });
  const rowInputs = grid.getByRole('textbox');
  await expect(rowInputs).toHaveCount(2, { timeout: 10_000 });

  const secretName = `${AUTOTEST_PREFIX}secret_${RUN_ID}`;
  await rowInputs.nth(0).fill(secretName);
  await rowInputs.nth(1).fill('placeholder-secret-value');
  await grid.getByRole('button', { name: 'Save' }).click();
  await expect(secretsPage.getByText(secretName, { exact: true })).toBeVisible({ timeout: 15_000 });
  await secretsPage.close();

  // Back on the original tab: the credential form is untouched (opening the
  // new tab closed the dropdown popup itself — MUI's Popover treats the lost
  // focus as an outside click — but the form's own data survives).
  await expect(page.getByRole('textbox', { name: 'Name', exact: true })).toHaveValue(credentialName);
  // ELITEA-1071: PRODUCT GAP — the dropdown should stay open; it does not.
  await expect(page.getByRole('option', { name: 'Create new secret' })).toHaveCount(0);

  // The refresh icon sits BESIDE the (closed) select, not inside its popup
  // (`SecretField.tsx`'s `SecretSelect`), so it works without reopening it.
  await page.getByRole('button', { name: 'Refresh secrets' }).click();

  await page.getByRole('combobox').click();
  await expect(page.getByRole('option', { name: secretName })).toBeVisible({ timeout: 15_000 });
  await page.getByRole('option', { name: secretName }).click();
  // The Select's display shows the MATCHING option's own label (the secret's
  // readable NAME), not the raw `{{secret.NAME}}` reference the value holds.
  await expect(page.getByRole('combobox', { name: 'Access Token' })).toHaveText(secretName);
});

test('ELITEA-1069/1074: PRODUCT GAP — the secret-create option carries a generic label, not a scope-aware "New Project/Private Secret" one', async ({ page }) => {
  /* onetest: ELITEA-1069, ELITEA-1074 — the CREATE section should read "New Project Secret" (team project) or "New Private Secret" (private project); `SecretField.tsx`'s own default, and every caller (`useSecretFieldOptions.ts`), never overrides `createLabel`, so it always reads "Create new secret" regardless of project type */
  test.setTimeout(90_000);
  await openGithubCredentialForm(page, `${AUTOTEST_PREFIX}cred_ghlabel_${RUN_ID}`);
  await openSecretDropdown(page);

  test.fail(true, 'ELITEA-1069 (#925)/1074: product gap — SecretFieldInput (ToolBaseProperty.renderers.tsx) calls useSecretFieldOptions() with no createLabel override, so the option always reads the generic "Create new secret", never "New Project Secret"/"New Private Secret"');
  await expect(page.getByRole('option', { name: /^New (Project|Private) Secret$/ })).toBeVisible();
});

/**
 * A real `github` credential — `base_url` is github's one required `data`
 * field (`GET /configurations/available/`).
 */
async function createGithubCredential(request: APIRequestContext, eliteaTitle: string): Promise<string> {
  const created = await request.post(`${API_BASE}/configurations/configurations/${DEFAULT_PROJECT_ID}`, {
    data: { elitea_title: eliteaTitle, label: eliteaTitle, type: 'github', data: { base_url: 'https://autotest.invalid/api' } },
  });
  expect(created.status(), `creating the github credential answered ${created.status()}: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const id = String(((await created.json()) as { id?: string | number }).id ?? '');
  if (id !== '') createdCredentialIds.push(id);
  return eliteaTitle;
}

async function createGithubToolkitReferencing(request: APIRequestContext, name: string, credentialTitle: string): Promise<string> {
  const created = await request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name,
      type: 'github',
      settings: {
        github_configuration: { elitea_title: credentialTitle, private: false },
        repository: `${AUTOTEST_PREFIX}org/${AUTOTEST_PREFIX}repo`,
        selected_tools: [],
      },
    },
  });
  expect(created.status(), `creating the github toolkit answered ${created.status()}: ${(await created.text()).slice(0, 300)}`).toBe(201);
  const id = String(((await created.json()) as { id?: string | number }).id ?? '');
  expect(id).not.toBe('');
  createdToolkitIds.push(id);
  return id;
}

test('ELITEA-1088/1090/1091/1093/1097: PRODUCT GAP — a toolkit whose credential cannot be resolved shows the old plain error text, not the styled warning banner', async ({ page }) => {
  /* onetest: ELITEA-1088, ELITEA-1090, ELITEA-1091, ELITEA-1093, ELITEA-1097 — a mismatched private credential should draw the styled `CredentialWarningBanner` ("Credential setup required:" + a prefilled "Create a credential" link); `pages/toolkits/lib/credentialPicker.tsx`'s `ToolkitCredentialPicker` hardcodes `mismatch={{ mismatchedPrivateCredential: false, … }}`, so `CredentialsSelect` always falls through to the OLD plain `FormHelperText` branch instead */
  test.setTimeout(120_000);
  // `validateToolkitCreate` answers 400 `configuration_not_found` for a reference
  // that never resolved (so a mismatch cannot be created directly) — a real
  // credential is attached first, then removed, to reach the mismatched state.
  const goneCredentialTitle = `${AUTOTEST_PREFIX}cred_gone_${RUN_ID}`;
  await createGithubCredential(page.request, goneCredentialTitle);
  const id = await createGithubToolkitReferencing(page.request, `${AUTOTEST_PREFIX}tkmismatch_${RUN_ID}`, goneCredentialTitle);
  const goneCredentialId = createdCredentialIds.at(-1);
  expect(goneCredentialId).toBeDefined();
  const deleted = await page.request.delete(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${goneCredentialId}`);
  expect(deleted.ok(), await deleted.text()).toBe(true);
  createdCredentialIds.pop();

  await page.goto(`${BASE_URL}/app/toolkits/all/${id}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('edit-toolkit-test-pane-slot')).toBeAttached({ timeout: 30_000 });

  // The plain, pre-existing text — real, and currently the ONLY rendered outcome.
  await expect(page.getByText('Your configuration does not match any available configurations.')).toBeVisible({ timeout: 20_000 });

  test.fail(true, 'ELITEA-1088 (#927)/1090/1091/1093/1097: product gap — ToolkitCredentialPicker.tsx hardcodes mismatchedPrivateCredential: false, so CredentialsSelect never renders CredentialWarningBanner for a toolkit picker, whatever the mismatch really is');
  await expect(page.getByText('Credential setup required:')).toBeVisible();
});

/*
 * ELITEA-1092/1094/1099 (Discard/Confirm on the "Credential Configuration
 * Change" modal) are LIVE-ONLY on this stack, not ported here: the toolkit
 * Save button is disabled by `useCredentialSaveGate.ts` whenever ANY
 * credential-like field on the form carries an unverified reference, and
 * `deploy/docker-compose.e2e-standalone.yml` sets
 * `ELITEA_TOOLKIT_CHECK_ALLOWLIST=elitea-main` — every credential this
 * suite can author (no real GitHub/Jira/etc. endpoint reachable) is refused
 * `unreachable` before the dial, which BLOCKS Save outright (measured: the
 * button stays `disabled`, `aria-describedby` names
 * "toolkit-save-disabled-reason": "The selected credential did not pass its
 * connection check…"). The modal itself has no author-conditional branch
 * (`credentialWarning.helpers.ts`'s `hasCredentialConfigChanged` reads only
 * the settings diff), so this is an environment limit, not a product gap —
 * see `S/port/ledger-P9-toolkits-B.tsv`.
 */
