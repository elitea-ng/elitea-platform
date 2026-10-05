/**
 * Journey: Admin › Configuration › Native clients — register, refuse a bad
 * redirect URI, edit, disable and remove a native client (ADR-0025 WP2).
 *
 * The server shipped the registry routes (`/admin/native_clients/administration`)
 * in v3.3.0 with no UI: the Configuration section only said "registered on
 * their own editor". This journey drives that editor end to end and checks
 * every write against a fresh GET of the same registry, so a control that
 * renders but saves nothing cannot pass.
 *
 * Needs the stack's `DEPLOYMENT_URL` (deploy/docker-compose.e2e-standalone.yml):
 * a save without a public origin is refused 409 `public_origin_required`.
 */
import { test as adminTest, expect, type APIRequestContext, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { API_BASE } from '../../fixtures/api';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

const CLIENTS_URL = `${API_BASE}/admin/native_clients/administration`;

interface NativeClientWire {
  readonly client_id: string;
  readonly display_name: string;
  readonly redirect_uris: readonly string[];
  readonly enabled: boolean;
  readonly min_client_version: string;
  readonly source: string;
}

/** Created ids, removed in afterAll whatever the outcome. */
const createdHere = new Set<string>();

async function listClients(request: APIRequestContext): Promise<NativeClientWire[]> {
  const response = await request.get(CLIENTS_URL);
  expect(response.status(), `list native clients: ${await response.text()}`).toBe(200);
  return ((await response.json()) as { rows: NativeClientWire[] }).rows;
}

async function openNativeClients(page: Page): Promise<void> {
  await page.goto(BASE_URL + '/admin/app/configuration', { waitUntil: 'domcontentloaded' });
  const sections = page.getByRole('navigation', { name: 'Configuration sections' });
  // Anchored: "Native client policy" is a sibling section.
  await sections.getByRole('button', { name: /^Native clients/ }).click({ timeout: 20_000 });
  await expect(page.getByTestId('admin-native-clients-editor')).toBeVisible({ timeout: 15_000 });
}

adminTest.afterAll(async ({ browser }) => {
  const context = await browser.newContext({ storageState: STORAGE_STATE.admin });
  try {
    for (const id of createdHere) {
      const response = await context.request.delete(`${CLIENTS_URL}/${encodeURIComponent(id)}`);
      expect([200, 404], `cleanup ${id}: ${response.status()}`).toContain(response.status());
    }
  } finally {
    await context.close();
  }
});

adminTest('Native clients: register, refuse a bad URI, edit, disable and remove', async ({ page }, testInfo) => {
  adminTest.setTimeout(90_000);
  const suffix = `${testInfo.project.name.replace(/[^a-z0-9]/gi, '').toLowerCase()}${Date.now()}`;
  const clientId = `com.elitea.e2e${suffix}`;
  const scheme = `${clientId}:/oauth/callback`;

  await openNativeClients(page);
  await checkA11y(page);

  /* ── register, first with a redirect URI the server refuses ─────────── */
  await page.getByTestId('admin-native-clients-add').click();
  const dialog = page.getByTestId('native-client-dialog');
  await dialog.getByTestId('native-client-id').fill(clientId);
  await dialog.getByTestId('native-client-display-name').fill('E2E Native');
  await dialog.getByTestId('native-client-redirect-uris').fill(`${scheme}\nhttps://example.com/callback`);
  await dialog.getByTestId('native-client-save').click();

  // The server's reason is placed beside the URI it refused, and nothing was saved.
  await expect(dialog.getByTestId('native-client-redirect-uri-error')).toContainText('https://example.com/callback', {
    timeout: 10_000,
  });
  expect((await listClients(page.request)).some((row) => row.client_id === clientId)).toBe(false);

  await dialog.getByTestId('native-client-redirect-uris').fill(`${scheme}\nhttp://127.0.0.1/callback`);
  const saved = page.waitForResponse(
    (res) => res.request().method() === 'PUT' && res.url().includes('/admin/native_clients/administration/'),
  );
  await dialog.getByTestId('native-client-save').click();
  expect((await saved).status()).toBe(200);
  createdHere.add(clientId);
  await expect(dialog).toBeHidden();

  const row = page.getByTestId(`native-client-row-${clientId}`);
  await expect(row).toBeVisible();
  let persisted = (await listClients(page.request)).find((entry) => entry.client_id === clientId);
  expect(persisted).toMatchObject({
    display_name: 'E2E Native',
    redirect_uris: [scheme, 'http://127.0.0.1/callback'],
    enabled: true,
    source: 'db',
  });

  /* ── edit ───────────────────────────────────────────────────────────── */
  await row.getByRole('button', { name: 'Edit' }).click();
  await expect(dialog.getByTestId('native-client-id')).toBeDisabled();
  await dialog.getByTestId('native-client-display-name').fill('E2E Native Renamed');
  await dialog.getByTestId('native-client-min-version').fill('1.0.0');
  await dialog.getByTestId('native-client-save').click();
  await expect(dialog).toBeHidden({ timeout: 10_000 });
  await expect(row.getByText('E2E Native Renamed')).toBeVisible();
  persisted = (await listClients(page.request)).find((entry) => entry.client_id === clientId);
  expect(persisted).toMatchObject({ display_name: 'E2E Native Renamed', min_client_version: '1.0.0', enabled: true });

  /* ── disable: confirm first, then the server agrees ─────────────────── */
  await row.getByRole('switch', { name: 'Enable E2E Native Renamed' }).click();
  const confirm = page.getByTestId('native-client-confirm-dialog');
  await expect(confirm).toContainText('signs out all 0 of its signed-in devices');
  await confirm.getByTestId('native-client-confirm').click();
  await expect(confirm).toBeHidden({ timeout: 10_000 });
  await expect.poll(async () => (await listClients(page.request)).find((e) => e.client_id === clientId)?.enabled).toBe(
    false,
  );

  /* ── remove ─────────────────────────────────────────────────────────── */
  await row.getByRole('button', { name: 'Remove' }).click();
  await confirm.getByTestId('native-client-confirm').click();
  await expect(row).toBeHidden({ timeout: 10_000 });
  expect((await listClients(page.request)).some((entry) => entry.client_id === clientId)).toBe(false);
  createdHere.delete(clientId);
});
