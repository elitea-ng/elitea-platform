/**
 * HTTP core under a registered native transport (ADR-0029 decision 9): bearer
 * auth, absolute URLs, no cookies, one refresh on 401, `device_revoked` wipes,
 * 426 reported. Also pins that WITHOUT a transport nothing changes.
 */
import { afterEach, describe, expect, it, vi, type Mock } from 'vitest';

import { createHttpClient } from './http';
import { sendRaw, type BrowserFetch } from './httpNative';
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
function transport(options: Partial<Mocks> & { fetch: typeof fetch; onUpgradeRequired?: () => void; origin?: string }): Mocks {
  const { fetch: fetchImpl, onUpgradeRequired, origin, ...given } = options;
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
    ...(origin !== undefined ? { origin } : {}),
    ...mocks,
  });
  return mocks;
}

afterEach(() => {
  setNativeTransport(undefined);
  vi.restoreAllMocks();
});

describe('native transport — request shape', () => {
  it('resolves a page-relative base against the deployment origin, not the page (tauri://) origin', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(json(200, { ok: true }));
    transport({ fetch: fetchMock, origin: 'https://elitea.example.com' });

    await createHttpClient({ baseUrl: '/' }).get('/auth/info');

    expect(fetchMock.mock.calls[0]?.[0]).toBe('https://elitea.example.com/auth/info');
  });

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

  it('a replay refused again after a successful refresh is an auth error, NOT a sign-out', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockImplementation(() => Promise.resolve(json(401, {})));
    const t = transport({ fetch: fetchMock });

    const result = await createHttpClient({ baseUrl: BASE }).get('/x');

    expect(!result.ok && result.error.kind).toBe('auth');
    expect(t.refresh).toHaveBeenCalledTimes(1);
    expect(t.signOut).not.toHaveBeenCalled();
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('a replay refused with device_revoked still signs out', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValueOnce(json(401, {})).mockResolvedValueOnce(json(401, { error: 'device_revoked' }));
    const t = transport({ fetch: fetchMock });

    await createHttpClient({ baseUrl: BASE }).get('/x');

    expect(t.signOut).toHaveBeenCalledWith('device_revoked');
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

describe('native transport — failures before the request is sent', () => {
  it('an access-token failure (offline with an expired token) is a network failure value, not a throw', async () => {
    const fetchMock = vi.fn<typeof fetch>();
    transport({ fetch: fetchMock, accessToken: vi.fn<NativeTransport['accessToken']>().mockRejectedValue('could not reach the deployment') });

    const result = await createHttpClient({ baseUrl: BASE }).get('/x');

    expect(result.ok).toBe(false);
    expect(!result.ok && result.error.kind).toBe('network');
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe('native transport — bearer scope', () => {
  it('does not attach the bearer to a URL outside the deployment origin', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockImplementation(() => Promise.resolve(json(200, {})));
    const t = transport({ fetch: fetchMock });
    setNativeTransport({ ...t, headers: {}, fetch: fetchMock, origin: 'https://elitea.example.com' });

    await createHttpClient({ baseUrl: 'https://elsewhere.example/api/v2' }).get('/x');
    await createHttpClient({ baseUrl: BASE }).get('/x');

    expect(new Headers(fetchMock.mock.calls[0]?.[1]?.headers).has('Authorization')).toBe(false);
    expect(new Headers(fetchMock.mock.calls[1]?.[1]?.headers).get('Authorization')).toBe('Bearer tok-1');
  });
});

describe('native transport — raw-fetch modules (artifacts, downloads)', () => {
  it('without a transport, sends with the browser fetch exactly as given', async () => {
    const browser = vi.fn<BrowserFetch>().mockResolvedValue(json(200, {}));
    const init: RequestInit = { credentials: 'same-origin' };
    await sendRaw(browser, '/api/v2/artifacts/buckets/7', init);
    expect(browser).toHaveBeenCalledWith('/api/v2/artifacts/buckets/7', init);
  });

  it('resolves the relative artifact URL against the deployment, with the bearer and no cookies', async () => {
    // The desktop's Artifacts page said "Failed to load buckets": the raw
    // fetch went to `/api/v2/...` on the bundled page's origin, cookie-only.
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(json(200, { buckets: [] }));
    const browser = vi.fn<BrowserFetch>();
    transport({ fetch: fetchMock, origin: 'https://elitea.example.com' });

    const response = await sendRaw(browser, '/api/v2/artifacts/buckets/7', { credentials: 'same-origin' });

    expect(response.status).toBe(200);
    expect(browser).not.toHaveBeenCalled();
    const [url, init] = fetchMock.mock.calls[0] ?? [];
    expect(url).toBe('https://elitea.example.com/api/v2/artifacts/buckets/7');
    expect(new Headers(init?.headers).get('Authorization')).toBe('Bearer tok-1');
    expect(init?.credentials).toBe('omit');
    expect(init?.redirect).toBe('error');
  });

  it('keeps a multipart body and refreshes once on a 401', async () => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(new Response(null, { status: 401 }))
      .mockResolvedValueOnce(json(201, {}));
    const mocks = transport({ fetch: fetchMock, origin: 'https://elitea.example.com' });
    mocks.accessToken.mockResolvedValueOnce('tok-1').mockResolvedValueOnce('tok-2');
    const body = new FormData();
    body.append('file', new Blob(['x']), 'folder/a.txt');

    const response = await sendRaw(vi.fn<BrowserFetch>(), '/api/v2/artifacts/objects/7/b', { method: 'POST', body });

    expect(response.status).toBe(201);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1]?.[1]?.body).toBe(body);
    expect(new Headers(fetchMock.mock.calls[1]?.[1]?.headers).get('Authorization')).toBe('Bearer tok-2');
  });
});
