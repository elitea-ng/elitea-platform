import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../test/setup';

import { registerDynamicClient } from './registerDynamicClient';
import { productName } from '@/shared/brand';

afterEach(() => {
  resetGeneratedClient();
});

describe('registerDynamicClient', () => {
  it('registers via the DCR proxy and returns the issued client_id', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    let capturedBody: unknown;
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/4', async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ client_id: 'issued-client-id' });
      }),
    );

    const result = await registerDynamicClient('https://as.example.com/register', 'https://app.example.com/mcp-auth-callback', 4);

    expect(result).toEqual({ clientId: 'issued-client-id', clientSecret: undefined });
    expect(capturedBody).toMatchObject({
      registration_endpoint: 'https://as.example.com/register',
      redirect_uris: ['https://app.example.com/mcp-auth-callback'],
      client_name: `${productName()} MCP Client`,
      token_endpoint_auth_method: 'none',
    });
  });

  // Regression test for the exact bug fixed upstream in `frontends/EliteaUI`
  // commit `6ebe8ff7` ("fix: [EL-5697] Aha! mcp token issue") — some
  // authorization servers (e.g. Aha!) issue a `client_secret` in the DCR
  // response even for a `token_endpoint_auth_method: none` request. Before
  // this fix, `registerDynamicClient` returned only the bare `client_id`
  // string, silently dropping that secret; the subsequent token exchange
  // then fails as "unknown client" because the server expects it echoed
  // back.
  it('propagates client_secret when the DCR proxy issues one, instead of silently discarding it', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/8', () => HttpResponse.json({ client_id: 'issued-client-id', client_secret: 'server-issued-secret' })),
    );

    const result = await registerDynamicClient('https://as.example.com/register', 'https://app.example.com/cb', 8);

    expect(result).toEqual({ clientId: 'issued-client-id', clientSecret: 'server-issued-secret' });
  });

  it('defaults projectId to 1 when omitted', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    let capturedUrl = '';
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/1', ({ request }) => {
        capturedUrl = request.url;
        return HttpResponse.json({ client_id: 'cid' });
      }),
    );

    await registerDynamicClient('https://as.example.com/register', 'https://app.example.com/cb', undefined);
    expect(capturedUrl).toContain('/elitea_core/mcp_dcr_proxy/1');
  });

  it('throws when the proxy response has no client_id', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(http.post('*/api/v2/elitea_core/mcp_dcr_proxy/2', () => HttpResponse.json({})));

    await expect(
      registerDynamicClient('https://as.example.com/register', 'https://app.example.com/cb', 2),
    ).rejects.toThrow('Registration response missing client_id');
  });

  it('surfaces the OAuth server\'s error_description when the DCR proxy call fails', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.post('*/api/v2/elitea_core/mcp_dcr_proxy/3', () =>
        HttpResponse.json({ error: 'invalid_redirect_uri', error_description: 'redirect_uri is not allowed for this client' }, { status: 400 }),
      ),
    );

    await expect(registerDynamicClient('https://as.example.com/register', 'https://app.example.com/cb', 3)).rejects.toThrow(
      'Dynamic client registration failed: redirect_uri is not allowed for this client',
    );
  });

  it('falls back to the bare `error` code when the proxy response has no error_description', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(http.post('*/api/v2/elitea_core/mcp_dcr_proxy/5', () => HttpResponse.json({ error: 'invalid_client_metadata' }, { status: 400 })));

    await expect(registerDynamicClient('https://as.example.com/register', 'https://app.example.com/cb', 5)).rejects.toThrow(
      'Dynamic client registration failed: invalid_client_metadata',
    );
  });

  it('falls back to a generic message when the proxy failure carries no OAuth error body', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(http.post('*/api/v2/elitea_core/mcp_dcr_proxy/6', () => HttpResponse.text('Internal Server Error', { status: 500 })));

    await expect(registerDynamicClient('https://as.example.com/register', 'https://app.example.com/cb', 6)).rejects.toThrow(
      'Dynamic client registration failed: Unknown error',
    );
  });
});
