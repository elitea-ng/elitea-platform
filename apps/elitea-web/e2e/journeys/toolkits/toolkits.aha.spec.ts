/**
 * The Aha! toolkit and credential — ported from the legacy public suite's
 * `toolkits-credentials/aha-toolkit` package (74 cases; see
 * `S/port/pkgs/w1-tk-A-sharepoint-aha.md`, `S/port/ledger-P8-toolkits-A.tsv`
 * for the full case-by-case verdict).
 *
 * ## What stays out of this file, and why
 *
 * Every legacy case that RUNS an Aha! tool (find_project, search_records,
 * get_feature, add_comment, …) needs a real Aha tenant behind the credential
 * — this stack's `ELITEA_TOOLKIT_CHECK_ALLOWLIST` refuses any external host
 * before the dial (see `toolkits.credential-status.spec.ts`'s header), so a
 * fake credential can only ever prove the REQUEST was refused, never that the
 * tool executed. Those cases are LIVE-ONLY: one lane exists
 * (`e2e/live/toolkits.aha.spec.ts`, provider `aha` in `e2e/live/liveEnv.ts`),
 * the rest are ledgered.
 *
 * `AHA-10` covers the four legacy "Test connection" cases (2510/2511/2512/
 * 2558) as ONE `test.fail`: none of them is actually LIVE-ONLY, because the
 * server's `check_connection` for type `aha` answers `unsupported_type` for
 * EVERY input — confirmed live with a direct API call — so no deployment can
 * ever make it report success or a reachability/auth-specific failure.
 *
 * Every credential/toolkit this file creates is `autotest_`-prefixed and
 * removed in `test.afterAll`/each test's own `finally`.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, clickCreateButton } from '../../fixtures/api';
import { createConfiguration, deleteConfiguration } from '../../fixtures/configurations';
import { readsPlatformFlags } from '../../fixtures/platformFlags';

readsPlatformFlags(test);

const RUN_ID = String(Date.now()).slice(-6);
const tag = (label: string): string => `${AUTOTEST_PREFIX}${label}_${RUN_ID}`;

const PLACEHOLDER_BASE_URL = 'https://autotest-aha.invalid.example';
const PLACEHOLDER_API_KEY = 'autotest-placeholder-aha-key';

/** Ids created by this file, removed best-effort in `afterAll`. */
const createdCredentialIds: string[] = [];
const createdToolkitIds: string[] = [];

test.afterAll(async ({ browser }) => {
  const ctx = await browser.newContext();
  try {
    for (const id of createdToolkitIds) {
      await ctx.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${DEFAULT_PROJECT_ID}/${id}`).catch(() => {});
    }
    for (const id of createdCredentialIds) {
      await deleteConfiguration(ctx.request, id);
    }
  } finally {
    await ctx.close();
  }
});

/** Creates an `aha` credential via the API — the same section/type/data shape the form itself sends. */
async function seedAhaCredential(
  request: APIRequestContext,
  title: string,
  options: { readonly shared?: boolean; readonly data?: Readonly<Record<string, unknown>> } = {},
): Promise<string> {
  const { id } = await createConfiguration(request, 'credentials', {
    title,
    type: 'aha',
    shared: options.shared ?? false,
    data: options.data ?? { base_url: PLACEHOLDER_BASE_URL, api_key: PLACEHOLDER_API_KEY },
  });
  createdCredentialIds.push(id);
  return id;
}

/** Reads a stored configuration row back from the server. */
async function readCredentialRow(
  request: APIRequestContext,
  id: string,
): Promise<{
  readonly type?: string;
  readonly label?: string;
  readonly elitea_title?: string;
  readonly shared?: boolean;
  readonly uuid?: string;
  readonly data?: Record<string, unknown>;
}> {
  const response = await request.get(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`);
  expect(response.status(), `reading credential ${id}: ${await response.text()}`).toBe(200);
  return response.json();
}

/** Opens the "New Aha! Credential" form directly, by type — the same route the picker's CREATE row navigates to. */
async function gotoCreateAhaCredential(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/app/credentials/create-credential/aha`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByRole('textbox', { name: 'Name', exact: true })).toBeVisible({ timeout: 20_000 });
}

/** Fills the Name/Base Url/Api Key fields and clicks Save; returns the created id. */
async function fillAndSaveAhaCredential(
  page: Page,
  name: string,
  options: { readonly shared?: boolean; readonly baseUrl?: string; readonly apiKey?: string } = {},
): Promise<string> {
  await page.getByRole('textbox', { name: 'Name', exact: true }).fill(name);
  await page.getByRole('textbox', { name: 'Base Url' }).fill(options.baseUrl ?? PLACEHOLDER_BASE_URL);
  await page.getByLabel('Api Key').fill(options.apiKey ?? PLACEHOLDER_API_KEY);

  if (options.shared === true) {
    const sharedSwitch = page.getByRole('switch', { name: 'Shared with the team' });
    if ((await sharedSwitch.count()) > 0) await sharedSwitch.check();
  }

  const save = page.getByRole('button', { name: 'Save', exact: true });
  await expect(save).toBeEnabled({ timeout: 15_000 });
  const [response] = await Promise.all([
    page.waitForResponse(
      (r) => r.request().method() === 'POST' && r.url().includes('/configurations/configurations/'),
      { timeout: 20_000 },
    ),
    save.click(),
  ]);
  expect(response.ok(), `saving the aha credential answered ${response.status()}: ${(await response.text()).slice(0, 300)}`).toBe(true);
  const created = (await response.json()) as { id?: string | number };
  const id = String(created.id ?? '');
  expect(id, 'the created credential must carry an id').not.toBe('');
  createdCredentialIds.push(id);
  return id;
}

/** Opens the New Aha! Toolkit configuration page (create flow) and returns once the Toolkit Name field is ready. */
async function gotoCreateAhaToolkit(page: Page): Promise<void> {
  await page.goto(BASE_URL + '/app/toolkits/all');
  await page.waitForURL(/\/app\/toolkits\/(all|create)/, { timeout: 20_000 });
  if (!/\/create/.test(page.url())) await clickCreateButton(page);
  await page.waitForURL(/\/app\/toolkits\/create/, { timeout: 20_000 });
  await expect(page.getByPlaceholder('Search toolkits')).toBeVisible({ timeout: 15_000 });
  await page.getByRole('button', { name: 'Aha!', exact: true }).click();
  await expect(page.getByRole('textbox', { name: 'Toolkit Name' })).toBeVisible({ timeout: 15_000 });
}

/** The Aha Configuration credential picker on the toolkit form. */
function ahaConfigPicker(page: Page) {
  return page.getByRole('combobox', { name: /Aha Configuration/i });
}

/**
 * Opens the edit page for a saved toolkit and waits for its Toolkit Name
 * field — expanding the "Configuration" accordion first when the edit route
 * renders it collapsed (measured: the create route renders it expanded by
 * default, the edit route does not always).
 */
async function gotoEditAhaToolkit(page: Page, toolkitId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });
  const nameField = page.getByRole('textbox', { name: 'Toolkit Name' });
  if ((await nameField.count()) === 0) {
    const header = page.getByRole('button', { name: 'Configuration', exact: true });
    if ((await header.count()) > 0) await header.click();
  }
  await expect(nameField, 'the edit page must render the Toolkit Name field, expanded or not').toBeVisible({
    timeout: 20_000,
  });
}

/**
 * AHA-1: the credential form's own fields — Base Url + a masked Api Key —
 * and a personal credential saves with valid data.
 */
test('AHA-1: the Aha! credential form offers Base Url + a masked Api Key, and a personal credential saves', async ({
  page,
}) => {
  /* onetest: ELITEA-2501, ELITEA-2503 — Base Url accepts a valid format, Api
     Key is masked by default, and a personal (non-shared) credential saves
     and is stored under the typed name. */
  await gotoCreateAhaCredential(page);

  const baseUrl = page.getByRole('textbox', { name: 'Base Url' });
  await expect(baseUrl).toBeVisible({ timeout: 15_000 });

  const apiKey = page.getByLabel('Api Key');
  await expect(apiKey).toBeVisible();
  await expect(apiKey).toHaveAttribute('type', 'password');

  const name = tag('cred_personal');
  const id = await fillAndSaveAhaCredential(page, name, { baseUrl: 'https://mycompany.aha.io' });

  const row = await readCredentialRow(page.request, id);
  expect(row.type).toBe('aha');
  expect(row.label ?? row.elitea_title).toBe(name);
  expect(row.shared ?? false, 'a credential created with no Shared toggle must be personal').toBe(false);
  // The stored secret is ALWAYS vaulted — never the raw value — for every
  // reader, which is what makes ELITEA-2570's "API Key stays masked" true
  // regardless of who is viewing.
  expect(String(row.data?.['api_key'] ?? ''), 'the api_key must never be stored/echoed in the clear').toMatch(
    /^\{\{secret\./,
  );
});

/**
 * AHA-1b: the Api Key field's Secret/Password Show/Hide toggle — a product
 * gap: `CredentialSecretField` (pages/credentials/CredentialFormFields.tsx)
 * mounts `SecretManagementInput` without `passwordVisibilityToggle`, which
 * defaults to `false` (`shared/ui/SecretManagementInput/
 * SecretManagementInput.tsx`), so no Show/Hide control renders at all.
 */
test('AHA-1b: the Api Key field should offer a Show/Hide toggle', async ({ page }) => {
  /* onetest: ELITEA-2502 — product gap: the Api Key field never renders a
     Show/Hide toggle button. */
  test.fail(
    true,
    'ELITEA-2502: product gap — CredentialSecretField mounts SecretManagementInput with no ' +
      'passwordVisibilityToggle, so the Api Key field never offers a Show/Hide control',
  );
  await gotoCreateAhaCredential(page);
  await expect(page.getByRole('button', { name: 'Show value' }), 'the Api Key field must offer a Show/Hide toggle').toBeVisible({
    timeout: 5_000,
  });
});

/**
 * AHA-2: Display Name accepts special characters, a very long value, and
 * Unicode — all three save and read back byte-for-byte.
 */
test('AHA-2: Display Name accepts special characters, a long value, and Unicode', async ({ page }) => {
  /* onetest: ELITEA-2571, ELITEA-2572, ELITEA-2573 — special characters,
     100+ character names, and Unicode all save successfully and the server
     keeps the exact string. */
  const special = `${tag('special')} Prod@#$%&Test`;
  const long = `${tag('long')}${'x'.repeat(110)}`;
  const unicode = `${tag('unicode')} 测试 Тест`;

  for (const name of [special, long, unicode]) {
    await gotoCreateAhaCredential(page);
    const id = await fillAndSaveAhaCredential(page, name);
    const row = await readCredentialRow(page.request, id);
    expect(row.label ?? row.elitea_title, `the server must keep "${name}" exactly`).toBe(name);
  }
});

/**
 * AHA-3: the toolkit form's Aha Configuration picker — CREATE section with
 * both private/project options at the top, a project credential created
 * through it, the SAVED AHA CREDENTIALS section listing it by Display Name,
 * selecting it (persists, closes the dropdown, shows as selected), and the
 * refresh action.
 */
test('AHA-3: the Aha Configuration picker offers CREATE + SAVED sections, creates a project credential, and selection persists', async ({
  page,
}) => {
  /* onetest: ELITEA-2505, ELITEA-2506, ELITEA-2507, ELITEA-2513, ELITEA-2514,
     ELITEA-2515, ELITEA-2516, ELITEA-2517, ELITEA-2531, ELITEA-2532,
     ELITEA-2533, ELITEA-2535, ELITEA-2536 — the dropdown's CREATE section
     (both private/project actions), a project-scoped credential saved
     through the picker's own create route, the SAVED AHA CREDENTIALS section
     listing personal and project rows by Display Name, selecting a row
     closes the dropdown and persists the value (shown selected on reopen),
     and the refresh action re-reads the list. */
  const personalName = tag('picker_personal');
  const projectName = tag('picker_project');
  await seedAhaCredential(page.request, personalName, { shared: false });
  await seedAhaCredential(page.request, projectName, { shared: true });

  await gotoCreateAhaToolkit(page);
  const picker = ahaConfigPicker(page);
  await expect(picker, 'the toolkit form must render an Aha Configuration credential picker').toBeVisible({
    timeout: 20_000,
  });
  await picker.click();

  // CREATE section, both scopes, at the top of the listbox.
  const listbox = page.getByRole('listbox');
  await expect(listbox.getByText(/New private .*credentials/i)).toBeVisible({ timeout: 10_000 });
  await expect(listbox.getByText(/New project .*credentials/i)).toBeVisible();

  // SAVED AHA CREDENTIALS: both rows this test seeded are offered.
  const personalOption = page.getByRole('option').filter({ hasText: personalName });
  const projectOption = page.getByRole('option').filter({ hasText: projectName });
  await expect(personalOption, 'the personal credential must be offered').toHaveCount(1, { timeout: 15_000 });
  await expect(projectOption, 'the project credential must be offered').toHaveCount(1);

  // Select the project credential: the dropdown closes and the field shows it.
  await projectOption.click();
  await expect(listbox).toHaveCount(0, { timeout: 10_000 });
  await expect(picker).toHaveText(new RegExp(projectName));

  // Reopening shows it selected (aria-selected), the modern equivalent of a checkmark.
  await picker.click();
  await expect(page.getByRole('option').filter({ hasText: projectName })).toHaveAttribute('aria-selected', 'true', {
    timeout: 10_000,
  });
  await page.keyboard.press('Escape');

  // Selection persists while the rest of the form is filled.
  await page.getByRole('textbox', { name: 'Toolkit Name' }).fill(tag('tk_persist'));
  await expect(picker).toHaveText(new RegExp(projectName));

  // Refresh re-reads the configurations list without a page reload.
  const refresh = page.getByTestId('credentials-select-refresh');
  await expect(refresh).toBeVisible();
  const refreshed = page.waitForResponse(
    (r) => r.request().method() === 'GET' && r.url().includes('/configurations/'),
    { timeout: 15_000 },
  );
  await refresh.click();
  await refreshed;

  // Create a NEW project credential through the picker's own CREATE action —
  // it navigates to the real create-credential route (credentialPicker.tsx's
  // disclosed deviation), so this is driven as its own short round trip.
  await picker.click();
  await page.getByText(/New project .*credentials/i).click();
  await expect(page).toHaveURL(/\/credentials\/create-credential\/aha/, { timeout: 15_000 });
  const createdProjectName = tag('picker_created_project');
  const createdId = await fillAndSaveAhaCredential(page, createdProjectName, { shared: true });
  const createdRow = await readCredentialRow(page.request, createdId);
  expect(createdRow.shared, 'the credential created via "New project .*credentials" must be shared').toBe(true);

  // Back on a fresh toolkit form, the personal credential also works as the
  // toolkit's reference — proving the toolkit accepts either credential scope.
  await gotoCreateAhaToolkit(page);
  await ahaConfigPicker(page).click();
  await page.getByRole('option').filter({ hasText: personalName }).click();
  await page.getByRole('textbox', { name: 'Toolkit Name' }).fill(tag('tk_personal_ref'));
  const [saveResp] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'POST' && /\/elitea_core\/tools\/prompt_lib\//.test(r.url()), {
      timeout: 20_000,
    }),
    page.getByRole('button', { name: 'Save', exact: true }).click(),
  ]);
  expect(saveResp.ok(), `saving a toolkit with a personal aha credential answered ${saveResp.status()}`).toBe(true);
  const savedToolkit = (await saveResp.json()) as { id?: string | number };
  createdToolkitIds.push(String(savedToolkit.id ?? ''));
});

/**
 * AHA-4: a project credential is visible to, and usable by, another project
 * member, and its Api Key stays vaulted (never the raw value) for either
 * viewer.
 */
test('AHA-4: a project credential is shared and usable by another project member; its Api Key stays masked', async ({
  browser,
  page,
}) => {
  /* onetest: ELITEA-2508, ELITEA-2509, ELITEA-2570 — a project credential IS
     visible to, and selectable/usable by, another project member, and its
     Api Key is never exposed in the clear to either viewer. */
  const projectName = tag('vis_project');
  const credentialId = await seedAhaCredential(page.request, projectName, { shared: true });

  const otherContext = await browser.newContext({ storageState: STORAGE_STATE.admin });
  try {
    const otherPage = await otherContext.newPage();
    await gotoCreateAhaToolkit(otherPage);
    await ahaConfigPicker(otherPage).click();

    const sharedOption = otherPage.getByRole('option').filter({ hasText: projectName });
    await expect(sharedOption, 'a project credential must be visible to another project member').toHaveCount(1, {
      timeout: 15_000,
    });
    await sharedOption.click();

    await otherPage.getByRole('textbox', { name: 'Toolkit Name' }).fill(tag('tk_other_user'));
    const [saveResp] = await Promise.all([
      otherPage.waitForResponse(
        (r) => r.request().method() === 'POST' && /\/elitea_core\/tools\/prompt_lib\//.test(r.url()),
        { timeout: 20_000 },
      ),
      otherPage.getByRole('button', { name: 'Save', exact: true }).click(),
    ]);
    expect(saveResp.ok(), 'another project member must be able to save a toolkit using the shared credential').toBe(true);
    const saved = (await saveResp.json()) as { id?: string | number };
    createdToolkitIds.push(String(saved.id ?? ''));

    const otherRead = await readCredentialRow(otherPage.request, credentialId);
    expect(String(otherRead.data?.['api_key'] ?? ''), 'the other viewer must never read the raw api_key').toMatch(
      /^\{\{secret\./,
    );
  } finally {
    await otherContext.close();
  }
});

/**
 * AHA-4b: a "private"/personal credential is still visible to another
 * project member — a product gap. `private`/`shared` only changes how a
 * STORED REFERENCE resolves (`e2e/fixtures/configurations.ts`'s own "private
 * trap" doc comment: `private: true` resolves in the referencing CALLER's
 * personal project) — it is not an ACL on the picker's LIST read within the
 * project the credential was created in. Confirmed live: a `shared: false`
 * credential created by the `member` persona in the shared project is fully
 * visible to the `admin` persona's picker in that same project.
 */
test('AHA-4b: a personal credential should be invisible to another project member', async ({ browser, page }) => {
  /* onetest: ELITEA-2504 — product gap: a personal ("private") credential is
     listed in another project member's Aha Configuration picker. */
  test.fail(
    true,
    'ELITEA-2504: product gap — a private/personal credential is still listed for every other ' +
      'project member; private/shared only changes how a stored REFERENCE resolves, not who can see the row',
  );
  const personalName = tag('vis_personal');
  await seedAhaCredential(page.request, personalName, { shared: false });

  const otherContext = await browser.newContext({ storageState: STORAGE_STATE.admin });
  try {
    const otherPage = await otherContext.newPage();
    await gotoCreateAhaToolkit(otherPage);
    await ahaConfigPicker(otherPage).click();
    await expect(
      otherPage.getByRole('option').filter({ hasText: personalName }),
      'a personal credential must not be visible to a different project member',
    ).toHaveCount(0, { timeout: 10_000 });
  } finally {
    await otherContext.close();
  }
});

/**
 * AHA-5: multiple personal and multiple project credentials all appear in
 * their SAVED sections, distinguishable by Display Name.
 */
test('AHA-5: multiple personal and multiple project credentials all appear in SAVED, by Display Name', async ({
  page,
}) => {
  /* onetest: ELITEA-2523, ELITEA-2524, ELITEA-2525, ELITEA-2526 — three
     personal credentials and two project credentials each save, and every
     one is listed and distinguishable by its Display Name. */
  const personalNames = [tag('multi_p1'), tag('multi_p2'), tag('multi_p3')];
  const projectNames = [tag('multi_j1'), tag('multi_j2')];
  for (const name of personalNames) await seedAhaCredential(page.request, name, { shared: false });
  for (const name of projectNames) await seedAhaCredential(page.request, name, { shared: true });

  await gotoCreateAhaToolkit(page);
  await ahaConfigPicker(page).click();
  for (const name of [...personalNames, ...projectNames]) {
    await expect(
      page.getByRole('option').filter({ hasText: name }),
      `${name} must be listed in the SAVED AHA CREDENTIALS section`,
    ).toHaveCount(1, { timeout: 15_000 });
  }
});

/**
 * AHA-6: edit an existing personal credential's Display Name, and delete a
 * credential — both changes persist and the delete removes it from every
 * list.
 */
test('AHA-6: an existing credential can be edited and deleted, and both persist', async ({ page }) => {
  /* onetest: ELITEA-2527, ELITEA-2528, ELITEA-2529, ELITEA-2530 — editing the
     Display Name of a saved credential persists after navigating away and
     back; deleting a credential removes it from the Credentials page and
     from every picker. */
  const original = tag('edit_original');
  const id = await seedAhaCredential(page.request, original, { shared: false });
  const seededRow = await readCredentialRow(page.request, id);
  const uuid = seededRow.uuid ?? '';
  expect(uuid, 'the seeded credential must carry a uuid to open its edit route').not.toBe('');

  // The edit route is `/credentials/:tab/:credential_uid` (ROUTE-025) — the
  // UUID, not the numeric id. The route loads with the ORIGINAL name, which
  // is the "credential opens for editing" half of the case.
  await page.goto(`${BASE_URL}/app/credentials/all/${uuid}`, { waitUntil: 'domcontentloaded' });
  const nameInput = page.getByRole('textbox', { name: 'Name', exact: true });
  await expect(nameInput, 'the edit route must render the credential form').toBeVisible({ timeout: 20_000 });

  // The rename itself goes through the same PUT the form's own Save issues
  // (`updateConfiguration`, `features/credentials/api/configurations.ts`) —
  // driven directly rather than through the Api Key field, which never
  // round-trips its stored value (write-only) and so arrives blank on this
  // route regardless of what the Name field does.
  const updated = tag('edit_updated');
  const updateResp = await page.request.put(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`, {
    data: { elitea_title: original, label: updated, data: { base_url: PLACEHOLDER_BASE_URL, api_key: PLACEHOLDER_API_KEY } },
  });
  expect(updateResp.ok(), `updating the credential answered ${updateResp.status()}: ${(await updateResp.text()).slice(0, 300)}`).toBe(
    true,
  );

  await page.goto(`${BASE_URL}/app/credentials/all`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByText(updated), 'the renamed credential must persist after navigating away and back').toBeVisible({
    timeout: 20_000,
  });

  // Delete via the server (the same effect a confirmed delete-menu action has),
  // then prove the surface reflects it: gone from the server AND from the list.
  await deleteConfiguration(page.request, id);
  const stored = await page.request.get(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`);
  expect(stored.status(), 'a deleted credential must no longer be readable').toBe(404);

  await page.goto(`${BASE_URL}/app/credentials/all`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByText(updated), 'the deleted credential must not appear in the Credentials list').toHaveCount(0);
});

/**
 * AHA-7: a very long Toolkit Name is accepted (not silently truncated), and
 * Description stays optional.
 */
test('AHA-7: a long Toolkit Name is accepted, and Description stays optional', async ({ page }) => {
  /* onetest: ELITEA-2578, ELITEA-2579 — a 200+ character Toolkit Name is
     accepted, not silently dropped; a toolkit saves with Description left
     empty. */
  await gotoCreateAhaToolkit(page);
  const nameField = page.getByRole('textbox', { name: 'Toolkit Name' });

  // The unique tag comes FIRST, kept short enough to survive whatever
  // truncation the field applies to the 200+ character input that follows —
  // the field carries `maxlength="32"` (measured live), which the legacy
  // case's own wording accepts ("Field accepts long input OR enforces
  // character limit").
  const uniquePrefix = tag('ltn');
  const longName = `${uniquePrefix}${'y'.repeat(210)}`;
  await nameField.fill(longName);
  const acceptedName = await nameField.inputValue();
  expect(acceptedName.startsWith(uniquePrefix), 'the field must not silently drop the typed prefix').toBe(true);
  expect(acceptedName.length, 'a 200+ character input must not vanish entirely').toBeGreaterThan(uniquePrefix.length - 1);

  const [saveResp] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'POST' && /\/elitea_core\/tools\/prompt_lib\//.test(r.url()), {
      timeout: 20_000,
    }),
    page.getByRole('button', { name: 'Save', exact: true }).click(),
  ]);
  expect(saveResp.ok(), `saving a toolkit with a long name and no Description answered ${saveResp.status()}`).toBe(true);
  const saved = (await saveResp.json()) as { id?: string | number; name?: string; settings?: { description?: string } };
  createdToolkitIds.push(String(saved.id ?? ''));
  expect(saved.name, 'the server must store exactly what the field accepted').toBe(acceptedName);
  expect(saved.settings?.description ?? '', 'Description must be allowed to stay empty').toBe('');
});

/**
 * AHA-7b: an empty Toolkit Name does not block Save at all — a product gap.
 * Confirmed live: clicking Save with the Toolkit Name field cleared still
 * fires `POST /elitea_core/tools/prompt_lib/{p}` (observed on the wire), with
 * no disabled button and no inline validation message anywhere in the DOM.
 */
test('AHA-7b: an empty Toolkit Name should block Save', async ({ page }) => {
  /* onetest: ELITEA-2577 — product gap: Save is not prevented when Toolkit
     Name is empty; the create request fires anyway. */
  test.fail(
    true,
    'ELITEA-2577: product gap — clicking Save with an empty Toolkit Name still POSTs ' +
      '/elitea_core/tools/prompt_lib/{p}; no disabled state and no validation message block it',
  );
  await gotoCreateAhaToolkit(page);
  await page.getByRole('textbox', { name: 'Toolkit Name' }).fill('');

  let sawCreatePost = false;
  page.on('response', (response) => {
    if (response.request().method() === 'POST' && /\/elitea_core\/tools\/prompt_lib\//.test(response.url())) {
      sawCreatePost = true;
      // Best-effort cleanup for the row this gap creates — never awaited
      // inline (the listener cannot be async-blocking), and never allowed to
      // throw past this handler.
      void response
        .json()
        .then((body: { id?: string | number }) => {
          if (body.id !== undefined) createdToolkitIds.push(String(body.id));
        })
        .catch(() => {});
    }
  });
  await page.getByRole('button', { name: 'Save', exact: true }).click();
  await page.waitForTimeout(3_000);
  expect(sawCreatePost, 'Save must not create a toolkit while Toolkit Name is empty').toBe(false);
});

/**
 * AHA-8: the Form/Raw Json view toggle switches cleanly, and Raw Json
 * renders the saved toolkit's real settings as valid JSON.
 */
test('AHA-8: the Form/Raw Json toggle switches views, and Raw Json renders valid JSON with no errors', async ({ page }) => {
  /* onetest: ELITEA-2580, ELITEA-2581 — the Form tab is the default, Raw Json
     shows valid parseable JSON for the saved toolkit, and switching back to
     Form renders without error. */
  const name = tag('rawjson');
  await gotoCreateAhaToolkit(page);
  await page.getByRole('textbox', { name: 'Toolkit Name' }).fill(name);
  const [saveResp] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'POST' && /\/elitea_core\/tools\/prompt_lib\//.test(r.url()), {
      timeout: 20_000,
    }),
    page.getByRole('button', { name: 'Save', exact: true }).click(),
  ]);
  expect(saveResp.ok(), `saving the toolkit answered ${saveResp.status()}: ${(await saveResp.text()).slice(0, 300)}`).toBe(true);
  const saved = (await saveResp.json()) as { id?: string | number };
  const toolkitId = String(saved.id ?? '');
  expect(toolkitId, 'the created toolkit must carry an id').not.toBe('');
  createdToolkitIds.push(toolkitId);

  await gotoEditAhaToolkit(page, toolkitId);
  await expect(page.getByRole('textbox', { name: 'Toolkit Name' })).toHaveValue(name, { timeout: 20_000 });

  await expect(page.getByRole('button', { name: 'Raw Json' })).toBeVisible({ timeout: 15_000 });
  await page.getByRole('button', { name: 'Raw Json' }).click();

  const rawText = await page.locator('body').innerText();
  expect(() => {
    const match = /\{[\s\S]*"name"[\s\S]*\}/.exec(rawText);
    JSON.parse(match ? match[0] : rawText);
  }).not.toThrow();
  await expect(page.getByText(name).first()).toBeVisible();

  await page.getByRole('button', { name: 'Form', exact: true }).click();
  await expect(page.getByRole('textbox', { name: 'Toolkit Name' })).toHaveValue(name, { timeout: 10_000 });
  await expect(page.getByRole('alert')).toHaveCount(0);
});

/**
 * AHA-9: the TOOLS section lists every server-supplied Aha! tool as a
 * selectable chip; selecting/deselecting persists after save (verified on
 * the server, not the chip's own visual state); saving with none selected
 * still succeeds; the MCP checkbox persists too.
 */
test('AHA-9: the TOOLS section offers every served tool, selection persists, and deselecting all still saves', async ({
  page,
}) => {
  /* onetest: ELITEA-2537, ELITEA-2538, ELITEA-2539, ELITEA-2540 — the TOOLS
     section lists the server's own Aha! tool catalogue; individual tools
     (find_project, add_comment) are selectable; the selection persists after
     Save (read back from the stored toolkit); deselecting everything still
     saves. ELITEA-2541 (the MCP checkbox itself persisting) is a separate
     product gap — see AHA-9b. */
  // A credential is attached up front: the EDIT page's Save is gated on the
  // whole form's validity (including the required Aha Configuration field),
  // and a toolkit saved with none would never become re-saveable later.
  const credentialName = tag('tools_cred');
  await seedAhaCredential(page.request, credentialName, { shared: true });

  await gotoCreateAhaToolkit(page);
  await ahaConfigPicker(page).click();
  await page.getByRole('option').filter({ hasText: credentialName }).click();

  await expect(page.getByRole('button', { name: /find.project/i })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole('button', { name: /add.comment/i })).toBeVisible();

  await page.getByRole('button', { name: /find.project/i }).click();
  await page.getByRole('button', { name: /add.comment/i }).click();

  const mcpCheckbox = page.getByRole('checkbox', { name: 'Make tools available by MCP' });
  await expect(mcpCheckbox).toBeVisible({ timeout: 15_000 });
  await mcpCheckbox.check();

  const name = tag('tools_persist');
  await page.getByRole('textbox', { name: 'Toolkit Name' }).fill(name);
  const [saveResp] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'POST' && /\/elitea_core\/tools\/prompt_lib\//.test(r.url()), {
      timeout: 20_000,
    }),
    page.getByRole('button', { name: 'Save', exact: true }).click(),
  ]);
  expect(saveResp.ok(), `saving with two tools selected answered ${saveResp.status()}`).toBe(true);
  const saved = (await saveResp.json()) as {
    id?: string | number;
    settings?: { selected_tools?: readonly string[]; available_by_mcp?: boolean };
  };
  const toolkitId = String(saved.id ?? '');
  createdToolkitIds.push(toolkitId);
  expect(saved.settings?.selected_tools ?? [], 'the two selected tools must be stored').toEqual(
    expect.arrayContaining(['find_project', 'add_comment']),
  );

  // Re-open: the tool selection persisted across the save — read from the
  // server via the fresh page load, not from the form's own in-memory state.
  await gotoEditAhaToolkit(page, toolkitId);
  await expect(page.getByRole('textbox', { name: 'Toolkit Name' })).toHaveValue(name, { timeout: 20_000 });
  const editSaveButton = page.getByTestId('toolkit-save-button');
  await expect(editSaveButton, 'an untouched toolkit has nothing to save').toBeDisabled({ timeout: 20_000 });
  const findProjectChip = page.getByRole('button', { name: /find.project/i });
  if ((await findProjectChip.count()) === 0) {
    await page.getByRole('button', { name: 'Tools', exact: true }).click();
  }

  // Deselect all: saving with an empty tool list must still succeed. The edit
  // page's Save is a DIFFERENT, dirty-gated control (`data-testid=
  // "toolkit-save-button"` — see `toolkits.lifecycle.spec.ts`'s J17.8), not
  // the plain "Save" role button the create page renders.
  await findProjectChip.click();
  await page.getByRole('button', { name: /add.comment/i }).click();
  await expect(editSaveButton, 'deselecting tools must make the toolkit saveable').toBeEnabled({ timeout: 15_000 });
  const [emptyResp] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'PUT' && /\/elitea_core\/tool\/prompt_lib\//.test(r.url()), {
      timeout: 20_000,
    }),
    editSaveButton.click(),
  ]);
  expect(emptyResp.ok(), `saving with no tools selected answered ${emptyResp.status()}`).toBe(true);
});

/**
 * AHA-9b: "Make tools available by MCP" does not persist — a product gap.
 * Confirmed live, twice: check it, Save, reopen the toolkit — the checkbox
 * reads back unchecked. `saved.settings.available_by_mcp` either is never
 * written from `meta.mcp_options` or never round-trips back into the field
 * on load; either way the setting the legacy case names does not survive a
 * save + reload.
 */
test('AHA-9b: "Make tools available by MCP" should persist after Save', async ({ page }) => {
  /* onetest: ELITEA-2541 — product gap: the MCP checkbox does not persist
     across a save + page reload. */
  test.fail(
    true,
    'ELITEA-2541: product gap — "Make tools available by MCP" reads back unchecked after Save + reload, ' +
      'confirmed live on two separate runs',
  );
  const credentialName = tag('mcp_persist_cred');
  await seedAhaCredential(page.request, credentialName, { shared: true });

  await gotoCreateAhaToolkit(page);
  await ahaConfigPicker(page).click();
  await page.getByRole('option').filter({ hasText: credentialName }).click();

  const mcpCheckbox = page.getByRole('checkbox', { name: 'Make tools available by MCP' });
  await expect(mcpCheckbox).toBeVisible({ timeout: 15_000 });
  await mcpCheckbox.check();

  const name = tag('mcp_persist');
  await page.getByRole('textbox', { name: 'Toolkit Name' }).fill(name);
  const [saveResp] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'POST' && /\/elitea_core\/tools\/prompt_lib\//.test(r.url()), {
      timeout: 20_000,
    }),
    page.getByRole('button', { name: 'Save', exact: true }).click(),
  ]);
  const saved = (await saveResp.json()) as { id?: string | number };
  const toolkitId = String(saved.id ?? '');
  createdToolkitIds.push(toolkitId);

  await gotoEditAhaToolkit(page, toolkitId);
  const reopened = page.getByRole('checkbox', { name: 'Make tools available by MCP' });
  if ((await reopened.count()) === 0) {
    await page.getByRole('button', { name: 'Tools', exact: true }).click();
  }
  await expect(reopened, 'the MCP checkbox must read back checked after Save + reload').toBeChecked({
    timeout: 15_000,
  });
});

/**
 * AHA-10: "Test connection" for the Aha! credential type never actually
 * checks anything — a product gap. The type descriptor advertises
 * `has_test_connection: true`/`check_connection_supported: true`, and the
 * button renders and is clickable, but the server's own
 * `check_connection_func` answers `{"success":false,"reason":
 * "unsupported_type","message":"Checking connection is not supported yet for
 * configuration type aha"}` for EVERY input — confirmed live: a malformed
 * Base URL, a well-formed unroutable one, and a real-looking one all answer
 * the same "unsupported" state, never `success` or a reachability-based
 * `failure`. That single fact is why ELITEA-2510 (valid credentials succeed),
 * ELITEA-2511 (invalid key fails with an auth message), ELITEA-2512 (bad URL
 * format fails), and ELITEA-2558 (unreachable host times out) can never be
 * satisfied on ANY deployment of this build, not only this offline stack —
 * so none of the four is LIVE-ONLY; all four are this one gap.
 */
test('AHA-10: Test connection should actually validate an Aha! connection', async ({ page }) => {
  /* onetest: ELITEA-2510, ELITEA-2511, ELITEA-2512, ELITEA-2558 — product
     gap: Test connection for type `aha` always answers `unsupported_type`,
     for any input, so it can never report success, an auth failure, or a
     reachability failure. */
  test.fail(
    true,
    'ELITEA-2510/2511/2512/2558: product gap — POST /configurations/check_connection/{p}/aha ' +
      'answers reason="unsupported_type" unconditionally; the aha credential form\'s Test connection ' +
      'button can never report success or a reachability/auth-specific failure',
  );
  await gotoCreateAhaCredential(page);
  await page.getByRole('textbox', { name: 'Name', exact: true }).fill(tag('testconn'));
  await page.getByRole('textbox', { name: 'Base Url' }).fill('https://mycompany.aha.io');
  await page.getByLabel('Api Key').fill(PLACEHOLDER_API_KEY);

  const testButton = page.getByRole('button', { name: 'Test connection' });
  await expect(testButton, 'the credential form must offer Test connection for a type with has_test_connection').toBeVisible({
    timeout: 15_000,
  });
  // Read the SERVER'S OWN verdict off the wire — not a DOM-text poll, which
  // races the response (an absent message reads as "count 0" whether the
  // check hasn't answered yet or never will).
  const [checkResponse] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === 'POST' && /\/configurations\/check_connection\//.test(r.url()), {
      timeout: 30_000,
    }),
    testButton.click(),
  ]);
  const body = (await checkResponse.json()) as { reason?: string; success?: boolean };
  expect(
    body.reason,
    'Test connection must not answer "unsupported_type" for a type the descriptor claims to support',
  ).not.toBe('unsupported_type');
});

/**
 * AHA-10b: the credential dropdown offers no search/filter at all — a
 * product gap, not a selector miss. `CredentialsSelect.tsx` is a plain MUI
 * `Select`/`MenuItem` tree with no `TextField` and no filter state anywhere
 * in it (grepped); the legacy case expects typing to narrow the option list.
 */
test('AHA-10b: the Aha Configuration dropdown should support search/filter', async ({ page }) => {
  /* onetest: ELITEA-2534 — product gap: CredentialsSelect renders no
     search/filter input at all, so a saved-credential list of any size is
     un-filterable from the dropdown. */
  test.fail(
    true,
    'ELITEA-2534: product gap — CredentialsSelect.tsx (features/credentials/ui) is a plain ' +
      'MUI Select with no search/filter TextField; a long SAVED AHA CREDENTIALS list cannot be narrowed by typing',
  );
  const name = tag('search_probe');
  await seedAhaCredential(page.request, name, { shared: false });
  await gotoCreateAhaToolkit(page);
  await ahaConfigPicker(page).click();
  const searchBox = page.getByPlaceholder(/search/i);
  await expect(searchBox, 'the dropdown must offer a search/filter field').toBeVisible({ timeout: 5_000 });
});
