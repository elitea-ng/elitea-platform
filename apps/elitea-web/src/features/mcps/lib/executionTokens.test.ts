import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../test/setup';

import { getExecutionTokens } from './executionTokens';
import { publishLogout } from './logoutSync';
import { getRefreshToken, getTokenInfo, logout, setAccessToken, setSavedCredentials } from './storage';
import { refreshAccessToken, triggerProactiveRefresh } from './tokenLifecycle';

const KEY = 'credential-one:https://issuer.example.com';
const OTHER = 'credential-two:https://issuer.example.com';

function store(key = KEY, project = '7', lifetime = -1): void {
  setAccessToken(key, 'old-access', lifetime, 'session', undefined, 'refresh-one', {
    project_id: project, toolkit_id: '27', client_id: 'client', token_endpoint: 'https://issuer.example.com/token',
  });
}

function refreshHandler(response: () => Promise<Response> | Response): void {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/7', response));
}

afterEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  resetGeneratedClient();
});

describe('execution token reuse', () => {
  it('reuses fresh project tokens without refresh credentials or other projects', async () => {
    store(KEY, '7', 3600);
    store(OTHER, '8', 3600);
    expect(await getExecutionTokens('7')).toEqual({ [KEY]: { access_token: 'old-access', session_id: 'session' } });
    expect(await getExecutionTokens(undefined)).toEqual({});
  });

  it('refreshes expired tokens once for concurrent turns and keeps rotated metadata', async () => {
    store();
    let requests = 0;
    refreshHandler(() => {
      requests += 1;
      return HttpResponse.json({ access_token: 'new-access', refresh_token: 'refresh-two', expires_in: 3600 });
    });
    const results = await Promise.all([getExecutionTokens('7'), getExecutionTokens('7'), triggerProactiveRefresh(KEY)]);
    expect(requests).toBe(1);
    expect(results[0]).toEqual({ [KEY]: { access_token: 'new-access', session_id: 'session' } });
    expect(results[1]).toEqual(results[0]);
    expect(getRefreshToken(KEY)).toBe('refresh-two');
    expect(getTokenInfo(KEY)).toMatchObject({ project_id: '7', toolkit_id: '27', client_id: 'client' });
    expect(await getExecutionTokens('7')).toEqual(results[0]);
    expect(requests).toBe(1);
  });

  it('drops expired tokens when refresh fails and does not send a stale access token', async () => {
    store();
    refreshHandler(() => HttpResponse.json({ error: 'invalid_grant' }, { status: 400 }));
    expect(await getExecutionTokens('7')).toEqual({});
  });

  it('keeps separate credentials at one issuer independent', async () => {
    store(KEY, '7', 3600);
    store(OTHER, '7', 3600);
    logout(KEY);
    expect(await getExecutionTokens('7')).toEqual({ [OTHER]: { access_token: 'old-access', session_id: 'session' } });
  });

  it.each(['local logout', 'remote logout', 'new authorization'] as const)(
    'does not overwrite %s with a late refresh', async (action) => {
      store();
      let release!: () => void;
      let started!: () => void;
      const startedPromise = new Promise<void>((resolve) => { started = resolve; });
      const responsePromise = new Promise<void>((resolve) => { release = resolve; });
      refreshHandler(async () => {
        started();
        await responsePromise;
        return HttpResponse.json({ access_token: 'late-access', refresh_token: 'late-refresh', expires_in: 3600 });
      });
      const pending = getExecutionTokens('7');
      await startedPromise;
      if (action === 'local logout') logout(KEY);
      if (action === 'remote logout') publishLogout(KEY);
      if (action === 'new authorization') setAccessToken(KEY, 'new-authorization', 3600, 'new-session', undefined, 'new-refresh');
      release();
      const result = await pending;
      if (action === 'new authorization') {
        expect(result).toEqual({ [KEY]: { access_token: 'new-authorization', session_id: 'new-session' } });
        expect(getRefreshToken(KEY)).toBe('new-refresh');
      } else {
        expect(result).toEqual({});
        expect(getTokenInfo(KEY)).toBeNull();
      }
    },
  );

  it('shares manual and execution refresh grants', async () => {
    store();
    let requests = 0;
    refreshHandler(() => {
      requests += 1;
      return HttpResponse.json({ access_token: 'new-access', expires_in: 3600 });
    });
    await Promise.all([
      refreshAccessToken({ serverUrl: KEY, projectId: '7', tokenEndpoint: 'https://issuer.example.com/token' }),
      getExecutionTokens('7'),
    ]);
    expect(requests).toBe(1);
  });

  it('keeps the DCR-issued client ahead of saved credentials on refresh', async () => {
    store();
    setSavedCredentials({ serverUrl: KEY, clientId: 'unrelated-client', clientSecret: 'unrelated-secret' });
    setAccessToken(KEY, 'old-access', -1, undefined, undefined, 'refresh-one', { used_dcr: true, client_id: 'dcr-client' });
    let captured: unknown;
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(http.post('*/api/v2/elitea_core/mcp_oauth_proxy/7', async ({ request }) => {
      captured = await request.json();
      return HttpResponse.json({ access_token: 'new-access', expires_in: 3600 });
    }));
    await getExecutionTokens('7');
    expect(captured).toMatchObject({ client_id: 'dcr-client', used_dcr: true });
    expect(captured).not.toHaveProperty('client_secret');
  });
});
