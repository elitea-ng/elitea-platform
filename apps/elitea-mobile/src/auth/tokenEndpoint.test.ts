import {
  DEVICE_REVOKED_BODY,
  FIXTURE_ORIGIN,
  oauthError,
  opaqueToken,
  tokenResponse,
  upgradeRequiredBody,
} from '@/api/__fixtures__/nativeClient';
import type { BaseFetch } from '@/api/middleware';
import {
  __formBodyForTests,
  exchangeAuthorizationCode,
  refreshTokens,
  revokeToken,
} from '@/auth/tokenEndpoint';
import { __resetSessionForTests, getSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const TOKEN = `${FIXTURE_ORIGIN}/api/v2/auth/native/token`;
const REVOKE = `${FIXTURE_ORIGIN}/api/v2/auth/native/revoke`;

type Seen = { url: string; method: string; headers: Headers; body: string };

function server(status: number, body?: unknown, headers: Record<string, string> = {}) {
  const seen: Seen[] = [];
  const fetch: jest.MockedFunction<BaseFetch> = jest.fn(async (input: Request, init?: RequestInit) => {
    seen.push({ url: input.url, method: input.method, headers: new Headers(init?.headers), body: await input.text() });
    return new Response(body === undefined ? null : JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json', ...headers },
    });
  });
  return { fetch, seen };
}

function parseForm(body: string): Record<string, string> {
  return Object.fromEntries(body.split('&').map((pair) => pair.split('=').map(decodeURIComponent)));
}

beforeEach(() => {
  __resetSessionForTests();
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('exchangeAuthorizationCode', () => {
  it('posts the code grant as a form, anonymously, with X-Client-Version', async () => {
    const { fetch, seen } = server(200, tokenResponse());
    const outcome = await exchangeAuthorizationCode(
      FIXTURE_ORIGIN,
      TOKEN,
      { code: 'c0de', codeVerifier: 'v'.repeat(43), redirectUri: 'ca.zefir.agent:/oauth/callback', clientId: 'ca.zefir.agent' },
      { fetch, now: () => 1000 },
    );
    expect(outcome).toEqual({ kind: 'ok', response: tokenResponse(), receivedAt: 1000 });
    expect(seen[0].method).toBe('POST');
    expect(seen[0].headers.get('Content-Type')).toBe('application/x-www-form-urlencoded');
    expect(seen[0].headers.get('Authorization')).toBeNull();
    expect(seen[0].headers.get('X-Client-Version')).toMatch(/^\d+\.\d+\.\d+$/);
    expect(parseForm(seen[0].body)).toEqual({
      grant_type: 'authorization_code',
      code: 'c0de',
      redirect_uri: 'ca.zefir.agent:/oauth/callback',
      client_id: 'ca.zefir.agent',
      code_verifier: 'v'.repeat(43),
    });
  });

  it('refuses a token response without the required fields', async () => {
    const { fetch } = server(200, { access_token: 'elnat_short' });
    expect(
      await exchangeAuthorizationCode(FIXTURE_ORIGIN, TOKEN, { code: 'c', codeVerifier: 'v', redirectUri: 'r', clientId: 'x' }, { fetch }),
    ).toEqual({ kind: 'failed', error: 'invalid_response' });
  });
});

describe('refreshTokens: outcome mapping (server handler shapes)', () => {
  const request = { refreshToken: opaqueToken('elnrt_', 1), clientId: 'ca.zefir.agent', clientVersion: '0.1.0' };

  it('sends the refresh grant with client_version', async () => {
    const { fetch, seen } = server(200, tokenResponse(2));
    const outcome = await refreshTokens(FIXTURE_ORIGIN, TOKEN, request, { fetch });
    expect(outcome.kind).toBe('ok');
    expect(parseForm(seen[0].body)).toEqual({
      grant_type: 'refresh_token',
      refresh_token: opaqueToken('elnrt_', 1),
      client_id: 'ca.zefir.agent',
      client_version: '0.1.0',
    });
  });

  it.each([
    ['401 device_revoked', 401, DEVICE_REVOKED_BODY, 'device_revoked'],
    ['401 invalid_client', 401, oauthError('invalid_client'), 'invalid_client'],
    ['404 (no native client registered)', 404, { error: 'not_found' }, 'invalid_client'],
    // Not the token handler answering (an edge or proxy in front of it): no
    // statement about this client, so nothing is wiped on it.
    ['404 without the handler body', 404, undefined, 'failed'],
    ['404 with another body', 404, { message: 'Not Found' }, 'failed'],
    ['401 without an OAuth error', 401, undefined, 'failed'],
    ['400 invalid_grant', 400, oauthError('invalid_grant'), 'invalid_grant'],
    ['400 invalid_request', 400, oauthError('invalid_request'), 'failed'],
    ['429', 429, oauthError('invalid_request'), 'unavailable'],
    ['503 temporarily_unavailable', 503, oauthError('temporarily_unavailable'), 'unavailable'],
  ])('%s', async (_label, status, body, kind) => {
    const { fetch } = server(status, body, { 'Retry-After': '5' });
    const outcome = await refreshTokens(FIXTURE_ORIGIN, TOKEN, request, { fetch });
    expect(outcome.kind).toBe(kind);
    if (outcome.kind === 'unavailable') expect(outcome.retryAfterMs).toBe(5000);
  });

  it('maps 426 to upgrade_required and raises the update screen', async () => {
    const { fetch } = server(426, upgradeRequiredBody('0.2.0'), { 'X-Min-Client-Version': '0.2.0' });
    expect(await refreshTokens(FIXTURE_ORIGIN, TOKEN, request, { fetch })).toEqual({ kind: 'upgrade_required' });
    expect(getSession().upgradeRequired).toEqual({ minVersion: '0.2.0' });
  });

  it('maps a transport failure to network', async () => {
    const fetch: BaseFetch = async () => {
      throw new TypeError('Network request failed');
    };
    expect(await refreshTokens(FIXTURE_ORIGIN, TOKEN, request, { fetch })).toEqual({ kind: 'network' });
  });

  it('never posts to an endpoint on another origin', async () => {
    const { fetch } = server(200, tokenResponse());
    expect(await refreshTokens(FIXTURE_ORIGIN, 'https://evil.example/token', request, { fetch })).toEqual({
      kind: 'failed',
      error: 'origin_mismatch',
    });
    expect(fetch).not.toHaveBeenCalled();
  });
});

describe('revokeToken', () => {
  it('posts token, hint and client id and reports 200', async () => {
    const { fetch, seen } = server(200);
    expect(await revokeToken(FIXTURE_ORIGIN, REVOKE, { token: 'elnrt_refresh1', clientId: 'ca.zefir.agent' }, { fetch })).toBe(true);
    expect(seen[0].headers.get('Authorization')).toBeNull();
    expect(parseForm(seen[0].body)).toEqual({
      token: 'elnrt_refresh1',
      token_type_hint: 'refresh_token',
      client_id: 'ca.zefir.agent',
    });
  });

  it('never throws', async () => {
    const fetch: BaseFetch = async () => {
      throw new TypeError('offline');
    };
    expect(await revokeToken(FIXTURE_ORIGIN, REVOKE, { token: 't', clientId: 'c' }, { fetch })).toBe(false);
  });
});

it('form-encodes reserved characters', () => {
  expect(__formBodyForTests({ redirect_uri: 'ca.zefir.agent:/oauth/callback', x: 'a b&c=d' })).toBe(
    'redirect_uri=ca.zefir.agent%3A%2Foauth%2Fcallback&x=a%20b%26c%3Dd',
  );
});

describe('refreshTokens: re-delivery', () => {
  it('accepts a re-delivered pair whose access token has under a second left (expires_in 0)', async () => {
    // Store.Refresh's grace path answers the remaining lifetime of the pair it
    // minted, truncated to seconds and floored at 0. Refusing it would keep
    // the consumed token, and the next try (outside the window) is reuse.
    const { fetch } = server(200, tokenResponse(2, { expires_in: 0 }));
    const outcome = await refreshTokens(
      FIXTURE_ORIGIN,
      TOKEN,
      { refreshToken: opaqueToken('elnrt_', 1), clientId: 'ca.zefir.agent', clientVersion: '0.1.0' },
      { fetch, now: () => 5000 },
    );
    expect(outcome).toMatchObject({ kind: 'ok', receivedAt: 5000 });
    expect(outcome.kind === 'ok' && outcome.response.refresh_token).toBe(opaqueToken('elnrt_', 2));
  });
});
