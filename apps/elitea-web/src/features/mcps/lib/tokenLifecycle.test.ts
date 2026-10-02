import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../test/setup';

import { getAccessToken, getRefreshToken, getTokenInfo, setAccessToken } from './storage';
import { getValidAccessToken, refreshAccessToken, triggerProactiveRefresh } from './tokenLifecycle';

afterEach(() => {
  window.sessionStorage.clear();
  resetGeneratedClient();
  vi.restoreAllMocks();
});

describe('refreshAccessToken', () => {
  it('throws immediately when no refresh_token is stored', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    await expect(refreshAccessToken({ serverUrl: 'https://never-authed.example.com' })).rejects.toThrow(
      'No refresh token available',
    );
  });

  it('exchanges the refresh_token and persists the new access token under the canonical URL', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setAccessToken('https://Refresh.example.com/', 'old-access', 3600, undefined, undefined, 'refresh-1');

    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () =>
        HttpResponse.json({ access_token: 'new-access', expires_in: 3600, refresh_token: 'new-refresh' }),
      ),
    );

    const result = await refreshAccessToken({ serverUrl: 'https://Refresh.example.com/', tokenEndpoint: 'https://as.example.com/token' });

    expect(result.access_token).toBe('new-access');
    expect(getAccessToken('https://refresh.example.com')).toBe('new-access'); // canonicalised
    expect(getRefreshToken('https://refresh.example.com')).toBe('new-refresh');
  });

  it('keeps the old refresh_token when the response omits a new one', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setAccessToken('https://keep-refresh.example.com', 'old', 3600, undefined, undefined, 'original-refresh');
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () => HttpResponse.json({ access_token: 'new' })));

    await refreshAccessToken({ serverUrl: 'https://keep-refresh.example.com' });
    expect(getRefreshToken('https://keep-refresh.example.com')).toBe('original-refresh');
  });

  it('logs the user out and rethrows when the refresh request itself fails (token may be revoked)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setAccessToken('https://revoked.example.com', 'old', 3600, undefined, undefined, 'bad-refresh');
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () => HttpResponse.json({ error: 'invalid_grant' }, { status: 400 })));

    await expect(refreshAccessToken({ serverUrl: 'https://revoked.example.com' })).rejects.toThrow();
    expect(getAccessToken('https://revoked.example.com')).toBeNull(); // logged out
  });

  it('throws when the response has no access_token', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setAccessToken('https://empty-response.example.com', 'old', 3600, undefined, undefined, 'refresh-x');
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () => HttpResponse.json({})));

    await expect(refreshAccessToken({ serverUrl: 'https://empty-response.example.com' })).rejects.toThrow(
      'No access token received from token refresh',
    );
  });

  it('forwards the stored used_dcr flag on the refresh request (backend needs it on every grant, not just the initial exchange)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setAccessToken(
      'https://refresh-dcr.example.com',
      'old',
      3600,
      undefined,
      undefined,
      'refresh-dcr',
      { client_id: 'dcr-client', used_dcr: true, resource: 'https://protected.example/mcp' },
    );

    let refreshBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', async ({ request }) => {
        refreshBody = await request.json();
        return HttpResponse.json({ access_token: 'refreshed' });
      }),
    );

    await refreshAccessToken({ serverUrl: 'https://refresh-dcr.example.com' });
    expect(refreshBody).toMatchObject({ used_dcr: true, resource: 'https://protected.example/mcp' });
    expect(getTokenInfo('https://refresh-dcr.example.com')?.resource).toBe('https://protected.example/mcp');
  });
});

describe('getValidAccessToken', () => {
  it('returns the current token unchanged when it does not need a refresh', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setAccessToken('https://still-fresh.example.com', 'fresh-token', 3600, undefined, undefined, undefined);
    await expect(getValidAccessToken({ serverUrl: 'https://still-fresh.example.com' })).resolves.toBe('fresh-token');
  });

  it('refreshes and returns the NEW token when due and a tokenEndpoint is provided', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setAccessToken('https://due.example.com', 'stale', -1, undefined, undefined, 'refresh-due');
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () => HttpResponse.json({ access_token: 'refreshed' })));

    await expect(
      getValidAccessToken({ serverUrl: 'https://due.example.com', tokenEndpoint: 'https://as.example.com/token' }),
    ).resolves.toBe('refreshed');
  });

  it('falls back to the existing (possibly stale) token when the refresh attempt itself fails', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setAccessToken('https://refresh-fails.example.com', 'stale-but-present', -1, undefined, undefined, 'refresh-fails');
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () => HttpResponse.json({ error: 'server_error' }, { status: 500 })));

    // refreshAccessToken logs out on failure, so "the existing token" post-failure is null —
    // getValidAccessToken must not throw, and must return whatever getAccessToken now reports.
    await expect(
      getValidAccessToken({ serverUrl: 'https://refresh-fails.example.com', tokenEndpoint: 'https://as.example.com/token' }),
    ).resolves.toBeNull();
  });

  it('returns null with no stored token and nothing due to refresh', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    await expect(getValidAccessToken({ serverUrl: 'https://never-seen.example.com' })).resolves.toBeNull();
  });
});

describe('triggerProactiveRefresh', () => {
  it('is a safe no-op (never throws) when no token or no refresh_token is stored', () => {
    expect(() => triggerProactiveRefresh('https://nothing-stored.example.com')).not.toThrow();
  });

  it('resolves credentials from storage and refreshes, persisting the result', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setAccessToken(
      'https://proactive.example.com',
      'old',
      3600,
      undefined,
      undefined,
      'refresh-proactive',
      { token_endpoint: 'https://as.example.com/token', client_id: 'cid', project_id: '1', toolkit_id: 'tk-1' },
    );

    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () => HttpResponse.json({ access_token: 'refreshed-proactively', expires_in: 3600 })),
    );

    await triggerProactiveRefresh('https://proactive.example.com');
    // The refresh must persist the new access token.
    await vi.waitFor(() =>
      expect(getAccessToken('https://proactive.example.com')).toBe('refreshed-proactively'));
  });

  it('forwards the stored used_dcr flag on a proactive refresh request too', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setAccessToken(
      'https://proactive-dcr.example.com',
      'old',
      3600,
      undefined,
      undefined,
      'refresh-proactive-dcr',
      { token_endpoint: 'https://as.example.com/token', client_id: 'dcr-client', project_id: '1', toolkit_id: 'tk-1', used_dcr: true, resource: 'https://protected.example/mcp' },
    );

    let refreshBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', async ({ request }) => {
        refreshBody = await request.json();
        return HttpResponse.json({ access_token: 'refreshed-proactively', expires_in: 3600 });
      }),
    );

    await triggerProactiveRefresh('https://proactive-dcr.example.com');
    // The proxy receives the original protected resource audience.
    await vi.waitFor(() => expect(refreshBody).toMatchObject({ used_dcr: true, resource: 'https://protected.example/mcp' }));
    expect(getTokenInfo('https://proactive-dcr.example.com')?.resource).toBe('https://protected.example/mcp');
  });

  it('does not log the user out on a failed proactive refresh (best-effort only)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    setAccessToken(
      'https://proactive-fail.example.com',
      'still-here',
      3600,
      undefined,
      undefined,
      'refresh-proactive-fail',
      { token_endpoint: 'https://as.example.com/token', client_id: 'cid', project_id: '1', toolkit_id: 'tk-1' },
    );
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', () => HttpResponse.json({ error: 'nope' }, { status: 400 })));

    await triggerProactiveRefresh('https://proactive-fail.example.com');
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(getAccessToken('https://proactive-fail.example.com')).toBe('still-here');
  });

  it('gives up quietly when no token_endpoint can be resolved from any source', async () => {
    setAccessToken('https://no-endpoint.example.com', 'x', 3600, undefined, undefined, 'refresh-no-endpoint');
    // No token_endpoint anywhere in the stored metadata, no saved credentials, no toolkit_id/project_id
    // to try the toolkit-settings fallback -> should log a debug message and return without calling the network.
    expect(() => triggerProactiveRefresh('https://no-endpoint.example.com')).not.toThrow();
    await new Promise((resolve) => setTimeout(resolve, 0));
  });

  it('never lets the toolkit-API credential fallback overwrite a DCR-issued client_id (used_dcr gate)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    // A pre-built-toolkit MCP that used DCR: client_id/client_secret came
    // from Dynamic Client Registration, but toolkit_id/project_id are ALSO
    // present (the exact combination that, before the used_dcr gate,
    // let applyToolkitCredentialFallback silently substitute a different,
    // unrelated toolkit-DB OAuth client for the refresh_token grant).
    setAccessToken(
      'https://dcr-proactive.example.com',
      'old',
      3600,
      undefined,
      undefined,
      'refresh-dcr',
      {
        token_endpoint: 'https://as.example.com/token',
        client_id: 'dcr-issued-client',
        client_secret: 'dcr-issued-secret',
        project_id: '1',
        toolkit_id: 'tk-dcr',
        used_dcr: true,
      },
    );

    // If the used_dcr gate were missing, this handler's WRONG credentials
    // would win instead of the DCR-issued ones. The mock returns the RAW
    // body only ({settings: {...}}) -- eliteaFetch builds the {data,
    // status, headers} envelope itself; double-wrapping here would make
    // getToolkitOAuthSettings's `envelope.data?.settings` read `undefined`
    // and this test would pass for the wrong reason (a broken mock, not a
    // working used_dcr gate).
    server.use(
      http.get('*/api/v2/elitea_core/tool/prompt_lib/1/tk-dcr', () =>
        HttpResponse.json({ settings: { client_id: 'wrong-toolkit-client', client_secret: 'wrong-toolkit-secret' } }),
      ),
    );

    let capturedBody: Record<string, unknown> | undefined;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', async ({ request }) => {
        capturedBody = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ access_token: 'refreshed-after-dcr', expires_in: 3600 });
      }),
    );

    await triggerProactiveRefresh('https://dcr-proactive.example.com');
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(getAccessToken('https://dcr-proactive.example.com')).toBe('refreshed-after-dcr');
    expect(capturedBody?.client_id).toBe('dcr-issued-client');
    expect(capturedBody?.client_secret).toBe('dcr-issued-secret');
  });
});
