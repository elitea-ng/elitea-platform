/**
 * §6.2 discipline: no `vi.mock()` of application modules (R-M1 — mocks are
 * sanctioned ONLY for the network boundary (MSW, already used below) and
 * the socket double). `window.ts`'s real `openAuthPopup`/
 * `navigateAuthPopup`/`createAuthorizationMonitor` run for real here; the
 * only stub is the actual browser global `window.open` (a real DOM API,
 * the true I/O boundary a popup-based flow has), exactly like
 * `window.test.ts`'s own technique. Because the OAuth `state` is generated
 * INSIDE `startMcpAuthFlow` (unknown to the test up front), each test
 * captures it by reading the fake popup's `location.href` the flow
 * navigates to (real `buildAuthorizationUrl` output) and parsing out the
 * `state` query param, then resolves the flow with a real `postMessage`
 * `MessageEvent` — the same channel `createAuthorizationMonitor` really
 * listens on.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { MockInstance } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';

import { server } from '../../../test/setup';

import { getAccessToken, getTokenInfo, setAccessToken } from './storage';
import { forgetClientSecret } from './clientSecretVault';
import { triggerProactiveRefresh, refreshAccessToken } from './tokenLifecycle';
import { startMcpAuthFlow } from './oauthFlow';

interface FakePopup {
  closed: boolean;
  location: { href: string };
  document: { body: { style: { cssText: string }; appendChild: (node: unknown) => void }; createElement: (tag: string) => { style: Record<string, unknown>; textContent: string } };
}

/** A fake `window.open()` return value shaped for BOTH `openAuthPopup` (writes a placeholder into `popup.document.body`) and `navigateAuthPopup` (sets `popup.location.href`). */
function makeFakePopup(): FakePopup {
  return {
    closed: false,
    location: { href: '' },
    document: {
      body: { style: { cssText: '' }, appendChild: () => {} },
      createElement: () => ({ style: {}, textContent: '' }),
    },
  };
}

function stubPopup(): { popup: FakePopup; openSpy: MockInstance<typeof window.open> } {
  const popup = makeFakePopup();
  const openSpy: MockInstance<typeof window.open> = vi.spyOn(window, 'open').mockReturnValue(popup as unknown as Window);
  return { popup, openSpy };
}

function stateFromPopupUrl(popup: FakePopup): string {
  const url = new URL(popup.location.href);
  const state = url.searchParams.get('state');
  if (!state) throw new Error(`test setup: popup was never navigated to an authorize URL with a state param (got "${popup.location.href}")`);
  return state;
}

/** Dispatches the SAME shape `pages/mcps`' callback page really sends, on the SAME channel `createAuthorizationMonitor` really listens on (`window` `message` events, spec §6.2: real browser messaging, not a mocked module). */
function deliverAuthResult(result: { code?: string; error?: string; error_description?: string }, state: string): void {
  window.dispatchEvent(
    new MessageEvent('message', {
      origin: window.location.origin,
      data: { type: 'mcp-auth-result', state, ...result },
    }),
  );
}

const globals = globalThis as unknown as Record<string, unknown>;

/** Installs the shipped runtime config, whose `vite_base_uri` ends in a slash. */
function setDeployedConfig(baseUri: string): void {
  globals['elitea_ui_config'] = {
    vite_server_url: '/api/v2',
    vite_base_uri: baseUri,
    vite_public_project_id: '1',
  };
  resetConfigForTests();
}

afterEach(() => {
  window.sessionStorage.clear();
  resetGeneratedClient();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
});

describe('startMcpAuthFlow', () => {
  it('retains a Main client reference through code exchange, document-secret loss, and both refresh paths', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup } = stubPopup();
    const resource = 'https://confidential.example.com/mcp';
    // A previous document grant must not contaminate the new client's identity.
    setAccessToken(resource, 'old', 3600, undefined, undefined, 'old-refresh', { client_secret: 'old-client-secret' });
    const grants: Record<string, unknown>[] = [];
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/7', async ({ request }) => {
        expect(await request.json()).toMatchObject({
          token_endpoint: 'https://as.example.com/token', resource,
        });
        return HttpResponse.json({ client_id: 'registered-client', client_reference: 'opaque-reference' });
      }),
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/7', async ({ request }) => {
        grants.push(await request.json() as Record<string, unknown>);
        return HttpResponse.json({ access_token: `access-${grants.length}`, expires_in: 3600, refresh_token: 'refresh' });
      }),
    );
    const flow = startMcpAuthFlow({
      serverUrl: resource, projectId: 7,
      resourceMetadata: { oauth_authorization_server: {
        authorization_endpoint: 'https://as.example.com/authorize',
        token_endpoint: 'https://as.example.com/token',
        registration_endpoint: 'https://as.example.com/register',
      } },
    });
    await vi.waitFor(() => expect(popup.location.href).toContain('/authorize?'));
    deliverAuthResult({ code: 'code' }, stateFromPopupUrl(popup));
    await flow;
    expect(getTokenInfo(resource)?.client_secret).toBeUndefined();
    expect(window.sessionStorage.getItem('el.mcp.tokens')).not.toContain('old-client-secret');
    forgetClientSecret('token', resource);
    await triggerProactiveRefresh(resource);
    await refreshAccessToken({ serverUrl: resource, projectId: 7, clientId: 'registered-client', tokenEndpoint: 'https://as.example.com/token' });
    expect(grants).toHaveLength(3);
    for (const grant of grants) {
      expect(grant).toMatchObject({ client_reference: 'opaque-reference', client_id: 'registered-client', used_dcr: true, resource });
      expect(grant).not.toHaveProperty('client_secret');
    }
    expect(getTokenInfo(resource)?.client_reference).toBe('opaque-reference');
  });

  it('rejects immediately when neither serverUrl nor a pre-built toolkitType is given', async () => {
    const { openSpy } = stubPopup();
    await expect(startMcpAuthFlow({ resourceMetadata: {} })).rejects.toThrow('Missing MCP server URL');
    expect(openSpy).not.toHaveBeenCalled();
  });

  it('rejects with POPUP_BLOCKED when window.open returns null', async () => {
    vi.spyOn(window, 'open').mockReturnValue(null);
    await expect(startMcpAuthFlow({ serverUrl: 'https://mcp.example.com', resourceMetadata: {} })).rejects.toThrow(/Popup blocked/);
  });

  it('rejects with MISSING_CLIENT_ID when the server has no DCR endpoint and no client_id was supplied', async () => {
    stubPopup();
    await expect(
      startMcpAuthFlow({
        serverUrl: 'https://mcp.example.com',
        resourceMetadata: {
          oauth_authorization_server: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token' },
        },
      }),
    ).rejects.toThrow(/Client ID is required/);
  });

  it('completes the full DCR + PKCE flow: registers a client, navigates the popup, exchanges the code, and persists the token', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup } = stubPopup();

    let dcrBody: unknown;
    let exchangeBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/7', async ({ request }) => {
        dcrBody = await request.json();
        return HttpResponse.json({ client_id: 'dcr-issued-client' });
      }),
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/7', async ({ request }) => {
        exchangeBody = await request.json();
        return HttpResponse.json({ access_token: 'issued-access-token', expires_in: 3600, refresh_token: 'issued-refresh' });
      }),
    );

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://mcp.example.com',
      projectId: 7,
      resourceMetadata: {
        oauth_authorization_server: {
          authorization_endpoint: 'https://as.example.com/authorize',
          token_endpoint: 'https://as.example.com/token',
          registration_endpoint: 'https://as.example.com/register',
        },
      },
    });

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    deliverAuthResult({ code: 'auth-code-1' }, stateFromPopupUrl(popup));

    const result = await flowPromise;

    expect(result.access_token).toBe('issued-access-token');
    expect(dcrBody).toMatchObject({ registration_endpoint: 'https://as.example.com/register' });
    expect(exchangeBody).toMatchObject({ grant_type: 'authorization_code', code: 'auth-code-1', client_id: 'dcr-issued-client' });
    expect(exchangeBody).toHaveProperty('code_verifier'); // PKCE used (no client_secret supplied)
    expect(new URL(popup.location.href).searchParams.get('resource')).toBe('https://mcp.example.com');
    expect(exchangeBody).toHaveProperty('resource', 'https://mcp.example.com');
    expect(getTokenInfo('https://mcp.example.com')?.resource).toBe('https://mcp.example.com');
    expect(getAccessToken('https://mcp.example.com')).toBe('issued-access-token');
  });

  it('uses a caller-supplied client_id/secret when the server has no DCR endpoint, without PKCE', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup } = stubPopup();

    let exchangeBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', async ({ request }) => {
        exchangeBody = await request.json();
        return HttpResponse.json({ access_token: 'manual-flow-token' });
      }),
    );

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://manual.example.com',
      clientId: 'manual-client',
      clientSecret: 'manual-secret',
      resourceMetadata: {
        oauth_authorization_server: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token' },
      },
    });

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    expect(popup.location.href).not.toContain('code_challenge'); // no PKCE: server doesn't advertise S256 AND a client_secret was supplied
    deliverAuthResult({ code: 'auth-code-2' }, stateFromPopupUrl(popup));

    await flowPromise;
    expect(exchangeBody).toMatchObject({ client_id: 'manual-client', client_secret: 'manual-secret' });
    expect(exchangeBody).not.toHaveProperty('code_verifier');
  });

  /*
   * DEFECT: `getRedirectUri()` joined the origin, the configured basename and
   * a leading-slash path literal with no normalization. The shipped basename
   * is `/app/` (`docker-entrypoint.sh` default), so the OAuth `redirect_uri`
   * carried an empty path segment: `https://host/app//mcp-auth-callback`.
   *
   * RFC 6749 3.1.2.3 makes the authorization server compare that URI as a
   * simple string. An operator who registers `https://host/app/mcp-auth-callback`
   * and pastes the `client_id` into the toolkit form therefore got
   * `redirect_uri_mismatch` and the popup never returned a code. The same
   * malformed value went to the token endpoint. This suite never asserted
   * `redirect_uri` at all, which is why the defect survived.
   */
  it('sends a redirect_uri with no doubled slash when the deployment basename ends in one', async () => {
    vi.stubEnv('DEV', false);
    setDeployedConfig('/app/');
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup } = stubPopup();

    let exchangeBody: Record<string, unknown> | undefined;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', async ({ request }) => {
        exchangeBody = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ access_token: 'redirect-uri-token' });
      }),
    );

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://redirect.example.com',
      clientId: 'manual-client',
      resourceMetadata: {
        oauth_authorization_server: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token' },
      },
    });

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    const expected = `${window.location.protocol}//${window.location.host}/app/mcp-auth-callback`;
    expect(new URL(popup.location.href).searchParams.get('redirect_uri')).toBe(expected);

    deliverAuthResult({ code: 'auth-code-redirect' }, stateFromPopupUrl(popup));
    await flowPromise;

    expect(exchangeBody?.['redirect_uri']).toBe(expected);
  });

  it('propagates a "no authorization code" rejection when the popup reports success with no code', async () => {
    const { popup } = stubPopup();

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://no-code.example.com',
      clientId: 'c',
      resourceMetadata: {
        oauth_authorization_server: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token' },
      },
    });
    const assertion = expect(flowPromise).rejects.toThrow('No authorization code received from popup');

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    // Reaching startMcpAuthFlow's OWN "no code" check (as opposed to
    // createAuthorizationMonitor's own onError branch, covered by
    // window.test.ts) needs `waitForAuthorizationResult` to RESOLVE
    // without a `code` — the real success/tokenData-but-no-code shape
    // `handleAuthResult` accepts (baseline: `mcpAuthWindow.helpers.js`'s
    // `data.type === 'mcp-auth-result' && data.success && data.tokenData` branch).
    window.dispatchEvent(
      new MessageEvent('message', {
        origin: window.location.origin,
        data: { type: 'mcp-auth-result', state: stateFromPopupUrl(popup), success: true, tokenData: {} },
      }),
    );

    await assertion;
  });

  it('propagates a rejection when the OAuth provider reports an error (e.g. user cancelled)', async () => {
    const { popup } = stubPopup();

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://cancelled.example.com',
      clientId: 'c',
      resourceMetadata: {
        oauth_authorization_server: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token' },
      },
    });
    const assertion = expect(flowPromise).rejects.toThrow('Authorization cancelled by user');

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    deliverAuthResult({ error: 'Authorization cancelled by user' }, stateFromPopupUrl(popup));

    await assertion;
  });

  it('rejects with the OAuth server\'s error_description when the exchange call fails', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup } = stubPopup();
    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () =>
        HttpResponse.json({ error: 'invalid_grant', error_description: 'authorization code already used' }, { status: 400 }),
      ),
    );

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://exchange-fails.example.com',
      clientId: 'c',
      resourceMetadata: {
        oauth_authorization_server: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token' },
      },
    });
    // Regression coverage: before this fix, a failed exchange's generic
    // "eliteaFetch: 400 from ..." message propagated as-is instead of the
    // OAuth server's own `error_description` — this assertion fails against
    // that prior behaviour.
    const assertion = expect(flowPromise).rejects.toThrow('authorization code already used');

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    deliverAuthResult({ code: 'auth-code-fail' }, stateFromPopupUrl(popup));

    await assertion;
  });

  it('falls back to a generic "Token exchange failed" message when the exchange failure carries no OAuth error body', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup } = stubPopup();
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () => HttpResponse.text('Internal Server Error', { status: 500 })));

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://exchange-fails-no-body.example.com',
      clientId: 'c',
      resourceMetadata: {
        oauth_authorization_server: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token' },
      },
    });
    const assertion = expect(flowPromise).rejects.toThrow('Token exchange failed');

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    deliverAuthResult({ code: 'auth-code-fail-no-body' }, stateFromPopupUrl(popup));

    await assertion;
  });

  it('sends used_dcr to the backend on token exchange when DCR issued the client_id', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup } = stubPopup();

    let exchangeBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/9', () => HttpResponse.json({ client_id: 'dcr-issued-client' })),
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/9', async ({ request }) => {
        exchangeBody = await request.json();
        return HttpResponse.json({ access_token: 'issued-access-token' });
      }),
    );

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://mcp-used-dcr.example.com',
      projectId: 9,
      resourceMetadata: {
        oauth_authorization_server: {
          authorization_endpoint: 'https://as.example.com/authorize',
          token_endpoint: 'https://as.example.com/token',
          registration_endpoint: 'https://as.example.com/register',
        },
      },
    });

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    deliverAuthResult({ code: 'auth-code-used-dcr' }, stateFromPopupUrl(popup));

    await flowPromise;
    // The currently-running legacy pylon backend
    // (`mcp_oauth_proxy.py`: `if not client_secret and not data.used_dcr: ...`)
    // reads this field to decide whether to load a DB-configured
    // `client_secret` for the toolkit — omitting it would make the backend
    // send a secret for what is actually a DCR-registered PUBLIC client,
    // reproducing the "unknown client" rejection bug upstream commit
    // `6ebe8ff7` ("Aha! mcp token issue") fixed.
    expect(exchangeBody).toMatchObject({ used_dcr: true });
    // ...and also persisted LOCALLY — `tokenLifecycle.ts`'s
    // `applyToolkitCredentialFallback` depends on `tokenInfo.used_dcr` to
    // stop a proactive refresh's toolkit-API credential fallback from
    // overwriting this DCR-issued client_id with an unrelated one.
    expect(getTokenInfo('https://mcp-used-dcr.example.com')).toMatchObject({ used_dcr: true });
  });

  it('threads a DCR-issued client_secret through the token exchange and into what gets persisted (upstream commit 6ebe8ff7, "Aha! mcp token issue")', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup } = stubPopup();

    let exchangeBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/9', () => HttpResponse.json({ client_id: 'dcr-issued-client', client_secret: 'dcr-issued-secret' })),
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/9', async ({ request }) => {
        exchangeBody = await request.json();
        return HttpResponse.json({ access_token: 'issued-access-token' });
      }),
    );

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://mcp-dcr-secret.example.com',
      projectId: 9,
      resourceMetadata: {
        oauth_authorization_server: {
          authorization_endpoint: 'https://as.example.com/authorize',
          token_endpoint: 'https://as.example.com/token',
          registration_endpoint: 'https://as.example.com/register',
        },
      },
    });

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    deliverAuthResult({ code: 'auth-code-dcr-secret' }, stateFromPopupUrl(popup));

    await flowPromise;
    expect(exchangeBody).toMatchObject({ client_id: 'dcr-issued-client', client_secret: 'dcr-issued-secret' });
    // Persisted for a later refresh — `tokenLifecycle.ts` reads `tokenInfo.client_secret`.
    expect(getTokenInfo('https://mcp-dcr-secret.example.com')).toMatchObject({ client_id: 'dcr-issued-client', client_secret: 'dcr-issued-secret' });
  });

  it('does not send a client_secret when DCR issues none (PKCE-only public client)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup } = stubPopup();

    let exchangeBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/9', () => HttpResponse.json({ client_id: 'dcr-no-secret-client' })),
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/9', async ({ request }) => {
        exchangeBody = await request.json();
        return HttpResponse.json({ access_token: 'issued-access-token' });
      }),
    );

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://mcp-dcr-no-secret.example.com',
      projectId: 9,
      resourceMetadata: {
        oauth_authorization_server: {
          authorization_endpoint: 'https://as.example.com/authorize',
          token_endpoint: 'https://as.example.com/token',
          registration_endpoint: 'https://as.example.com/register',
        },
      },
    });

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    deliverAuthResult({ code: 'auth-code-dcr-no-secret' }, stateFromPopupUrl(popup));

    await flowPromise;
    expect(exchangeBody).toMatchObject({ client_id: 'dcr-no-secret-client' });
    expect((exchangeBody as { client_secret?: unknown }).client_secret).toBeUndefined();
  });

  it('uses the caller-supplied clientSecret unchanged on the non-DCR (caller-provided client_id) path', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup } = stubPopup();

    let exchangeBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/9', async ({ request }) => {
        exchangeBody = await request.json();
        return HttpResponse.json({ access_token: 'issued-access-token' });
      }),
    );

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://mcp-caller-secret.example.com',
      projectId: 9,
      clientId: 'caller-client',
      clientSecret: 'caller-secret',
      resourceMetadata: {
        oauth_authorization_server: {
          authorization_endpoint: 'https://as.example.com/authorize',
          token_endpoint: 'https://as.example.com/token',
        },
      },
    });

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    deliverAuthResult({ code: 'auth-code-caller-secret' }, stateFromPopupUrl(popup));

    await flowPromise;
    // No DCR endpoint was even advertised — `usedDCR` is false, so the
    // caller-supplied secret must pass through unchanged, and `used_dcr`
    // must NOT be sent (the backend must still be free to fall back to a
    // DB-configured secret for this non-DCR client, per its own gate).
    expect(exchangeBody).toMatchObject({ client_id: 'caller-client', client_secret: 'caller-secret' });
    expect((exchangeBody as { used_dcr?: unknown }).used_dcr).toBeUndefined();
  });

  it('a pre-built MCP (toolkitType) omits its client_id/secret from the exchange UNLESS DCR was used', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup } = stubPopup();

    let exchangeBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', async ({ request }) => {
        exchangeBody = await request.json();
        return HttpResponse.json({ access_token: 'prebuild-token' });
      }),
    );

    const flowPromise = startMcpAuthFlow({
      toolkitType: 'mcp_github',
      clientId: 'preset-client', // no DCR endpoint below -> credentials NOT sent for a pre-built MCP
      clientSecret: 'preset-secret',
      resourceMetadata: {
        oauth_authorization_server: { authorization_endpoint: 'https://github.com/login/oauth/authorize', token_endpoint: 'https://github.com/login/oauth/access_token' },
      },
    });

    await vi.waitFor(() => expect(popup.location.href).toContain('https://github.com/login/oauth/authorize?'), { timeout: 3000 });
    deliverAuthResult({ code: 'prebuild-code' }, stateFromPopupUrl(popup));

    const result = await flowPromise;
    expect(result.access_token).toBe('prebuild-token');
    expect(exchangeBody).not.toHaveProperty('client_id');
    expect(exchangeBody).not.toHaveProperty('client_secret');
    expect(exchangeBody).toMatchObject({ toolkit_type: 'mcp_github' });
    expect(getAccessToken(undefined, 'mcp_github')).toBe('prebuild-token'); // stored under toolkitType, not serverUrl
  });

  it('reuses a caller-provided authWindow instead of opening a new popup', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const { popup, openSpy } = stubPopup();
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () => HttpResponse.json({ access_token: 'reused-token' })));

    const flowPromise = startMcpAuthFlow({
      serverUrl: 'https://reused.example.com',
      clientId: 'c',
      authWindow: popup as unknown as Window,
      resourceMetadata: {
        oauth_authorization_server: { authorization_endpoint: 'https://as.example.com/authorize', token_endpoint: 'https://as.example.com/token' },
      },
    });

    await vi.waitFor(() => expect(popup.location.href).toContain('https://as.example.com/authorize?'), { timeout: 3000 });
    expect(openSpy).not.toHaveBeenCalled(); // the SUPPLIED window was navigated, window.open was never called
    deliverAuthResult({ code: 'reused-window-code' }, stateFromPopupUrl(popup));

    await expect(flowPromise).resolves.toMatchObject({ access_token: 'reused-token' });
  });
});
