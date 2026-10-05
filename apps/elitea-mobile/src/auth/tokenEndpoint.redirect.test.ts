import { FIXTURE_ORIGIN, tokenResponse } from '@/api/__fixtures__/nativeClient';
import { exchangeAuthorizationCode, refreshTokens, revokeToken } from '@/auth/tokenEndpoint';
import { __resetSessionForTests } from '@/state/session';

// SEC-2: the token, refresh and revoke POSTs carry the refresh token or the
// code + verifier. A natively followed 307/308 would re-send that body to the
// new location before the after-the-fact origin check ran, so these requests
// must go through a transport that refuses redirects.
const mockExpoFetch = jest.fn();
jest.mock('expo/fetch', () => ({ fetch: (...args: unknown[]) => mockExpoFetch(...args) }));
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const TOKEN = `${FIXTURE_ORIGIN}/api/v2/auth/native/token`;
const REVOKE = `${FIXTURE_ORIGIN}/api/v2/auth/native/revoke`;

const realFetch = globalThis.fetch;
beforeEach(() => {
  __resetSessionForTests();
  mockExpoFetch.mockReset();
  mockExpoFetch.mockImplementation(
    async (url: string) =>
      new Response(url === REVOKE ? null : JSON.stringify(tokenResponse(2)), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
  );
  globalThis.fetch = jest.fn(async () => {
    throw new Error('the redirect-following global fetch must not be used');
  }) as typeof fetch;
});
afterEach(() => {
  globalThis.fetch = realFetch;
});

const redirectModes = () => mockExpoFetch.mock.calls.map(([, init]) => (init as RequestInit).redirect);

it('sends refresh, code exchange and revoke with redirects refused', async () => {
  expect(
    await refreshTokens(FIXTURE_ORIGIN, TOKEN, { refreshToken: 'elnrt_aaaaaaaaaaaaaaaa', clientId: 'ca.zefir.agent', clientVersion: '0.1.0' }),
  ).toMatchObject({ kind: 'ok' });
  await exchangeAuthorizationCode(FIXTURE_ORIGIN, TOKEN, {
    code: 'code',
    codeVerifier: 'verifier',
    redirectUri: 'agentzefir://auth',
    clientId: 'ca.zefir.agent',
    clientVersion: '0.1.0',
  } as never);
  await revokeToken(FIXTURE_ORIGIN, REVOKE, { token: 'elnrt_aaaaaaaaaaaaaaaa', clientId: 'ca.zefir.agent' });
  expect(redirectModes()).toEqual(['error', 'error', 'error']);
  const [, init] = mockExpoFetch.mock.calls[0] as [string, RequestInit];
  expect(String(init.body)).toContain('refresh_token=elnrt_aaaaaaaaaaaaaaaa');
  expect(new Headers(init.headers).get('X-Client-Version')).not.toBeNull();
});

it('a redirect fails the request without re-sending the body anywhere', async () => {
  mockExpoFetch.mockImplementation(async () => {
    throw new TypeError('redirect refused');
  });
  expect(
    await refreshTokens(FIXTURE_ORIGIN, TOKEN, { refreshToken: 'elnrt_aaaaaaaaaaaaaaaa', clientId: 'ca.zefir.agent', clientVersion: '0.1.0' }),
  ).toEqual({ kind: 'network' });
  expect(mockExpoFetch).toHaveBeenCalledTimes(1);
});
