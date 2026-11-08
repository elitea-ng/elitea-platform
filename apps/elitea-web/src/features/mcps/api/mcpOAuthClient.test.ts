/**
 * API-164/165/166 contract coverage — every request shape and semantic this
 * unit's manifest slice requires: exchange/refresh share one URL with
 * different `grant_type`, DCR posts to a distinct path, and null/undefined
 * body fields never reach the wire (baseline: `mcpOAuth.js:10,24,41`).
 */
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../test/setup';

import { exchangeMcpOAuthToken, refreshMcpOAuthToken, registerMcpDynamicClient } from './mcpOAuthClient';
import { productName } from '@/shared/brand';

afterEach(() => {
  resetGeneratedClient();
});

interface CapturedPost {
  url: string;
  body: unknown;
}

function captureBody(sink: CapturedPost[]) {
  return async ({ request }: { request: Request }) => {
    sink.push({ url: request.url, body: await request.json() });
    return HttpResponse.json({ access_token: 'tok-123', expires_in: 3600 });
  };
}

describe('exchangeMcpOAuthToken (API-164)', () => {
  it('POSTs to /elitea_core/mcp_oauth_proxy/{projectId} with grant_type=authorization_code', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const sink: CapturedPost[] = [];
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/42', captureBody(sink)));

    const result = await exchangeMcpOAuthToken({
      projectId: 42,
      token_endpoint: 'https://as.example.com/token',
      code: 'auth-code',
      redirect_uri: 'https://app.example.com/mcp-auth-callback',
      client_id: 'client-1',
    });

    expect(result.access_token).toBe('tok-123');
    expect(sink[0]?.url).toContain('/elitea_core/mcp_oauth_proxy/42');
    expect(sink[0]?.body).toMatchObject({
      grant_type: 'authorization_code',
      code: 'auth-code',
      redirect_uri: 'https://app.example.com/mcp-auth-callback',
      client_id: 'client-1',
    });
  });

  it('never sends a null/undefined field (client_secret omitted here)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const sink: CapturedPost[] = [];
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/1', captureBody(sink)));

    await exchangeMcpOAuthToken({
      projectId: 1,
      code: 'c',
      redirect_uri: 'https://app.example.com/cb',
      client_secret: undefined,
    });

    expect(sink[0]?.body).not.toHaveProperty('client_secret');
  });

  it('propagates a 4xx error body as a rejected EliteaApiError', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.post(
        '*/api/v2/elitea_core/mcp_oauth_proxy/9',
        () => HttpResponse.json({ error: 'invalid_grant', error_description: 'code expired' }, { status: 400 }),
      ),
    );

    await expect(
      exchangeMcpOAuthToken({ projectId: 9, code: 'stale-code', redirect_uri: 'https://app.example.com/cb' }),
    ).rejects.toThrow();
  });
});

describe('refreshMcpOAuthToken (API-165)', () => {
  it('POSTs to the SAME URL as exchange, forcing grant_type=refresh_token regardless of caller intent', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const sink: CapturedPost[] = [];
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/7', captureBody(sink)));

    await refreshMcpOAuthToken({
      projectId: 7,
      refresh_token: 'refresh-abc',
      token_endpoint: 'https://as.example.com/token',
    });

    expect(sink[0]?.url).toContain('/elitea_core/mcp_oauth_proxy/7');
    expect(sink[0]?.body).toMatchObject({ grant_type: 'refresh_token', refresh_token: 'refresh-abc' });
  });

  it('returns the new access_token/refresh_token pair on success', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.post('*/api/v2/elitea_core/mcp_oauth_proxy/3', () =>
        HttpResponse.json({ access_token: 'new-access', refresh_token: 'new-refresh', expires_in: 1800 }),
      ),
    );

    const result = await refreshMcpOAuthToken({ projectId: 3, refresh_token: 'old-refresh' });
    expect(result).toMatchObject({ access_token: 'new-access', refresh_token: 'new-refresh' });
  });

  // Regression test: `RefreshMcpOAuthTokenParams` used to omit `used_dcr`
  // entirely, so a refresh call could never forward it even when a caller
  // wanted to — asymmetric with `ExchangeMcpOAuthTokenParams`, which has
  // always carried it (baseline: `mcpAuthFlow.helpers.js`'s
  // `refreshAccessToken`/`getValidAccessToken`, threaded through by
  // `frontends/EliteaUI` commit `6ebe8ff7`).
  it('forwards used_dcr on the request body when the caller supplies it', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const sink: CapturedPost[] = [];
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/11', captureBody(sink)));

    await refreshMcpOAuthToken({ projectId: 11, refresh_token: 'refresh-abc', used_dcr: true });

    expect(sink[0]?.body).toMatchObject({ grant_type: 'refresh_token', used_dcr: true });
  });
});

describe('registerMcpDynamicClient (API-166)', () => {
  it('POSTs to /elitea_core/mcp_dcr_proxy/{projectId} with the DCR request body', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const sink: CapturedPost[] = [];
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/5', async ({ request }) => {
        sink.push({ url: request.url, body: await request.json() });
        return HttpResponse.json({ client_id: 'dynamically-registered-id' });
      }),
    );

    const result = await registerMcpDynamicClient({
      projectId: 5,
      registration_endpoint: 'https://as.example.com/register',
      redirect_uris: ['https://app.example.com/mcp-auth-callback'],
      client_name: `${productName()} MCP Client`,
      grant_types: ['authorization_code', 'refresh_token'],
      response_types: ['code'],
      token_endpoint_auth_method: 'none',
      application_type: 'web',
    });

    expect(result.client_id).toBe('dynamically-registered-id');
    expect(sink[0]?.url).toContain('/elitea_core/mcp_dcr_proxy/5');
    expect(sink[0]?.body).toMatchObject({ registration_endpoint: 'https://as.example.com/register' });
  });

  it('rejects when the DCR proxy returns an error body', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/6', () =>
        HttpResponse.json({ error: 'registration_not_supported' }, { status: 400 }),
      ),
    );

    await expect(
      registerMcpDynamicClient({
        projectId: 6,
        registration_endpoint: 'https://as.example.com/register',
        redirect_uris: ['https://app.example.com/cb'],
        client_name: `${productName()} MCP Client`,
        grant_types: ['authorization_code'],
        response_types: ['code'],
        token_endpoint_auth_method: 'none',
        application_type: 'web',
      }),
    ).rejects.toThrow();
  });
});
