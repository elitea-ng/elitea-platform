/**
 * Wave-1 tail package T1a-toolkits — `toolkits-credentials/aha-toolkit`,
 * the two cases `toolkits.aha.spec.ts` (P8-toolkits-A) did not cover:
 * ELITEA-2494 (a saved credential should pre-select on the create form) and
 * ELITEA-2499 (Display Name is required).
 *
 * Every other id in this legacy folder is judged in `S/tail/ledger-T1a.tsv`
 * against that file's own AHA-1..AHA-10b tests and its own header (LIVE-ONLY
 * for every case that RUNS a tool, e.g. ELITEA-2518/2544-2562/2519-2522).
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, clickCreateButton } from '../../fixtures/api';
import { createConfiguration, deleteConfiguration } from '../../fixtures/configurations';
import { readsPlatformFlags } from '../../fixtures/platformFlags';

readsPlatformFlags(test);

const RUN_ID = String(Date.now()).slice(-6);
const tag = (label: string): string => `${AUTOTEST_PREFIX}${label}_${RUN_ID}`;

const createdCredentialIds: string[] = [];

test.afterAll(async ({ request }) => {
  for (const id of createdCredentialIds) await deleteConfiguration(request, id);
});

/** Opens the New Aha! Toolkit creation page (matches `toolkits.aha.spec.ts`'s own helper). */
async function gotoCreateAhaToolkit(page: Page): Promise<void> {
  await page.goto(BASE_URL + '/app/toolkits/all');
  await page.waitForURL(/\/app\/toolkits\/(all|create)/, { timeout: 20_000 });
  if (!/\/create/.test(page.url())) await clickCreateButton(page);
  await page.waitForURL(/\/app\/toolkits\/create/, { timeout: 20_000 });
  await expect(page.getByPlaceholder('Search toolkits')).toBeVisible({ timeout: 15_000 });
  await page.getByRole('button', { name: 'Aha!', exact: true }).click();
  await expect(page.getByRole('textbox', { name: 'Toolkit Name' })).toBeVisible({ timeout: 15_000 });
}

test('ELITEA-2494: PRODUCT GAP — the New Aha! Toolkit page does not pre-select an existing credential', async ({ page, request }) => {
  /* onetest: ELITEA-2494 — product gap: when at least one Aha! credential already exists, opening "+ Toolkit" → Aha! should land on the create page with that credential pre-selected in the Aha Configuration dropdown ("field shows empty/placeholder state" only when NONE exists). No caller anywhere in `pages/toolkits/lib/credentialPicker.tsx`, `entities/toolkit/model/toolForm.ts` or `features/credentials` ever derives a default value for this field (grepped for `preselect`/`default*[Cc]redential`/`autoSelect` — none), so the dropdown starts empty regardless. */
  test.fail(
    true,
    'ELITEA-2494: product gap — the Aha Configuration picker on the create-toolkit form never pre-selects an ' +
      'existing saved credential; no default-selection logic exists anywhere in the credential-picker/toolForm code, ' +
      'so the field always starts empty even when exactly one credential is already saved',
  );

  const credentialTitle = tag('preselect');
  const { id } = await createConfiguration(request, 'credentials', {
    title: credentialTitle,
    type: 'aha',
    shared: false,
    data: { base_url: 'https://autotest-preselect.invalid.example', api_key: 'autotest-placeholder-key' },
  });
  createdCredentialIds.push(id);

  await gotoCreateAhaToolkit(page);

  const picker = page.getByRole('combobox', { name: /Aha Configuration/i });
  await expect(picker, 'the Aha Configuration field must be present').toBeVisible({ timeout: 15_000 });
  await expect(
    picker,
    'with an existing credential saved, the create form should pre-select it rather than start empty',
  ).toContainText(credentialTitle);
});

test('ELITEA-2499: the Display Name field is required and blocks Save on the Aha! credential form', async ({ page }) => {
  /* onetest: ELITEA-2499 — leaving Display Name empty on the New Aha! Credential form keeps Save disabled (or otherwise refuses the save); filling it, together with the other required fields, clears the block. */
  await page.goto(`${BASE_URL}/app/credentials/create-credential/aha`, { waitUntil: 'domcontentloaded' });
  const nameField = page.getByRole('textbox', { name: 'Name', exact: true });
  await expect(nameField).toBeVisible({ timeout: 20_000 });

  const save = page.getByRole('button', { name: 'Save', exact: true });

  // Every OTHER required field filled, Display Name left empty.
  await page.getByRole('textbox', { name: 'Base Url' }).fill('https://autotest-required.invalid.example');
  await page.getByLabel('Api Key').fill('autotest-placeholder-required-key');
  await expect(save, 'Save must stay refused while Display Name (Name) is empty').toBeDisabled({ timeout: 15_000 });

  // Filling Display Name — and nothing else — clears the block.
  await nameField.fill(tag('required_name'));
  await expect(save, 'filling Display Name must clear the block once every other required field is already filled').toBeEnabled({
    timeout: 15_000,
  });
});
