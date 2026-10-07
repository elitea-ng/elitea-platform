import type { AppStateStatus } from 'react-native';

import { clientPolicy, opaqueToken } from '@/api/__fixtures__/nativeClient';
import { __resetNativeSessionForTests, getNativeTokenSet } from '@/auth/nativeSession';
import { getPolicyState, resetPolicy } from '@/security/policy';
import { resetPolicySources } from '@/security/policyStore';
import { DEVICE_KEYS, SECURE_KEYS, bindWorkspace, boundWorkspace } from '@/security/secureStorage';
import { MIGRATED_WORKSPACE_ID, __resetRegistryForTests } from '@/security/workspaceRegistry';
import * as QuickActions from 'expo-quick-actions';
import * as AppIntentsMock from 'expo-app-intents';

import { __resetAgentCatalogForTests } from '@/features/intents/agentCatalog';
import { __setWidgetSinkForTests } from '@/features/surfaces/surfaceWriter';

import { __resetHydrateForTests, hydrateSession } from '@/state/hydrate';
import { __resetSessionForTests, getBearerToken, getSession } from '@/state/session';
import { TEST_WORKSPACE, registryValue, wsKey, type FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('@/security/freshInstall', () => ({
  checkFreshInstall: jest.fn(async () => ({ fresh: false, purgeIncomplete: false })),
}));

const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');
const { checkFreshInstall } = jest.requireMock<{ checkFreshInstall: jest.Mock }>('@/security/freshInstall');

const ORIGIN = 'https://ai.example.com';
const intents = AppIntentsMock as unknown as { catalogs: Map<string, unknown[]> };

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

type Items = Partial<Record<keyof typeof SECURE_KEYS, string>>;

/** A workspace slot (the active one unless `active` says otherwise), as this app version stores it. */
function stored(values: Items, workspaceId = TEST_WORKSPACE, ids: string[] = [workspaceId], active: string | null = workspaceId) {
  fake.values.set(DEVICE_KEYS.workspaces, registryValue(ids, active));
  for (const [name, value] of Object.entries(values)) {
    fake.values.set(wsKey(SECURE_KEYS[name as keyof typeof SECURE_KEYS], workspaceId), value!);
  }
}

/** The single-workspace layout of the previous app version (every item under its logical name). */
function legacy(values: Items) {
  for (const [name, value] of Object.entries(values)) {
    fake.values.set(SECURE_KEYS[name as keyof typeof SECURE_KEYS], value!);
  }
}

const has = (name: keyof typeof SECURE_KEYS, workspaceId = TEST_WORKSPACE) =>
  fake.values.has(wsKey(SECURE_KEYS[name], workspaceId));
/** Keys left of workspace `id`. */
const left = (id = TEST_WORKSPACE) => [...fake.values.keys()].filter((k) => k.startsWith(`az.ws.${id}.`));

beforeEach(() => {
  __resetSessionForTests();
  __resetRegistryForTests();
  bindWorkspace(null);
  __resetHydrateForTests();
  __resetNativeSessionForTests();
  resetPolicy();
  resetPolicySources();
  __setWidgetSinkForTests(null);
  __resetAgentCatalogForTests();
  intents.catalogs.clear();
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
    const reads = fake.getItemAsync.mock.calls.map((c) => c[0]);
    // The registry (with the upgrade check) comes first, then the workspace's own items.
    expect(reads[0]).toBe(DEVICE_KEYS.workspaces);
    expect(reads.filter((k) => k.startsWith('az.ws.'))).toEqual([
      wsKey(SECURE_KEYS.wipePending),
      wsKey(SECURE_KEYS.serverOrigin),
      wsKey(SECURE_KEYS.credentialKind),
      wsKey(SECURE_KEYS.projectSelected),
      wsKey(SECURE_KEYS.policy),
      wsKey(SECURE_KEYS.brandTheme),
      wsKey(SECURE_KEYS.selfUser),
      wsKey(SECURE_KEYS.pat),
    ]);
    expect(boundWorkspace()).toBe(TEST_WORKSPACE);
  });

  it('resumes an interrupted wipe before reading anything else', async () => {
    stored({
      wipePending: JSON.stringify({ reason: 'user_logout' }),
      serverOrigin: ORIGIN,
      credentialKind: 'pat',
      pat: 'tok',
    });
    await hydrateSession({ devSignIn: true });
    expect(left()).toEqual([]);
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: null, credential: null });
    expect(getSession().endReason).toBe('user_logout');
    expect(JSON.parse(fake.values.get(DEVICE_KEYS.workspaces)!)).toEqual({ v: 1, active: null, ids: [] });
  });

  // QA-SEC-1: iOS keeps the home-screen quick actions between launches, and a
  // resumed wipe runs no in-memory hook. A launch that restores no signed-in
  // workspace clears them (only the signed-in layout would rebuild them).
  it('clears the home-screen quick actions when a resumed wipe leaves no workspace signed in', async () => {
    stored({
      wipePending: JSON.stringify({ reason: 'user_logout' }),
      serverOrigin: ORIGIN,
      credentialKind: 'pat',
      pat: 'tok',
    });
    await hydrateSession({ devSignIn: true });
    expect(QuickActions.setItems).toHaveBeenCalledWith([]);
  });

  // Same class as QA-SEC-1: the widget snapshot (App Group) and the Siri agent catalog
  // also outlive the process, and only the signed-in layout rewrites them.
  it('clears the widget snapshot and the Siri agent catalog when a resumed wipe leaves no workspace signed in', async () => {
    const sink = { updateTimeline: jest.fn() };
    __setWidgetSinkForTests(sink);
    intents.catalogs.set('agent', [{ id: 'x:1:2', title: 'Left behind' }]);
    stored({
      wipePending: JSON.stringify({ reason: 'user_logout' }),
      serverOrigin: ORIGIN,
      credentialKind: 'pat',
      pat: 'tok',
    });
    await hydrateSession({ devSignIn: true });
    await new Promise((r) => setTimeout(r, 0));
    expect(sink.updateTimeline).toHaveBeenCalled();
    expect(sink.updateTimeline.mock.lastCall[0][0].props.signedIn).toBe(false);
    expect(intents.catalogs.get('agent')).toEqual([]);
  });

  it('leaves the quick actions to the signed-in layout when a workspace is restored signed in', async () => {
    stored({ serverOrigin: ORIGIN, credentialKind: 'pat', pat: 'tok' });
    await hydrateSession({ devSignIn: true });
    expect(getBearerToken()).toBe('tok');
    expect(QuickActions.setItems).not.toHaveBeenCalled();
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
      if (failPatDelete && key === wsKey(SECURE_KEYS.pat)) throw new Error('keystore error');
      fake.values.delete(key);
    });

    // Launch does not hang on the splash screen, and restores nothing.
    await hydrateSession({ devSignIn: true, appState });
    expect(getBearerToken()).toBeNull();
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: null, credential: null });
    expect(has('wipePending')).toBe(true);
    expect(fake.values.get(wsKey(SECURE_KEYS.pat))).toBe('tok');

    // Next launch: the delete works now, so the wipe finishes.
    failPatDelete = false;
    __resetHydrateForTests();
    __resetSessionForTests();
    await hydrateSession({ devSignIn: true, appState });
    expect(left()).toEqual([]);
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: null, credential: null });
  });

  it('reads nothing after a fresh install', async () => {
    checkFreshInstall.mockImplementation(async () => ({ fresh: true, purgeIncomplete: false }));
    stored({ serverOrigin: ORIGIN });
    await hydrateSession({ devSignIn: true });
    // No workspace item is read (the switcher may read the empty registry).
    expect(fake.getItemAsync.mock.calls.filter((c) => c[0].startsWith('az.ws.'))).toEqual([]);
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
    expect(has('refreshToken')).toBe(false);
    expect(has('credentialKind')).toBe(false);
    expect(getPolicyState().source).toBe('default'); // another workspace's policy is ignored
  });

  it('stays connected but signed out when this build cannot use the stored PAT', async () => {
    stored({ serverOrigin: ORIGIN, credentialKind: 'pat', pat: 'tok' });
    await hydrateSession({ devSignIn: false });
    expect(getSession()).toMatchObject({ serverOrigin: ORIGIN, credential: null });
    expect(has('pat')).toBe(false);
    expect(has('credentialKind')).toBe(false);
  });

  it('drops a stored origin that is not a normalised https origin', async () => {
    stored({ serverOrigin: 'http://evil.example.com/path', credentialKind: 'pat', pat: 'tok' });
    await hydrateSession({ devSignIn: true, allowPrivateHttp: false });
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: null, credential: null });
    // The slot is dropped altogether.
    expect(left()).toEqual([]);
    expect(boundWorkspace()).toBeNull();
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
    expect(has('pat')).toBe(true);
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

  it('opens the active workspace only; the others stay stored and are not read', async () => {
    const OTHER = 'bbbbbbbbbbbb';
    stored({ serverOrigin: 'https://other.example.com', credentialKind: 'pat', pat: 'other' }, OTHER, [TEST_WORKSPACE, OTHER]);
    stored({ serverOrigin: ORIGIN, credentialKind: 'pat', pat: 'tok' }, TEST_WORKSPACE, [TEST_WORKSPACE, OTHER]);
    await hydrateSession({ devSignIn: true });
    expect(getSession()).toMatchObject({ serverOrigin: ORIGIN, credential: { kind: 'pat' } });
    expect(getBearerToken()).toBe('tok');
    const reads = fake.getItemAsync.mock.calls.map((c) => c[0]);
    expect(reads.filter((k) => k.startsWith(`az.ws.${OTHER}.`))).toEqual([wsKey(SECURE_KEYS.wipePending, OTHER)]);
  });

  it('starts on the connect screen when no workspace is active, keeping every slot', async () => {
    stored({ serverOrigin: ORIGIN, credentialKind: 'pat', pat: 'tok' }, TEST_WORKSPACE, [TEST_WORKSPACE], null);
    await hydrateSession({ devSignIn: true });
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: null, credential: null });
    expect(has('pat')).toBe(true);
    expect(boundWorkspace()).toBeNull();
  });
});

describe('upgrade from the single-workspace layout', () => {
  const nativeItems = (): Items => ({
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
    projectSelected: '7',
    dbKey: 'ab'.repeat(32),
    policy: JSON.stringify({ v: 1, origin: ORIGIN, policy: clientPolicy({ require_device_lock: false }) }),
  });

  it('moves the signed-in workspace into the first slot: no new sign-in, same project, same database', async () => {
    legacy(nativeItems());
    await hydrateSession();
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: ORIGIN, projectId: 7, credential: { kind: 'oauth' } });
    expect(getBearerToken()).toBe(opaqueToken('elnat_', 1));
    expect(boundWorkspace()).toBe(MIGRATED_WORKSPACE_ID);
    expect(JSON.parse(fake.values.get(DEVICE_KEYS.workspaces)!)).toEqual({
      v: 1,
      active: MIGRATED_WORKSPACE_ID,
      ids: [MIGRATED_WORKSPACE_ID],
    });
    // The encrypted database keeps its file and key: its cache, outbox and sync cursors survive.
    expect(fake.values.get(wsKey(SECURE_KEYS.dbKey, MIGRATED_WORKSPACE_ID))).toBe('ab'.repeat(32));
    expect(JSON.parse(fake.values.get(wsKey(SECURE_KEYS.workspaceMeta, MIGRATED_WORKSPACE_ID))!)).toMatchObject({
      v: 1,
      dbName: 'az-offline.db',
    });
    // Nothing is left under the old names.
    for (const key of Object.values(SECURE_KEYS)) expect(fake.values.has(key)).toBe(false);
  });

  it('is idempotent: a crash before the registry was written copies again next launch', async () => {
    legacy(nativeItems());
    const appState = fakeAppState();
    fake.setItemAsync.mockImplementationOnce(async (key: string, value: string) => {
      fake.values.set(key, value);
    });
    let failRegistry = true;
    const realSet = fake.setItemAsync.getMockImplementation()!;
    fake.setItemAsync.mockImplementation(async (key: string, value: string) => {
      if (failRegistry && key === DEVICE_KEYS.workspaces) throw new Error('keychain busy');
      return realSet(key, value);
    });
    const done = hydrateSession({ appState });
    await new Promise((r) => setTimeout(r, 0));
    expect(getSession().hydrated).toBe(false);
    // The old items are still there: nothing was lost.
    expect(fake.values.get(SECURE_KEYS.projectSelected)).toBe('7');
    failRegistry = false;
    appState.emit('active');
    await done;
    expect(getSession()).toMatchObject({ serverOrigin: ORIGIN, projectId: 7, credential: { kind: 'oauth' } });
    expect(fake.values.has(SECURE_KEYS.projectSelected)).toBe(false);
  });

  it('finishes an interrupted single-workspace wipe instead of migrating it', async () => {
    legacy({ ...nativeItems(), wipePending: JSON.stringify({ reason: 'device_revoked' }) });
    await hydrateSession();
    expect(getSession()).toMatchObject({ hydrated: true, serverOrigin: null, credential: null });
    expect([...fake.values.keys()]).toEqual([]);
  });

  it('deletes old items left behind once the registry exists', async () => {
    stored({ serverOrigin: ORIGIN, credentialKind: 'pat', pat: 'tok' });
    legacy({ pat: 'stale' });
    await hydrateSession({ devSignIn: true });
    expect(fake.values.has(SECURE_KEYS.pat)).toBe(false);
    expect(getBearerToken()).toBe('tok');
  });
});
