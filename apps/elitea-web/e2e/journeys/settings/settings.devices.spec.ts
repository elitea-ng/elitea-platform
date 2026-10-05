/**
 * Journey: Settings › Devices — a native app signs in, the device appears in
 * the user's list, and the user revokes it (ADR-0025 WP3).
 *
 * The device is made the way a real app makes one, not seeded: an admin
 * registers a native client through the registry API, then the member's
 * browser walks the RFC 8252 flow — `/auth/native/authorize` with an S256
 * challenge, the consent page, the redirect to the loopback URI (intercepted
 * here, as the app's local listener would receive it) — and the code is
 * redeemed at `/auth/native/token`, which creates the device row. The page
 * must then list it, and its Revoke must reach the server: a fresh GET of the
 * user's own device list is the proof.
 *
 * Needs the stack's `DEPLOYMENT_URL` (deploy/docker-compose.e2e-standalone.yml):
 * without a public origin /authorize refuses with 503 and no client can be
 * registered.
 */
import { createHash, randomBytes } from 'node:crypto';

import { test, expect, type APIRequestContext } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { API_BASE } from '../../fixtures/api';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

const CLIENTS_URL = `${API_BASE}/admin/native_clients/administration`;
const DEVICES_URL = `${API_BASE}/auth/native/devices`;
const DEVICES_PAGE = `${BASE_URL}/app/settings/devices`;
const REDIRECT_URI = 'http://127.0.0.1/callback';

interface NativeDeviceWire {
  readonly id: string;
  readonly device_name: string;
  readonly client_id: string;
}

const createdClients = new Set<string>();

function base64url(buffer: Buffer): string {
  return buffer.toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

async function listOwnDevices(request: APIRequestContext): Promise<NativeDeviceWire[]> {
  const response = await request.get(DEVICES_URL);
  expect(response.status(), `list own devices: ${await response.text()}`).toBe(200);
  return ((await response.json()) as { devices: NativeDeviceWire[] }).devices;
}

test.afterAll(async ({ browser }) => {
  const context = await browser.newContext({ storageState: STORAGE_STATE.admin });
  try {
    // Removing the client also revokes every device it still has.
    for (const id of createdClients) {
      const response = await context.request.delete(`${CLIENTS_URL}/${encodeURIComponent(id)}`);
      expect([200, 404], `cleanup ${id}: ${response.status()}`).toContain(response.status());
    }
  } finally {
    await context.close();
  }
});

test('Settings › Devices: a signed-in app is listed, and Revoke signs it out', async ({ page, browser }, testInfo) => {
  test.setTimeout(90_000);
  const suffix = `${testInfo.project.name.replace(/[^a-z0-9]/gi, '').toLowerCase()}${Date.now()}`;
  const clientId = `com.elitea.e2edev${suffix}`;
  const deviceName = `E2E device ${suffix}`;

  /* ── an admin registers the app ─────────────────────────────────────── */
  const admin = await browser.newContext({ storageState: STORAGE_STATE.admin });
  try {
    const registered = await admin.request.put(`${CLIENTS_URL}/${encodeURIComponent(clientId)}`, {
      data: { display_name: 'E2E Devices App', redirect_uris: [REDIRECT_URI], enabled: true },
    });
    expect(registered.status(), `register native client: ${await registered.text()}`).toBe(200);
    createdClients.add(clientId);
  } finally {
    await admin.close();
  }

  /* ── the member's browser approves the device ───────────────────────── */
  const verifier = base64url(randomBytes(32));
  const challenge = base64url(createHash('sha256').update(verifier).digest());
  const state = base64url(randomBytes(16));
  // The app's loopback listener. Nothing listens on 127.0.0.1:80 in CI, so
  // the redirect is answered here and its query read from the request.
  await page.route('http://127.0.0.1/**', (route) => route.fulfill({ status: 200, body: 'signed in' }));
  const callback = page.waitForRequest((request) => request.url().startsWith(`${REDIRECT_URI}?`), {
    timeout: 30_000,
  });

  const authorize = new URL(`${API_BASE}/auth/native/authorize`);
  authorize.search = new URLSearchParams({
    response_type: 'code',
    client_id: clientId,
    redirect_uri: REDIRECT_URI,
    state,
    code_challenge: challenge,
    code_challenge_method: 'S256',
    device_name: deviceName,
    platform: 'linux',
    client_version: '1.0.0',
  }).toString();
  await page.goto(authorize.toString());
  await page.getByTestId('native-consent-allow').click({ timeout: 20_000 });

  const answered = new URL((await callback).url());
  expect(answered.searchParams.get('state')).toBe(state);
  const code = answered.searchParams.get('code');
  expect(code, `the consent redirect carried no code: ${answered.toString()}`).toBeTruthy();

  /* ── the app redeems the code, which creates the device ─────────────── */
  const token = await page.request.post(`${API_BASE}/auth/native/token`, {
    form: {
      grant_type: 'authorization_code',
      code: code!,
      redirect_uri: REDIRECT_URI,
      client_id: clientId,
      code_verifier: verifier,
    },
  });
  expect(token.status(), `token exchange: ${await token.text()}`).toBe(200);
  const deviceId = ((await token.json()) as { device_id: string }).device_id;
  expect(deviceId).toBeTruthy();

  /* ── the page lists it ──────────────────────────────────────────────── */
  await page.unroute('http://127.0.0.1/**');
  await page.goto(DEVICES_PAGE);
  const row = page.getByTestId(`native-device-row-${deviceId}`);
  await expect(row).toBeVisible({ timeout: 20_000 });
  await expect(row.getByText(deviceName)).toBeVisible();
  await expect(row.getByText('E2E Devices App')).toBeVisible();
  await expect(row.getByText('Linux')).toBeVisible();
  await checkA11y(page);

  /* ── Revoke confirms, then the server agrees ────────────────────────── */
  await row.getByRole('button', { name: `Revoke ${deviceName}` }).click();
  const dialog = page.getByTestId('native-device-revoke-dialog');
  await expect(dialog).toContainText('wipes its local data');
  const revoked = page.waitForResponse(
    (res) => res.request().method() === 'DELETE' && res.url().includes(`/auth/native/devices/${deviceId}`),
  );
  await dialog.getByTestId('native-device-revoke-confirm').click();
  expect((await revoked).status()).toBe(204);
  await expect(row).toBeHidden({ timeout: 10_000 });
  expect((await listOwnDevices(page.request)).some((device) => device.id === deviceId)).toBe(false);
});
