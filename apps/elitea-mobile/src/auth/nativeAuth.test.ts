import { FIXTURE_ORIGIN, discoveryDocument, discoveryWithoutNativeAuth, tokenResponse } from '@/api/__fixtures__/nativeClient';
import type { WebBrowserAuthSessionResult } from 'expo-web-browser';

import { DiscoveryDocumentSchema } from '@/api/discovery';
import {
  __resetNativeAuthForTests,
  buildAuthorizeUrl,
  parseRedirect,
  signInWithWorkspace,
  verifyRedirect,
} from '@/auth/nativeAuth';
import { __resetNativeSessionForTests } from '@/auth/nativeSession';
import { TokenResponseSchema, type TokenOutcome } from '@/auth/tokenEndpoint';
import { SECURE_KEYS, bindWorkspace } from '@/security/secureStorage';
import { __resetSessionForTests, getBearerToken, getSession, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, acceptDiscovery } from '@/state/workspaceInfo';
import type { FakeSecureStore } from '@/test/fakeSecureStore';

import { TEST_WORKSPACE, wsKey } from '@/test/fakeSecureStore';
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-web-browser', () => ({ openAuthSessionAsync: jest.fn(async () => ({ type: 'cancel' })) }));
const store = jest.requireMock<FakeSecureStore>('expo-secure-store');

const REDIRECT = 'ca.zefir.agent:/oauth/callback';
const CANCELLED = { type: 'cancel' } as WebBrowserAuthSessionResult;
const doc = DiscoveryDocumentSchema.parse(discoveryDocument());
const endpoints = doc.native_auth!;

/** Deterministic PKCE: random bytes are all 1s, digest is a fixed 32-byte value. */
const pkce = {
  randomBytes: (n: number) => new Uint8Array(n).fill(1),
  sha256: async () => new Uint8Array(32).fill(2),
};
const STATE = 'AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE';

function query(url: string): Record<string, string> {
  return Object.fromEntries(
    url
      .slice(url.indexOf('?') + 1)
      .split('&')
      .map((p) => p.split('=').map(decodeURIComponent)),
  );
}

/** A fake system browser: completes with the redirect `respond` builds from the authorize URL. */
function browser(respond: (authorize: Record<string, string>) => string | null) {
  const seen: string[] = [];
  const openAuthSession = jest.fn(async (url: string): Promise<WebBrowserAuthSessionResult> => {
    seen.push(url);
    const redirect = respond(query(url));
    return redirect === null ? CANCELLED : { type: 'success', url: redirect };
  });
  return { openAuthSession, seen };
}

const success = (q: Record<string, string>) =>
  `${REDIRECT}?code=one-time-code&state=${encodeURIComponent(q.state)}&iss=${encodeURIComponent(FIXTURE_ORIGIN)}`;

function exchangeOk() {
  return jest.fn(
    async (): Promise<TokenOutcome> => ({
      kind: 'ok',
      response: TokenResponseSchema.parse(tokenResponse()),
      receivedAt: Date.now(),
    }),
  );
}

beforeEach(() => {
  bindWorkspace(TEST_WORKSPACE);
  store.values.clear();
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  __resetNativeSessionForTests();
  __resetNativeAuthForTests();
  setSession({ hydrated: true, serverOrigin: FIXTURE_ORIGIN });
  acceptDiscovery(FIXTURE_ORIGIN, doc);
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
let afterThis: (() => void) | null = null;
afterEach(() => {
  afterThis?.();
  afterThis = null;
  jest.restoreAllMocks();
});

describe('buildAuthorizeUrl', () => {
  it('carries every parameter the server requires', () => {
    const url = buildAuthorizeUrl({
      endpoints,
      clientId: 'ca.zefir.agent',
      redirectUri: REDIRECT,
      codeChallenge: 'c'.repeat(43),
      state: 's t&ate',
      deviceName: 'Agent Zefir on iPhone',
      platform: 'ios',
      clientVersion: '0.1.0',
    });
    expect(url.startsWith(`${endpoints.authorization_endpoint}?`)).toBe(true);
    expect(query(url)).toEqual({
      response_type: 'code',
      client_id: 'ca.zefir.agent',
      redirect_uri: REDIRECT,
      code_challenge: 'c'.repeat(43),
      code_challenge_method: 'S256',
      state: 's t&ate',
      device_name: 'Agent Zefir on iPhone',
      platform: 'ios',
      client_version: '0.1.0',
    });
  });
});

describe('parseRedirect', () => {
  it('reads the query of our redirect URI (Go form encoding: + is a space)', () => {
    expect(parseRedirect(`${REDIRECT}?code=a%2Fb&state=x&iss=https%3A%2F%2Fai.example.com&error_description=not+allowed`)).toEqual({
      code: 'a/b',
      state: 'x',
      iss: 'https://ai.example.com',
      error_description: 'not allowed',
    });
    expect(parseRedirect('ca.zefir.agent:///oauth/callback?code=a')).toEqual({ code: 'a' });
  });

  it.each([
    'ca.zefir.agent:/oauth/other?code=a',
    'agentzefir://oauth/callback?code=a',
    'https://ai.example.com/oauth/callback?code=a',
    `${REDIRECT}?code=a&code=b`,
    `${REDIRECT}?code=a#frag`,
    `${REDIRECT}?code=%E0%A4%A`,
    REDIRECT,
  ])('refuses %s', (url) => {
    expect(parseRedirect(url)).toBeNull();
  });
});

describe('verifyRedirect', () => {
  const expected = { state: 'abc', issuer: FIXTURE_ORIGIN };

  it('accepts a matching state and issuer', () => {
    expect(verifyRedirect({ code: 'k', state: 'abc', iss: FIXTURE_ORIGIN }, expected)).toEqual({ ok: true, code: 'k' });
  });

  it.each([
    [{ code: 'k', state: 'abd', iss: FIXTURE_ORIGIN }, 'state_mismatch'],
    [{ code: 'k', iss: FIXTURE_ORIGIN }, 'state_mismatch'],
    [{ code: 'k', state: 'abc', iss: 'https://evil.example' }, 'issuer_mismatch'],
    [{ code: 'k', state: 'abc' }, 'issuer_mismatch'],
    [{ error: 'access_denied', state: 'abc', iss: FIXTURE_ORIGIN }, 'denied'],
    [{ error: 'server_error', state: 'abc', iss: FIXTURE_ORIGIN }, 'server_error'],
    [{ state: 'abc', iss: FIXTURE_ORIGIN }, 'server_error'],
  ])('%j → %s', (params, error) => {
    expect(verifyRedirect(params as Record<string, string>, expected)).toEqual({ ok: false, error });
  });
});

describe('signInWithWorkspace', () => {
  it('runs code + PKCE S256 in the system browser and stores the tokens', async () => {
    const { openAuthSession, seen } = browser(success);
    const exchange = exchangeOk();
    const result = await signInWithWorkspace({ openAuthSession, exchange, pkce });
    expect(result).toEqual({ ok: true });

    const authorize = query(seen[0]);
    expect(authorize).toMatchObject({
      response_type: 'code',
      client_id: 'ca.zefir.agent',
      redirect_uri: REDIRECT,
      code_challenge_method: 'S256',
      code_challenge: 'AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI',
      state: STATE,
      platform: 'ios',
    });
    expect(authorize.device_name).toMatch(/^Agent Zefir on /);
    expect(openAuthSession).toHaveBeenCalledWith(seen[0], REDIRECT);
    expect(exchange).toHaveBeenCalledWith(FIXTURE_ORIGIN, endpoints.token_endpoint, {
      code: 'one-time-code',
      codeVerifier: STATE, // same deterministic bytes
      redirectUri: REDIRECT,
      clientId: 'ca.zefir.agent',
    });
    expect(getSession().credential).toEqual({ kind: 'oauth' });
    expect(getBearerToken()).toBe(tokenResponse().access_token);
    expect(store.values.get(wsKey(SECURE_KEYS.credentialKind))).toBe('oauth');
  });

  it('refuses a response whose state does not match, without exchanging', async () => {
    const { openAuthSession } = browser(() => `${REDIRECT}?code=x&state=forged&iss=${encodeURIComponent(FIXTURE_ORIGIN)}`);
    const exchange = exchangeOk();
    expect(await signInWithWorkspace({ openAuthSession, exchange, pkce })).toEqual({ ok: false, error: 'state_mismatch' });
    expect(exchange).not.toHaveBeenCalled();
    expect(getSession().credential).toBeNull();
  });

  it('refuses a response from another issuer', async () => {
    const { openAuthSession } = browser((q) => `${REDIRECT}?code=x&state=${q.state}&iss=https%3A%2F%2Fevil.example`);
    const exchange = exchangeOk();
    expect(await signInWithWorkspace({ openAuthSession, exchange, pkce })).toEqual({ ok: false, error: 'issuer_mismatch' });
    expect(exchange).not.toHaveBeenCalled();
  });

  it('reports a cancelled browser session', async () => {
    const { openAuthSession } = browser(() => null);
    expect(await signInWithWorkspace({ openAuthSession, exchange: exchangeOk(), pkce })).toEqual({
      ok: false,
      error: 'cancelled',
    });
  });

  it('exchanges a code at most once', async () => {
    const { openAuthSession } = browser(success);
    const exchange = jest.fn(async (): Promise<TokenOutcome> => ({ kind: 'network' }));
    expect(await signInWithWorkspace({ openAuthSession, exchange, pkce })).toEqual({ ok: false, error: 'network' });
    expect(await signInWithWorkspace({ openAuthSession, exchange, pkce })).toEqual({ ok: false, error: 'invalid_grant' });
    expect(exchange).toHaveBeenCalledTimes(1);
  });

  // DEF-R6 (Agent Zefir E2E run 2): a fresh install skipped the identity
  // provider and offered consent for the user the system browser was still
  // signed in as. The sheet must be a private session that shares no cookies.
  it('opens the system sign-in sheet as an ephemeral session', async () => {
    const webBrowser = jest.requireMock<{ openAuthSessionAsync: jest.Mock }>('expo-web-browser');
    expect(await signInWithWorkspace({ pkce })).toEqual({ ok: false, error: 'cancelled' });
    expect(webBrowser.openAuthSessionAsync).toHaveBeenCalledTimes(1);
    const [url, redirect, options] = webBrowser.openAuthSessionAsync.mock.calls[0];
    expect(url).toContain('/api/v2/auth/native/authorize');
    expect(redirect).toBe(REDIRECT);
    expect(options).toMatchObject({ preferEphemeralSession: true });
  });

  it('allows one attempt at a time', async () => {
    let finish: () => void = () => {};
    const openAuthSession = jest.fn(
      () => new Promise<WebBrowserAuthSessionResult>((resolve) => (finish = () => resolve(CANCELLED))),
    );
    const first = signInWithWorkspace({ openAuthSession, pkce });
    expect(await signInWithWorkspace({ openAuthSession, pkce })).toEqual({ ok: false, error: 'busy' });
    await Promise.resolve();
    await Promise.resolve();
    finish();
    expect(await first).toEqual({ ok: false, error: 'cancelled' });
  });

  it.each([
    ['invalid_grant', 'invalid_grant'],
    ['invalid_client', 'invalid_client'],
    ['upgrade_required', 'upgrade_required'],
    ['unavailable', 'unavailable'],
  ] as const)('maps a %s token answer', async (kind, error) => {
    const { openAuthSession } = browser(success);
    const exchange = jest.fn(async () => ({ kind }) as TokenOutcome);
    expect(await signInWithWorkspace({ openAuthSession, exchange, pkce })).toEqual({ ok: false, error });
    expect(getSession().credential).toBeNull();
  });

  it('is not available for a workspace without native sign-in', async () => {
    acceptDiscovery(FIXTURE_ORIGIN, DiscoveryDocumentSchema.parse(discoveryWithoutNativeAuth()));
    const { openAuthSession } = browser(success);
    expect(await signInWithWorkspace({ openAuthSession, pkce })).toEqual({ ok: false, error: 'not_available' });
    expect(openAuthSession).not.toHaveBeenCalled();
  });

  it('shows the update screen instead of signing in when the app is below the minimum', async () => {
    acceptDiscovery(FIXTURE_ORIGIN, DiscoveryDocumentSchema.parse(discoveryDocument({ min_client_version: { 'ca.zefir.agent': '9.0.0' } })));
    const { openAuthSession } = browser(success);
    expect(await signInWithWorkspace({ openAuthSession, pkce })).toEqual({ ok: false, error: 'upgrade_required' });
    expect(getSession().upgradeRequired).toEqual({ minVersion: '9.0.0' });
    expect(openAuthSession).not.toHaveBeenCalled();
  });

  it('cleans up and revokes when the tokens cannot be stored', async () => {
    const { openAuthSession } = browser(success);
    store.setItemAsync.mockRejectedValueOnce(new Error('keychain error'));
    const revoke = jest.fn(async () => true);
    jest.spyOn(require('@/auth/tokenEndpoint'), 'revokeToken').mockImplementation(revoke);
    expect(await signInWithWorkspace({ openAuthSession, exchange: exchangeOk(), pkce })).toEqual({
      ok: false,
      error: 'storage',
    });
    expect(getSession().credential).toBeNull();
    expect(store.values.has(wsKey(SECURE_KEYS.refreshToken))).toBe(false);
    expect(revoke).toHaveBeenCalledWith(
      FIXTURE_ORIGIN,
      endpoints.revocation_endpoint,
      { token: tokenResponse().refresh_token, clientId: 'ca.zefir.agent' },
    );
  });

  // WS-SEC-3: the failure cleanup must target the slot the sign-in was for.
  it('cleans up the workspace it signed in to, not one switched to meanwhile', async () => {
    const { openAuthSession } = browser(success);
    const B = 'bbbbbbbbbbbb';
    store.values.set(wsKey(SECURE_KEYS.refreshToken, B), 'b-refresh');
    store.values.set(wsKey(SECURE_KEYS.credentialKind, B), 'oauth');
    store.setItemAsync.mockImplementationOnce(async () => {
      bindWorkspace(B); // the user switched while the keychain write failed
      throw new Error('keychain error');
    });
    jest.spyOn(require('@/auth/tokenEndpoint'), 'revokeToken').mockImplementation(async () => true);
    expect(await signInWithWorkspace({ openAuthSession, exchange: exchangeOk(), pkce })).toEqual({
      ok: false,
      error: 'storage',
    });
    expect(store.values.get(wsKey(SECURE_KEYS.refreshToken, B))).toBe('b-refresh');
    expect(store.values.get(wsKey(SECURE_KEYS.credentialKind, B))).toBe('oauth');
  });

  it('reports a switch during the sign-in as workspace_changed and keeps the stored session', async () => {
    const { openAuthSession } = browser(success);
    setSession({ workspaceId: TEST_WORKSPACE });
    const write = store.setItemAsync.getMockImplementation()!;
    afterThis = () => store.setItemAsync.mockImplementation(write);
    store.setItemAsync.mockImplementation(async (key: string, value: string) => {
      await write(key, value);
      if (key === wsKey(SECURE_KEYS.credentialKind)) {
        bindWorkspace('bbbbbbbbbbbb');
        setSession({ workspaceId: 'bbbbbbbbbbbb' });
      }
    });
    const revoke = jest.fn(async () => true);
    jest.spyOn(require('@/auth/tokenEndpoint'), 'revokeToken').mockImplementation(revoke);
    expect(await signInWithWorkspace({ openAuthSession, exchange: exchangeOk(), pkce })).toEqual({
      ok: false,
      error: 'workspace_changed',
    });
    expect(getBearerToken()).toBeNull();
    expect(store.values.get(wsKey(SECURE_KEYS.credentialKind))).toBe('oauth');
    expect(revoke).not.toHaveBeenCalled();
  });
});
