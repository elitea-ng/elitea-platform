import type { AppStateStatus } from 'react-native';

import { clientPolicy, opaqueToken } from '@/api/__fixtures__/nativeClient';
import { __resetNativeSessionForTests, getNativeTokenSet } from '@/auth/nativeSession';
import { getPolicyState, resetPolicy } from '@/security/policy';
import { resetPolicySources } from '@/security/policyStore';
import { SECURE_KEYS } from '@/security/secureStorage';
import { __resetHydrateForTests, hydrateSession } from '@/state/hydrate';
import { __resetSessionForTests, getBearerToken, getSession } from '@/state/session';
import type { FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('@/security/freshInstall', () => ({
  checkFreshInstall: jest.fn(async () => ({ fresh: false, purgeIncomplete: false })),
}));

const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');
const { checkFreshInstall } = jest.requireMock<{ checkFreshInstall: jest.Mock }>('@/security/freshInstall');

const ORIGIN = 'https://ai.example.com';

function fakeAppState() {
  const listeners = new Set<(s: AppStateStatus) => void>();
  return {
    addEventListener: (_: 'change', l: (s: AppStateStatus) => void) => {
      listeners.add(l);
      return {
        remove: () => {
          listeners.delete(l);
        },
      };
    },
    emit: (s: AppStateStatus) => [...listeners].forEach((l) => l(s)),
    size: () => listeners.size,
  };
}

function stored(values: Partial<Record<keyof typeof SECURE_KEYS, string>>) {
  for (const [name, value] of Object.entries(values)) {
    fake.values.set(SECURE_KEYS[name as keyof typeof SECURE_KEYS], value!);
  }
}

beforeEach(() => {
  __resetSessionForTests();
  __resetHydrateForTests();
  __resetNativeSessionForTests();
  resetPolicy();
  resetPolicySources();
  fake.values.clear();
  jest.clearAllMocks();
  // clearAllMocks keeps implementations: put back the default after a test overrode it.
  fake.deleteItemAsync.mockImplementation(async (key: string) => {
    fake.values.delete(key);
  });
  checkFreshInstall.mockImplementation(async () => ({ fresh: false, purgeIncomplete: false }));
  jest.spyOn(console, 'info').mockImplementation(() => {});
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});

afterEach(() => jest.restoreAllMocks());

describe('hydrateSession', () => {
  it('restores origin, PAT credential and selected project', async () => {
    stored({ serverOrigin: ORIGIN, credentialKind: 'pat', pat: 'tok', projectSelected: '7' });
    await hydrateSession({ devSignIn: true });
    const s = getSession();
    expect(s).toMatchObject({ hydrated: true, serverOrigin: ORIGIN, projectId: 7, credential: { kind: 'pat' } });
    expect(getBearerToken()).toBe('tok');
  });

  it('runs fresh-install check, then the pending-wipe check, then the reads', async () => {
    stored({ serverOrigin: ORIGIN, credentialKind: 'pat', pat: 'tok' });
    await hydrateSession({ devSignIn: true });
    expect(checkFreshInstall.mock.invocationCallOrder[0]).toBeLessThan(
      fake.getItemAsync.mock.invocationCallOrder[0],
    );
    expect(fake.getItemAsync.mock.calls.map((c) => c[0])).toEqual([
      SECURE_KEYS.wipePending,
      SECURE_KEYS.serverOrigin,
      SECURE_KEYS.credentialKind,
      SECURE_KEYS.projectSelected,
      SECURE_KEYS.policy,
      SECURE_KEYS.brandTheme,
      SECURE_KEYS.pat,
    ]);
  });

  it('resumes an interrupted wipe before reading anything else', async () => {
    stored({
      wipePending: JSON.stringify({ reason: 'user_logout' }),
      serverOrigin: ORIGIN,
      credentialKind: 'pat',
      pat: 'tok',
    });
    await hydrateSession({ devSignIn: true });
    expect(fake.values.size).toBe(0);
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: null, credential: null });
    expect(getSession().endReason).toBe('user_logout');
  });

  it('restores nothing while a resumed wipe cannot finish, starts signed out, and wipes again next launch', async () => {
    stored({
      wipePending: JSON.stringify({ reason: 'device_revoked' }),
      serverOrigin: ORIGIN,
      credentialKind: 'pat',
      pat: 'tok',
    });
    const appState = fakeAppState();
    let failPatDelete = true;
    fake.deleteItemAsync.mockImplementation(async (key: string) => {
      if (failPatDelete && key === SECURE_KEYS.pat) throw new Error('keystore error');
      fake.values.delete(key);
    });

    // Launch does not hang on the splash screen, and restores nothing.
    await hydrateSession({ devSignIn: true, appState });
    expect(getBearerToken()).toBeNull();
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: null, credential: null });
    expect(fake.values.has(SECURE_KEYS.wipePending)).toBe(true);
    expect(fake.values.get(SECURE_KEYS.pat)).toBe('tok');

    // Next launch: the delete works now, so the wipe finishes.
    failPatDelete = false;
    __resetHydrateForTests();
    __resetSessionForTests();
    await hydrateSession({ devSignIn: true, appState });
    expect(fake.values.size).toBe(0);
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: null, credential: null });
  });

  it('reads nothing after a fresh install', async () => {
    checkFreshInstall.mockImplementation(async () => ({ fresh: true, purgeIncomplete: false }));
    stored({ serverOrigin: ORIGIN });
    await hydrateSession({ devSignIn: true });
    expect(fake.getItemAsync).not.toHaveBeenCalled();
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: null });
  });

  it('restores a native sign-in and the workspace policy before the lock decision', async () => {
    stored({
      serverOrigin: ORIGIN,
      credentialKind: 'oauth',
      refreshToken: JSON.stringify({
        v: 1,
        origin: ORIGIN,
        clientId: 'ca.zefir.agent',
        tokenEndpoint: `${ORIGIN}/api/v2/auth/native/token`,
        revocationEndpoint: `${ORIGIN}/api/v2/auth/native/revoke`,
        refreshToken: opaqueToken('elnrt_', 1),
        refreshExpiresAt: Date.now() + 1e9,
        deviceId: 'd',
      }),
      accessToken: JSON.stringify({ v: 1, accessToken: opaqueToken('elnat_', 1), accessExpiresAt: Date.now() + 9e5 }),
      policy: JSON.stringify({ v: 1, origin: ORIGIN, policy: clientPolicy({ require_device_lock: false, allow_screenshots: true }) }),
    });
    await hydrateSession();
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: ORIGIN, credential: { kind: 'oauth' } });
    expect(getBearerToken()).toBe(opaqueToken('elnat_', 1));
    expect(getNativeTokenSet()?.refreshToken).toBe(opaqueToken('elnrt_', 1));
    expect(getPolicyState()).toMatchObject({
      source: 'server',
      policy: { require_device_lock: false, allow_screenshots: true, idle_lock_seconds: 120 },
    });
  });

  it('signs in again (staying connected) when the native credential belongs to another origin', async () => {
    stored({
      serverOrigin: ORIGIN,
      credentialKind: 'oauth',
      refreshToken: JSON.stringify({
        v: 1,
        origin: 'https://old.example.com',
        clientId: 'ca.zefir.agent',
        tokenEndpoint: 'https://old.example.com/api/v2/auth/native/token',
        revocationEndpoint: 'https://old.example.com/api/v2/auth/native/revoke',
        refreshToken: opaqueToken('elnrt_', 1),
        refreshExpiresAt: 0,
        deviceId: 'd',
      }),
      policy: JSON.stringify({ v: 1, origin: 'https://old.example.com', policy: clientPolicy({ require_device_lock: false }) }),
    });
    await hydrateSession();
    expect(getSession()).toMatchObject({ serverOrigin: ORIGIN, credential: null });
    expect(fake.values.has(SECURE_KEYS.refreshToken)).toBe(false);
    expect(fake.values.has(SECURE_KEYS.credentialKind)).toBe(false);
    expect(getPolicyState().source).toBe('default'); // another workspace's policy is ignored
  });

  it('stays connected but signed out when this build cannot use the stored PAT', async () => {
    stored({ serverOrigin: ORIGIN, credentialKind: 'pat', pat: 'tok' });
    await hydrateSession({ devSignIn: false });
    expect(getSession()).toMatchObject({ serverOrigin: ORIGIN, credential: null });
    expect(fake.values.has(SECURE_KEYS.pat)).toBe(false);
    expect(fake.values.has(SECURE_KEYS.credentialKind)).toBe(false);
  });

  it('drops a stored origin that is not a normalised https origin', async () => {
    stored({ serverOrigin: 'http://evil.example.com/path', credentialKind: 'pat', pat: 'tok' });
    await hydrateSession({ devSignIn: true, allowPrivateHttp: false });
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: null, credential: null });
    expect(fake.values.has(SECURE_KEYS.serverOrigin)).toBe(false);
  });

  it('ignores a malformed project id', async () => {
    stored({ serverOrigin: ORIGIN, credentialKind: 'pat', pat: 'tok', projectSelected: '-1' });
    await hydrateSession({ devSignIn: true });
    expect(getSession().projectId).toBeNull();
  });

  it('treats a read error as "retry when active", never as signed out', async () => {
    stored({ serverOrigin: ORIGIN, credentialKind: 'pat', pat: 'tok' });
    const appState = fakeAppState();
    fake.getItemAsync.mockImplementationOnce(async () => {
      throw new Error('errSecInteractionNotAllowed');
    });
    const done = hydrateSession({ devSignIn: true, appState });
    await new Promise((r) => setTimeout(r, 0));
    expect(getSession().hydrated).toBe(false);
    expect(fake.values.has(SECURE_KEYS.pat)).toBe(true);
    appState.emit('inactive');
    expect(appState.size()).toBe(1);
    appState.emit('active');
    await done;
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: ORIGIN, credential: { kind: 'pat' } });
  });

  it('runs once', async () => {
    await Promise.all([hydrateSession(), hydrateSession()]);
    expect(checkFreshInstall).toHaveBeenCalledTimes(1);
  });
});
