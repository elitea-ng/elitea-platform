/**
 * Journey: a person whose only administration role is `llm_manager` uses the
 * existing console through its navigation (ADR-0059 R0.16, issue 2591).
 *
 * Until R0.16 the navigation showed Configuration and LLM Governance only to
 * a holder of the legacy `runtime.plugins` and `configuration.governance`, so
 * an LLM manager saw Budgets and Projects and had to type every other URL.
 * Here the person opens `/admin/app/`, reaches Providers and models by
 * clicking, and adds a platform provider, which
 * the server admits on `llm.catalogue.manage`. A typed URL to Users shows
 * the access-denied page for that item.
 *
 * Since R3.2 (ADR-0059 decision 6, issue 2532) the navigation is the six
 * labelled areas of the capability document: the LLM manager's document
 * opens Projects and the seven items of Models and spend, whose gateway
 * pages were tabs of Configuration until R3.5a (issue 2535), so those eight
 * items and their two areas are all it shows.
 *
 * The subject is a seeded row (`scripts/e2e-stack.sh seed`, "administration
 * role fixtures"), one per browser project. The admin persona gives it the
 * role and takes it back; it signs in in a browser context of its own, so no
 * shared session is touched.
 */
import { randomBytes } from 'node:crypto';

import { test as adminTest, expect, type APIRequestContext } from '@playwright/test';

import { API_BASE, AUTOTEST_PREFIX } from '../../fixtures/api';
import { signInThroughOidc } from '../../fixtures/session';
import { confirmDangerousAction } from '../../fixtures/dangerousAction';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

const USERS_URL = `${API_BASE}/admin/auth_users/administration`;
const PROVIDERS_URL = `${API_BASE}/admin/gateway/providers`;
const PROVIDER_NAME = `${AUTOTEST_PREFIX}llm_manager_provider_${randomBytes(4).toString('hex')}`;

function subjectFor(projectName: string): string {
  return projectName === 'chromium'
    ? 'e2e-llm-manager-chromium@autotest.local'
    : 'e2e-llm-manager-webkit@autotest.local';
}

async function userId(request: APIRequestContext, email: string): Promise<number> {
  const response = await request.get(`${USERS_URL}?limit=20&offset=0&search=${encodeURIComponent(email)}`);
  expect(response.status(), await response.text()).toBe(200);
  const row = ((await response.json()) as { rows: { id: number; email: string }[] }).rows.find(
    (entry) => entry.email === email,
  );
  expect(row, `the seed must carry ${email}`).toBeTruthy();
  return row!.id;
}

async function setRoles(request: APIRequestContext, id: number, roleNames: readonly string[]): Promise<void> {
  const response = await request.post(USERS_URL, { data: { action: 'set_admin_role', user_id: id, role_names: roleNames } });
  expect(response.status(), await response.text()).toBe(200);
}

/** Deletes the provider this run created, whatever state the test ended in. */
async function sweepProvider(request: APIRequestContext): Promise<void> {
  const response = await request.get(PROVIDERS_URL);
  if (response.status() !== 200) return;
  const items = ((await response.json()) as { items?: { id: number | string; elitea_title?: string }[] }).items ?? [];
  for (const row of items) {
    if (row.elitea_title === PROVIDER_NAME) await request.delete(`${PROVIDERS_URL}/${String(row.id)}`);
  }
}

adminTest('an LLM manager reaches Providers and models through the navigation and adds a provider', async ({ browser, request }, testInfo) => {
  adminTest.setTimeout(120_000);
  const subject = subjectFor(testInfo.project.name);
  const id = await userId(request, subject);
  await setRoles(request, id, ['llm_manager']);

  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    // The round trip lands on the console itself. Signing in to `/app/` and
    // then navigating to `/admin/app/` fired the second navigation while the
    // main app was still importing its route chunks: WebKit cancelled them,
    // the main app's chunk guard reloaded `/app/` over the console navigation
    // ("Load request cancelled" on `/admin/app/`, then "Importing a module
    // script failed"), and the page never showed the console (CI run
    // 45478984064, both attempts).
    await signInThroughOidc(page, subject, '/admin/app/');

    // The navigation shows the pages the role opens, and no other.
    const nav = page.getByTestId('admin-nav');
    await expect(nav).toBeVisible({ timeout: 30_000 });
    const expected = [
      // Home, for a caller with more than one area (R3.3a, issue 2533).
      'admin-nav-item-home',
      'admin-nav-item-projects',
      'admin-nav-item-providers-models',
      'admin-nav-item-models-pricing',
      'admin-nav-item-tool-prices',
      'admin-nav-item-usage',
      'admin-nav-item-logs',
      'admin-nav-item-llm-governance',
      'admin-nav-item-budgets-alerts',
    ];
    await expect(nav.getByRole('link')).toHaveCount(expected.length);
    const ids = await nav.getByRole('link').evaluateAll((anchors) => anchors.map((anchor) => anchor.getAttribute('data-testid')));
    expect(ids).toEqual(expected);
    for (const area of ['projects', 'models_spend']) {
      await expect(nav.getByTestId(`admin-nav-area-${area}`)).toBeVisible();
    }
    // The index is Home (R3.3a, issue 2533): a card for each of its two areas,
    // and no queue, since the role decides none.
    await expect(page.getByTestId('admin-home')).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId('admin-home-area-models_spend')).toBeVisible();
    await expect(page.getByTestId('admin-home-queues')).toHaveCount(0);

    // Models and spend → Providers and models, by clicking.
    await page.getByTestId('admin-nav-item-providers-models').click();
    await page.waitForURL((url) => url.pathname === '/admin/app/models/providers', { timeout: 20_000 });
    await expect(page.getByTestId('llm-providers-table').or(page.getByTestId('llm-providers-empty'))).toBeVisible({
      timeout: 20_000,
    });

    await page.getByTestId('llm-providers-add').click();
    await page.getByTestId('llm-provider-name').fill(PROVIDER_NAME);
    await page.getByTestId('llm-provider-api_key').fill('sk-autotest-not-a-real-key');
    await page.getByTestId('llm-provider-api_base').fill('http://autotest.invalid/v1');
    const created = page.waitForResponse(
      (response) => response.url().endsWith('/admin/gateway/providers') && response.request().method() === 'POST',
    );
    await page.getByTestId('llm-provider-save').click();
    await confirmDangerousAction(page, PROVIDER_NAME);
    expect((await created).status(), 'the server must admit the create on llm.catalogue.manage').toBe(201);
    await expect(page.getByRole('row').filter({ hasText: PROVIDER_NAME })).toBeVisible({ timeout: 20_000 });

    // A typed URL to an item the role does not open shows that item's
    // access-denied page: the item, the roles that open it, and whom to ask.
    await page.goto(BASE_URL + '/admin/app/users', { waitUntil: 'domcontentloaded' });
    const denied = page.getByTestId('admin-item-denied');
    await expect(denied).toHaveAttribute('data-item-id', 'users', { timeout: 20_000 });
    await expect(denied.getByRole('heading', { level: 1 })).toHaveText('You cannot open Users');
    await expect(page.getByTestId('admin-item-denied-roles')).toContainText('Platform administrator', { timeout: 20_000 });
  } finally {
    await context.close();
    await sweepProvider(request);
    await setRoles(request, id, []);
  }
});
