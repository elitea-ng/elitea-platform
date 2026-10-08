/**
 * HTTP core under a registered native transport (ADR-0029 decision 9): bearer
 * auth, absolute URLs, no cookies, one refresh on 401, `device_revoked` wipes,
 * 426 reported. Also pins that WITHOUT a transport nothing changes.
 */
import { afterEach, describe, expect, it, vi, type Mock } from 'vitest';

import { createHttpClient } from './http';
import { setNativeTransport, type NativeTransport } from './nativeTransport';

const BASE = 'https://elitea.example.com/api/v2';

const json = (status: number, body: unknown): Response =>
  new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });

interface Mocks {
  accessToken: Mock<NativeTransport['accessToken']>;
  refresh: Mock<NativeTransport['refresh']>;
  signOut: Mock<NativeTransport['signOut']>;
}

/** Registers a native transport and returns its mocks (held apart so no method is read off the object). */
function transport(options: Partial<Mocks> & { fetch: typeof fetch; onUpgradeRequired?: () => void }): Mocks {
  const { fetch: fetchImpl, onUpgradeRequired, ...given } = options;
  const mocks: Mocks = {
    accessToken: vi.fn<NativeTransport['accessToken']>().mockResolvedValue('tok-1'),
    refresh: vi.fn<NativeTransport['refresh']>().mockResolvedValue('refreshed'),
    signOut: vi.fn<NativeTransport['signOut']>(),
    ...given,
  };
  setNativeTransport({
    headers: { 'X-Client-Version': '1.2.3' },
    fetch: fetchImpl,
    ...(onUpgradeRequired !== undefined ? { onUpgradeRequired } : {}),
    ...mocks,
  });
  return mocks;
}

afterEach(() => {
  setNativeTransport(undefined);
  vi.restoreAllMocks();
});

describe('native transport — request shape', () => {
  it('sends the bearer token and client version, omits cookies, refuses redirects, hits the absolute URL', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(json(200, { ok: true }));
    transport({ fetch: fetchMock });

    const result = await createHttpClient({ baseUrl: BASE }).get('/projects');

    expect(result.ok).toBe(true);
    const [url, init] = fetchMock.mock.calls[0] ?? [];
    expect(url).toBe('https://elitea.example.com/api/v2/projects');
    const headers = new Headers(init?.headers);
    expect(headers.get('Authorization')).toBe('Bearer tok-1');
    expect(headers.get('X-Client-Version')).toBe('1.2.3');
    expect(init?.credentials).toBe('omit');
    expect(init?.redirect).toBe('error');
  });

  it('applies to every client built after registration (the session probe builds its own)', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockImplementation(() => Promise.resolve(json(200, {})));
    transport({ fetch: fetchMock });
    await createHttpClient({ baseUrl: BASE }).get('/a');
    await createHttpClient({ baseUrl: BASE }).get('/b');
    for (const call of fetchMock.mock.calls) {
      expect(new Headers(call[1]?.headers).get('Authorization')).toBe('Bearer tok-1');
    }
  });

  it('without a transport the browser path is unchanged: no bearer, cookie credentials, default redirect', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(json(200, {}));
    vi.stubGlobal('fetch', fetchMock);
    try {
      await createHttpClient({ baseUrl: BASE }).get('/projects');
    } finally {
      vi.unstubAllGlobals();
    }
    const init = fetchMock.mock.calls[0]?.[1];
    expect(new Headers(init?.headers).has('Authorization')).toBe(false);
    expect(init?.credentials).toBe('include');
    expect(init && 'redirect' in init).toBe(false);
  });
});

describe('native transport — 401 handling', () => {
  it('refreshes ONCE, replays with the new token, and succeeds', async () => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(json(401, { error: { code: 'invalid_token' } }))
      .mockResolvedValueOnce(json(200, { ok: true }));
    const accessToken = vi.fn<NativeTransport['accessToken']>().mockResolvedValueOnce('old').mockResolvedValue('new');
    const t = transport({ fetch: fetchMock, accessToken });

    const result = await createHttpClient({ baseUrl: BASE }).post('/chat', { body: { q: 1 } });

    expect(result.ok).toBe(true);
    expect(t.refresh).toHaveBeenCalledTimes(1);
    expect(t.refresh).toHaveBeenCalledWith('old');
    expect(new Headers(fetchMock.mock.calls[1]?.[1]?.headers).get('Authorization')).toBe('Bearer new');
    expect(fetchMock.mock.calls[1]?.[1]?.body).toBe(fetchMock.mock.calls[0]?.[1]?.body);
    expect(t.signOut).not.toHaveBeenCalled();
  });

  it('signs out when the refresh fails', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(json(401, {}));
    const t = transport({ fetch: fetchMock, refresh: vi.fn<NativeTransport['refresh']>().mockResolvedValue('ended') });

    const result = await createHttpClient({ baseUrl: BASE }).get('/x');

    expect(result.ok).toBe(false);
    expect(!result.ok && result.error.kind).toBe('auth');
    expect(t.signOut).toHaveBeenCalledWith('refresh_failed');
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('does NOT sign out when the refresh is merely unavailable (offline): a network failure, session kept', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(json(401, {}));
    const t = transport({ fetch: fetchMock, refresh: vi.fn<NativeTransport['refresh']>().mockResolvedValue('unavailable') });

    const result = await createHttpClient({ baseUrl: BASE }).get('/x');

    expect(!result.ok && result.error.kind).toBe('network');
    expect(t.signOut).not.toHaveBeenCalled();
  });

  it('signs out when the replay is refused again (no second refresh)', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockImplementation(() => Promise.resolve(json(401, {})));
    const t = transport({ fetch: fetchMock });

    const result = await createHttpClient({ baseUrl: BASE }).get('/x');

    expect(result.ok).toBe(false);
    expect(t.refresh).toHaveBeenCalledTimes(1);
    expect(t.signOut).toHaveBeenCalledWith('refresh_failed');
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('device_revoked wipes at once, without a refresh attempt', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(json(401, { error: 'device_revoked' }));
    const t = transport({ fetch: fetchMock });

    const result = await createHttpClient({ baseUrl: BASE }).get('/x');

    expect(result.ok).toBe(false);
    expect(t.refresh).not.toHaveBeenCalled();
    expect(t.signOut).toHaveBeenCalledWith('device_revoked');
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('a resource-authorization 401 is not a session failure', async () => {
    const body = { requires_authorization: true, auth_metadata: {} };
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(json(401, body));
    const t = transport({ fetch: fetchMock });

    const result = await createHttpClient({ baseUrl: BASE }).get('/mcp');

    expect(!result.ok && result.error.kind === 'http' && result.error.body).toEqual(body);
    expect(t.refresh).not.toHaveBeenCalled();
    expect(t.signOut).not.toHaveBeenCalled();
  });

  it('never calls the cookie-plane re-auth popup', async () => {
    const reauthenticate = vi.fn<() => Promise<void>>().mockResolvedValue();
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValueOnce(json(401, {})).mockResolvedValueOnce(json(200, {}));
    transport({ fetch: fetchMock });

    await createHttpClient({ baseUrl: BASE, reauthenticate }).get('/x');

    expect(reauthenticate).not.toHaveBeenCalled();
  });

  it('reports 426 to the host and still returns the failure', async () => {
    const onUpgradeRequired = vi.fn<() => void>();
    transport({ fetch: vi.fn<typeof fetch>().mockResolvedValue(json(426, {})), onUpgradeRequired });

    const result = await createHttpClient({ baseUrl: BASE }).get('/x');

    expect(onUpgradeRequired).toHaveBeenCalledTimes(1);
    expect(!result.ok && result.error.kind === 'http' && result.error.status).toBe(426);
  });
});
