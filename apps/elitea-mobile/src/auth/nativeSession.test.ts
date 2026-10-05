import { FIXTURE_ORIGIN, opaqueToken, tokenResponse } from '@/api/__fixtures__/nativeClient';
import { getCredentialRefresher } from '@/auth/credentialRefresh';
import {
  ACCESS_REFRESH_SKEW_MS,
  __resetNativeSessionForTests,
  __setNativeSessionDepsForTests,
  getNativeTokenSet,
  onTokenGrant,
  refreshNativeSession,
  restoreNativeSession,
  startNativeSession,
} from '@/auth/nativeSession';
import { TokenResponseSchema, type TokenOutcome } from '@/auth/tokenEndpoint';
import { endSession } from '@/security/endSession';
import { SECURE_KEYS } from '@/security/secureStorage';
import { __resetSessionForTests, getBearerToken, getSession, setSession } from '@/state/session';
import type { FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
const store = jest.requireMock<FakeSecureStore>('expo-secure-store');

const BASE = {
  origin: FIXTURE_ORIGIN,
  clientId: 'ca.zefir.agent',
  tokenEndpoint: `${FIXTURE_ORIGIN}/api/v2/auth/native/token`,
  revocationEndpoint: `${FIXTURE_ORIGIN}/api/v2/auth/native/revoke`,
};

let clock = 1_000_000;
const ok = (n: number): TokenOutcome => ({
  kind: 'ok',
  response: TokenResponseSchema.parse(tokenResponse(n)),
  receivedAt: clock,
});

function useDeps(refresh: jest.Mock, revoke: jest.Mock = jest.fn(async () => true)) {
  __setNativeSessionDepsForTests({
    now: () => clock,
    sleep: async (ms) => {
      clock += ms;
    },
    refresh,
    revoke,
    clientVersion: () => '0.1.0',
  });
  return { refresh, revoke };
}

async function signedIn(n = 1) {
  setSession({ hydrated: true, serverOrigin: FIXTURE_ORIGIN });
  await startNativeSession(TokenResponseSchema.parse(tokenResponse(n)), clock, BASE);
}

beforeEach(() => {
  clock = 1_000_000;
  store.values.clear();
  store.setItemAsync.mockClear();
  store.setItemAsync.mockImplementation(async (key: string, value: string) => {
    store.values.set(key, value);
  });
  __resetSessionForTests();
  __resetNativeSessionForTests();
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('startNativeSession', () => {
  it('stores refresh, then access, then the credential kind, and activates the access token', async () => {
    useDeps(jest.fn());
    await signedIn();
    expect(store.setItemAsync.mock.calls.map((c) => c[0])).toEqual([
      SECURE_KEYS.refreshToken,
      SECURE_KEYS.accessToken,
      SECURE_KEYS.credentialKind,
    ]);
    expect(JSON.parse(store.values.get(SECURE_KEYS.refreshToken)!)).toMatchObject({
      origin: FIXTURE_ORIGIN,
      refreshToken: opaqueToken('elnrt_', 1),
      refreshExpiresAt: clock + 2592000 * 1000,
      deviceId: tokenResponse().device_id,
    });
    expect(store.values.get(SECURE_KEYS.credentialKind)).toBe('oauth');
    expect(getSession().credential).toEqual({ kind: 'oauth' });
    expect(getBearerToken()).toBe(opaqueToken('elnat_', 1));
    expect(getCredentialRefresher()).not.toBeNull();
  });

  it('tells grant listeners (the policy arrives with every token response)', async () => {
    useDeps(jest.fn(async () => ok(2)));
    const policies: unknown[] = [];
    const off = onTokenGrant((response) => policies.push(response.client_policy));
    await signedIn();
    await refreshNativeSession();
    off();
    expect(policies).toHaveLength(2);
    expect(policies[0]).toMatchObject({ require_device_lock: true, idle_lock_seconds: 120 });
  });
});

describe('ensureFresh', () => {
  it('uses the current access token while it is fresh', async () => {
    const { refresh } = useDeps(jest.fn());
    await signedIn();
    expect(await getCredentialRefresher()!.ensureFresh()).toEqual({ ok: true, token: opaqueToken('elnat_', 1) });
    expect(refresh).not.toHaveBeenCalled();
  });

  it('refreshes shortly before expiry', async () => {
    const { refresh } = useDeps(jest.fn(async () => ok(2)));
    await signedIn();
    clock += 900_000 - ACCESS_REFRESH_SKEW_MS + 1;
    expect(await getCredentialRefresher()!.ensureFresh()).toEqual({ ok: true, token: opaqueToken('elnat_', 2) });
    expect(refresh).toHaveBeenCalledWith(BASE.origin, BASE.tokenEndpoint, {
      refreshToken: opaqueToken('elnrt_', 1),
      clientId: 'ca.zefir.agent',
      clientVersion: '0.1.0',
    });
  });
});

describe('refresh', () => {
  it('is single-flight: a 401 storm causes one refresh', async () => {
    let release: (o: TokenOutcome) => void = () => {};
    const { refresh } = useDeps(jest.fn(() => new Promise<TokenOutcome>((resolve) => (release = resolve))));
    await signedIn();
    const rejected = opaqueToken('elnat_', 1);
    const storm = Array.from({ length: 10 }, () => getCredentialRefresher()!.afterRejection(rejected));
    release(ok(2));
    expect(await Promise.all(storm)).toEqual(Array(10).fill('refreshed'));
    expect(refresh).toHaveBeenCalledTimes(1);
    // A late 401 for the old token does not refresh again.
    expect(await getCredentialRefresher()!.afterRejection(rejected)).toBe('refreshed');
    expect(refresh).toHaveBeenCalledTimes(1);
  });

  it('persists the rotated pair BEFORE the new access token is used', async () => {
    useDeps(jest.fn(async () => ok(2)));
    await signedIn();
    const tokenAtWrite: Record<string, string | null> = {};
    store.setItemAsync.mockImplementation(async (key: string, value: string) => {
      tokenAtWrite[key] = getBearerToken();
      store.values.set(key, value);
    });
    await refreshNativeSession();
    expect(tokenAtWrite[SECURE_KEYS.refreshToken]).toBe(opaqueToken('elnat_', 1));
    expect(tokenAtWrite[SECURE_KEYS.accessToken]).toBe(opaqueToken('elnat_', 1));
    expect(JSON.parse(store.values.get(SECURE_KEYS.refreshToken)!).refreshToken).toBe(opaqueToken('elnrt_', 2));
    expect(getBearerToken()).toBe(opaqueToken('elnat_', 2));
  });

  it('re-presents the SAME refresh token after a lost answer (server re-delivery grace)', async () => {
    const { refresh } = useDeps(
      jest
        .fn()
        .mockResolvedValueOnce({ kind: 'network' })
        .mockImplementationOnce(async () => ok(2)),
    );
    await signedIn();
    expect(await refreshNativeSession()).toBe('refreshed');
    expect(refresh).toHaveBeenCalledTimes(2);
    expect(refresh.mock.calls[1][2].refreshToken).toBe(opaqueToken('elnrt_', 1));
    expect(clock - 1_000_000).toBeLessThan(30_000); // inside the server's default window
    expect(getBearerToken()).toBe(opaqueToken('elnat_', 2));
  });

  it('keeps the current pair and the session when the endpoint stays unreachable', async () => {
    const { refresh } = useDeps(jest.fn(async () => ({ kind: 'network' }) as TokenOutcome));
    await signedIn();
    expect(await refreshNativeSession()).toBe('unavailable');
    expect(refresh.mock.calls.length).toBeLessThanOrEqual(3);
    expect(getSession().credential).toEqual({ kind: 'oauth' });
    expect(getNativeTokenSet()?.refreshToken).toBe(opaqueToken('elnrt_', 1));
    expect(clock - 1_000_000).toBeLessThan(30_000);
  });

  it('wipes on device_revoked without calling the revoke endpoint', async () => {
    const { revoke } = useDeps(jest.fn(async () => ({ kind: 'device_revoked' }) as TokenOutcome));
    await signedIn();
    expect(await refreshNativeSession()).toBe('device_revoked');
    await endSession({ reason: 'device_revoked' }); // joins the wipe already running
    expect(getSession()).toMatchObject({ credential: null, serverOrigin: null, endReason: 'device_revoked' });
    expect(store.values.has(SECURE_KEYS.refreshToken)).toBe(false);
    expect(store.values.has(SECURE_KEYS.accessToken)).toBe(false);
    expect(revoke).not.toHaveBeenCalled();
    expect(getCredentialRefresher()).toBeNull();
  });

  it.each(['invalid_grant', 'invalid_client'] as const)('ends the session as credential_rejected on %s', async (kind) => {
    useDeps(jest.fn(async () => ({ kind }) as TokenOutcome));
    await signedIn();
    expect(await refreshNativeSession()).toBe('rejected');
    await endSession({ reason: 'credential_rejected' });
    expect(getSession()).toMatchObject({ credential: null, endReason: 'credential_rejected' });
  });

  it('still uses a rotated pair it could not store, and stores it later', async () => {
    useDeps(jest.fn(async () => ok(2)));
    await signedIn();
    store.setItemAsync.mockRejectedValue(new Error('keychain busy'));
    expect(await refreshNativeSession()).toBe('refreshed');
    expect(getBearerToken()).toBe(opaqueToken('elnat_', 2));
    store.setItemAsync.mockImplementation(async (key: string, value: string) => {
      store.values.set(key, value);
    });
    await getCredentialRefresher()!.ensureFresh();
    expect(JSON.parse(store.values.get(SECURE_KEYS.refreshToken)!).refreshToken).toBe(opaqueToken('elnrt_', 2));
  });
});

describe('sign out', () => {
  it('revokes the device session with the refresh token, then forgets it', async () => {
    const { revoke } = useDeps(jest.fn());
    await signedIn();
    await endSession({ reason: 'user_logout' });
    expect(revoke).toHaveBeenCalledWith(
      FIXTURE_ORIGIN,
      BASE.revocationEndpoint,
      { token: opaqueToken('elnrt_', 1), clientId: 'ca.zefir.agent' },
      expect.objectContaining({ signal: expect.anything() }),
    );
    expect(getNativeTokenSet()).toBeNull();
    expect(store.values.size).toBe(0);
  });
});

describe('restoreNativeSession', () => {
  it('restores the stored credential for the same origin', async () => {
    useDeps(jest.fn());
    await signedIn();
    __resetNativeSessionForTests();
    __resetSessionForTests();
    setSession({ hydrated: true, serverOrigin: FIXTURE_ORIGIN });
    expect(await restoreNativeSession(FIXTURE_ORIGIN)).toBe(true);
    expect(getBearerToken()).toBe(opaqueToken('elnat_', 1));
    expect(getNativeTokenSet()?.refreshToken).toBe(opaqueToken('elnrt_', 1));
  });

  it('refreshes first when the access record is missing', async () => {
    const { refresh } = useDeps(jest.fn(async () => ok(2)));
    await signedIn();
    store.values.delete(SECURE_KEYS.accessToken);
    __resetNativeSessionForTests();
    useDeps(refresh);
    expect(await restoreNativeSession(FIXTURE_ORIGIN)).toBe(true);
    expect(await getCredentialRefresher()!.ensureFresh()).toEqual({ ok: true, token: opaqueToken('elnat_', 2) });
  });

  it('refuses a credential stored for another origin or with foreign endpoints', async () => {
    useDeps(jest.fn());
    await signedIn();
    expect(await restoreNativeSession('https://other.example.com')).toBe(false);
    const record = JSON.parse(store.values.get(SECURE_KEYS.refreshToken)!);
    store.values.set(SECURE_KEYS.refreshToken, JSON.stringify({ ...record, tokenEndpoint: 'https://evil.example/token' }));
    expect(await restoreNativeSession(FIXTURE_ORIGIN)).toBe(false);
    store.values.set(SECURE_KEYS.refreshToken, 'not json');
    expect(await restoreNativeSession(FIXTURE_ORIGIN)).toBe(false);
  });
});

describe('a wipe during a token write', () => {
  it('leaves no rotated refresh token stored once endSession has finished', async () => {
    useDeps(jest.fn(async () => ok(2)));
    await signedIn(1);
    // The rotated pair's write is slow (keychain): the user wipes meanwhile.
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    let writeStarted!: () => void;
    const writing = new Promise<void>((resolve) => {
      writeStarted = resolve;
    });
    store.setItemAsync.mockImplementation(async (key: string, value: string) => {
      if (key === SECURE_KEYS.refreshToken && value.includes(opaqueToken('elnrt_', 2))) {
        writeStarted();
        await gate;
      }
      store.values.set(key, value);
    });
    const refreshing = refreshNativeSession();
    await writing;
    const wiping = endSession({ reason: 'user_logout' });
    // Give the wipe every chance to run to completion before the write lands.
    for (let i = 0; i < 20; i++) await new Promise<void>((resolve) => setImmediate(resolve));
    release();
    const result = await wiping;
    await refreshing;
    expect(result.storageWiped).toBe(true);
    expect(store.values.get(SECURE_KEYS.refreshToken)).toBeUndefined();
    expect(store.values.get(SECURE_KEYS.accessToken)).toBeUndefined();
    expect(getNativeTokenSet()).toBeNull();
  });

  // F6: the hook waited only for writes pending when it started; the retry's
  // second write landed after the keychain wipe.
  it('does not retry a failed token write once the wipe has begun', async () => {
    useDeps(jest.fn(async () => ok(2)));
    await signedIn(1);
    let fail!: () => void;
    const gate = new Promise<void>((resolve) => {
      fail = resolve;
    });
    let writeStarted!: () => void;
    const writing = new Promise<void>((resolve) => {
      writeStarted = resolve;
    });
    let rotatedWrites = 0;
    store.setItemAsync.mockImplementation(async (key: string, value: string) => {
      if (key === SECURE_KEYS.refreshToken && value.includes(opaqueToken('elnrt_', 2))) {
        rotatedWrites++;
        if (rotatedWrites === 1) {
          writeStarted();
          await gate;
          throw new Error('keychain hiccup');
        }
      }
      store.values.set(key, value);
    });
    const refreshing = refreshNativeSession();
    await writing;
    const wiping = endSession({ reason: 'user_logout' });
    for (let i = 0; i < 20; i++) await new Promise<void>((resolve) => setImmediate(resolve));
    fail();
    const result = await wiping;
    await refreshing;
    for (let i = 0; i < 20; i++) await new Promise<void>((resolve) => setImmediate(resolve));
    expect(result.storageWiped).toBe(true);
    expect(rotatedWrites).toBe(1);
    expect(store.values.get(SECURE_KEYS.refreshToken)).toBeUndefined();
    expect(store.values.get(SECURE_KEYS.accessToken)).toBeUndefined();
  });
});

