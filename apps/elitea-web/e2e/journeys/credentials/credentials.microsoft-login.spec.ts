/**
 * The Microsoft sign-in of the credential form (ADR-0057, epic #2254 story S1d): an
 * Outlook credential is authored from the type chooser, saved by the Login button, signed
 * in through the popup, checked with the signed-in token, signed in again after the check
 * refuses the token, and signed out.
 *
 * ## What is real, and what is mocked
 *
 * REAL: the catalogue (the four Microsoft types are offered, not hidden), the schema-driven
 * form, the credential save, and `POST /microsoft_login:start`, which the real Main answers
 * for the saved credential (PKCE, `state`, the Entra authorization URL, the channel name).
 *
 * MOCKED, at the browser: Microsoft Entra (`login.microsoftonline.com`) and Main's own
 * callback and redeem. Main reaches Entra for the token exchange and the identity read,
 * over a host that is pinned to `login.microsoftonline.com` in code (ADR-0057 decision 10),
 * so no stack in CI can serve it a fake Entra. Those server halves (code exchange, handoff
 * parking, single use, a foreign redeemer refused) are covered against a fixture Entra by
 * the Go tests of `internal/microsoftlogin` (story S1c, #2268). What this journey covers is
 * the client half, which nothing else drives in a browser: the popup, the handoff on
 * postMessage and BroadcastChannel matched on `state`, redeem, the account, the check, and
 * the repeat sign-in.
 *
 * The mocked callback page is the real page's script (`internal/api/v2/microsoftlogin/
 * page.go`): it posts `{type, state, handoff}` to `window.opener`, targeted at the
 * deployment origin, and on the BroadcastChannel, and closes the window.
 */
import { expect, test, type Page, type Route } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID } from '../../fixtures/api';

const RUN_ID = String(Date.now()).slice(-6);
const CREDENTIAL_ROUTE = 'credentials/create-credential';
const TENANT = 'e2e-tenant';
const AUTHORITY = `https://login.microsoftonline.com/${TENANT}`;
const CLIENT_ID = '11111111-2222-4333-8444-555555555555';
const ACCESS_TOKEN = 'e2e-access-token';
const ACCOUNT = 'E2E Tester';

/** The callback page's script, as the server serves it. `__STATE__` is the sign-in's own state. */
function callbackPage(state: string): string {
  return `<!doctype html><html><body><p>You are signed in. You can close this window.</p><script>
(function(){var m={type:'elitea:microsoft-login',state:${JSON.stringify(state)},handoff:'e2e-handoff'};
try{if(window.opener){window.opener.postMessage(m,location.origin)}}catch(e){}
try{var c=new BroadcastChannel('elitea-microsoft-login');c.postMessage(m);c.close()}catch(e){}
try{window.close()}catch(e){}})();
</script></body></html>`;
}

interface SignInTrace {
  /** What the real `start` answered, per sign-in. */
  starts: { credentialId: string; state: string; authorizationUrl: string; channel: string }[];
  /** The access token each connection check carried, `undefined` when it carried none. */
  checks: (string | undefined)[];
  entraVisits: string[];
}

/**
 * Mock Entra and Main's callback and redeem for this page's context. The check answers
 * from `checkMode`: `ok` for a token the journey issued, `refused` with the 401 a refused
 * or expired token gets.
 */
async function mockEntra(page: Page, mode: { check: 'ok' | 'refused' }): Promise<SignInTrace> {
  const trace: SignInTrace = { starts: [], checks: [], entraVisits: [] };
  const context = page.context();

  page.on('response', (response) => {
    if (!response.url().includes('microsoft_login:start') || response.request().method() !== 'POST') return;
    void (async () => {
      const body = (await response.json()) as { state: string; authorization_url: string; channel_name: string };
      const sent = response.request().postDataJSON() as { credential_id: string };
      trace.starts.push({
        credentialId: sent.credential_id,
        state: body.state,
        authorizationUrl: body.authorization_url,
        channel: body.channel_name,
      });
    })();
  });

  // Entra: "the person signed in" and Entra sends the browser to the redirect URI with the state.
  await context.route('https://login.microsoftonline.com/**', async (route: Route) => {
    const url = new URL(route.request().url());
    trace.entraVisits.push(url.toString());
    const state = url.searchParams.get('state') ?? '';
    await route.fulfill({
      status: 302,
      headers: { Location: `${BASE_URL}/api/v2/microsoft_login:callback?state=${encodeURIComponent(state)}&code=e2e-code` },
    });
  });
  await context.route('**/api/v2/microsoft_login:callback*', async (route: Route) => {
    const state = new URL(route.request().url()).searchParams.get('state') ?? '';
    await route.fulfill({ status: 200, contentType: 'text/html', body: callbackPage(state) });
  });
  await context.route('**/api/v2/microsoft_login:redeem', async (route: Route) => {
    const sent = route.request().postDataJSON() as { state: string; handoff: string };
    const start = trace.starts.find((entry) => entry.state === sent.state);
    if (start === undefined || sent.handoff !== 'e2e-handoff') {
      await route.fulfill({ status: 400, contentType: 'application/json', body: JSON.stringify({ error: 'not valid', code: 'handoff_invalid' }) });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        access_token: ACCESS_TOKEN,
        token_type: 'Bearer',
        expires_at: new Date(Date.now() + 3_600_000).toISOString(),
        expires_in: 3600,
        scope: ['Mail.Read'],
        credential_id: start.credentialId,
        credential_type: 'outlook',
        app: 'custom',
        tenant_id: TENANT,
        account_name: ACCOUNT,
      }),
    });
  });
  // The identity read goes to Microsoft from Main, which no stack can fake: answer the check here.
  await context.route('**/api/v2/configurations/check_connection/*/outlook', async (route: Route) => {
    const sent = route.request().postDataJSON() as { access_token?: string };
    trace.checks.push(sent.access_token);
    if (mode.check === 'ok' && sent.access_token === ACCESS_TOKEN) {
      await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ success: true, reason: 'ok', message: 'Connection successful' }) });
      return;
    }
    await route.fulfill({
      status: 401,
      contentType: 'application/json',
      body: JSON.stringify({
        success: false,
        requires_authorization: true,
        message: 'This credential signs in per user. Log in to authorise it.',
        auth_metadata: { server_url: AUTHORITY, resource_metadata: { authorization_servers: [AUTHORITY] } },
      }),
    });
  });
  return trace;
}

const createdCredentialIds: string[] = [];

test.afterAll(async ({ browser }) => {
  const context = await browser.newContext();
  try {
    for (const id of createdCredentialIds) {
      await context.request.delete(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${id}`).catch(() => {});
    }
  } finally {
    await context.close();
  }
});

test('MS-1: the four Microsoft types are offered by the catalogue, not hidden from the create pickers', async ({ page }) => {
  const response = await page.request.get(`${API_BASE}/configurations/available/`);
  expect(response.status(), await response.text()).toBe(200);
  const rows = (await response.json()) as { type?: string; config_schema?: { properties?: { data?: { metadata?: { hidden?: boolean } } } } }[];
  for (const type of ['outlook', 'teams', 'ms_graph', 'dataverse']) {
    const row = rows.find((entry) => entry.type === type);
    expect(row, `the catalogue does not offer ${type}`).toBeDefined();
    expect(row?.config_schema?.properties?.data?.metadata?.hidden, `${type} is still hidden`).not.toBe(true);
  }

  await page.goto(`${BASE_URL}/app/${CREDENTIAL_ROUTE}`, { waitUntil: 'domcontentloaded' });
  const search = page.getByPlaceholder('Search credentials');
  await expect(search).toBeVisible({ timeout: 30_000 });
  await search.fill('Outlook');
  await expect(page.getByRole('button', { name: /^Outlook$/i })).toHaveCount(1);
});

test('MS-2: Login saves the credential, signs in through the popup, shows the account and checks the connection with the token; a refused token asks again; Sign out forgets it', async ({
  page,
}) => {
  test.setTimeout(240_000);
  const mode = { check: 'ok' as 'ok' | 'refused' };
  const trace = await mockEntra(page, mode);

  // ── author the credential from the type chooser ─────────────────────────
  await page.goto(`${BASE_URL}/app/${CREDENTIAL_ROUTE}`, { waitUntil: 'domcontentloaded' });
  const search = page.getByPlaceholder('Search credentials');
  await expect(search).toBeVisible({ timeout: 30_000 });
  await search.fill('Outlook');
  await page.getByRole('button', { name: /^Outlook$/i }).click();
  await expect(page).toHaveURL(new RegExp(`/${CREDENTIAL_ROUTE}/outlook`), { timeout: 20_000 });

  const name = `${AUTOTEST_PREFIX}cred_outlook_${RUN_ID}`;
  await page.getByRole('textbox', { name: 'Name', exact: true }).fill(name);
  // The scopes are tick boxes with the consent note, Mail.Read ticked by default.
  await expect(page.getByRole('checkbox', { name: 'Mail.Read', exact: true })).toBeChecked();
  await expect(page.getByTestId('microsoft-scopes-note')).toContainText('every permission');
  await page.getByLabel('Client Id').fill(CLIENT_ID);
  await page.getByLabel('Client Secret').first().fill('e2e-client-secret');
  await page.getByLabel('Oauth Discovery Endpoint').fill(AUTHORITY);

  // With no sign-in yet, the check does not fail: it says to sign in first, and sends nothing.
  await page.getByTestId('credential-test-connection').click();
  await expect(page.getByTestId('credential-test-signin')).toBeVisible();
  expect(trace.checks).toEqual([]);

  // ── Login: save first, then the popup ───────────────────────────────────
  const created = page.waitForResponse(
    (r) => r.request().method() === 'POST' && r.url().includes('/configurations/configurations/'),
    { timeout: 30_000 },
  );
  const popupOpened = page.waitForEvent('popup', { timeout: 30_000 });
  await page.getByTestId('microsoft-login-button').click();
  const createdResponse = await created;
  expect(createdResponse.ok(), await createdResponse.text()).toBe(true);
  const credential = (await createdResponse.json()) as { id: string | number; uuid?: string };
  createdCredentialIds.push(String(credential.id));
  await popupOpened;

  // A message for another sign-in is ignored: the form keeps waiting for its own.
  await expect(page.getByTestId('microsoft-login-progress')).toBeVisible({ timeout: 30_000 });
  await page.evaluate(() => {
    window.postMessage({ type: 'elitea:microsoft-login', state: 'someone-elses-state', handoff: 'evil-handoff' }, window.location.origin);
  });

  await expect(page.getByText(`Signed in as ${ACCOUNT}`)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(`Microsoft tenant: ${TENANT}`)).toBeVisible();
  await expect(page.getByText(`Runs act as ${ACCOUNT}`)).toBeVisible();

  // The real start answered for the credential the form just saved, on Entra, with the web channel.
  expect(trace.starts).toHaveLength(1);
  const start = trace.starts[0];
  expect(start?.credentialId).toBe(credential.uuid);
  expect(start?.authorizationUrl.startsWith(`${AUTHORITY}/oauth2/v2.0/authorize?`)).toBe(true);
  expect(start?.channel).toBe('elitea-microsoft-login');
  expect(trace.entraVisits).toHaveLength(1);

  // The check that follows the sign-in carries the token and passes.
  await expect(page.getByTestId('credential-test-result')).toHaveText('Connection successful', { timeout: 30_000 });
  expect(trace.checks).toEqual([ACCESS_TOKEN]);

  // The credential is stored under the name the form typed, with the ticked scopes, and no token.
  const stored = await page.request.get(`${API_BASE}/configurations/configuration/${DEFAULT_PROJECT_ID}/${String(credential.id)}`);
  expect(stored.status(), await stored.text()).toBe(200);
  const row = (await stored.json()) as { type?: string; label?: string; data?: Record<string, unknown> };
  expect(row.type).toBe('outlook');
  expect(row.label).toBe(name);
  expect(row.data?.['scopes']).toEqual(['Mail.Read']);
  expect(JSON.stringify(row)).not.toContain(ACCESS_TOKEN);

  // ── the check refuses the token: the form asks for a new sign-in ────────
  mode.check = 'refused';
  await page.getByTestId('credential-test-connection').click();
  await expect(page.getByTestId('microsoft-login-expired')).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId('microsoft-login-button')).toBeVisible();

  mode.check = 'ok';
  const secondPopup = page.waitForEvent('popup', { timeout: 30_000 });
  await page.getByTestId('microsoft-login-button').click();
  await secondPopup;
  await expect(page.getByText(`Signed in as ${ACCOUNT}`)).toBeVisible({ timeout: 60_000 });
  await expect(page.getByTestId('credential-test-result')).toHaveText('Connection successful', { timeout: 30_000 });
  // The second sign-in updated the credential the first one created; it did not create another.
  expect(trace.starts).toHaveLength(2);
  expect(trace.starts[1]?.credentialId).toBe(credential.uuid);

  // ── Sign out ─────────────────────────────────────────────────────────────
  await page.getByTestId('microsoft-login-sign-out').click();
  await expect(page.getByTestId('microsoft-login-button')).toBeVisible();
  await expect(page.getByTestId('credential-test-result')).toHaveCount(0);
});
