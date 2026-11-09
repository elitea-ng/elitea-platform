// MDM lock_server (ADR-0052 W1b.2): only the managed workspace stays on the device.
import { __resetNativeSessionForTests } from '@/auth/nativeSession';
import { abortAll } from '@/lib/abortRegistry';
import { NO_MANAGED_CONFIG, parseManagedConfig, refreshManagedConfig } from '@/security/managedConfig';
import { lockedOrigin } from '@/security/managedServer';
import {
  __resetManagedServerLockForTests,
  managedServerLockSettled,
  startManagedServerLock,
} from '@/security/managedServerLock';
import { DEVICE_KEYS, SECURE_KEYS, bindWorkspace } from '@/security/secureStorage';
import { __resetRegistryForTests, readRegistry } from '@/security/workspaceRegistry';
import { __resetSessionForTests, getSession, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceTransitionsForTests, keepOnlyWorkspace } from '@/state/workspace';
import { __resetWorkspaceListForTests } from '@/state/workspaceList';
import { TEST_WORKSPACE, registryValue, wsKey, type FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');

const A = TEST_WORKSPACE;
const B = 'bbbbbbbbbbbb';
const C = 'cccccccccccc';
const ORIGIN_A = 'https://a.example.com';
const ORIGIN_B = 'https://b.example.com';
const ORIGIN_C = 'https://c.example.com';

function slot(id: string, origin: string, token: string) {
  fake.values.set(wsKey(SECURE_KEYS.serverOrigin, id), origin);
  fake.values.set(wsKey(SECURE_KEYS.credentialKind, id), 'pat');
  fake.values.set(wsKey(SECURE_KEYS.pat, id), token);
  fake.values.set(wsKey(SECURE_KEYS.dbKey, id), id.slice(0, 1).repeat(64));
}

/** A (active, signed in), B and C put aside. */
function threeWorkspaces() {
  slot(A, ORIGIN_A, 'token-a');
  slot(B, ORIGIN_B, 'token-b');
  slot(C, ORIGIN_C, 'token-c');
  fake.values.set(DEVICE_KEYS.workspaces, registryValue([A, B, C], A));
  bindWorkspace(A);
  setSession({ hydrated: true, serverOrigin: ORIGIN_A, workspaceId: A });
  setCredential({ kind: 'pat', token: 'token-a' });
}

const itemsOf = (id: string) => Object.fromEntries([...fake.values].filter(([k]) => k.startsWith(`az.ws.${id}.`)));
const registry = () => JSON.parse(fake.values.get(DEVICE_KEYS.workspaces)!);

let stop: () => void = () => {};

/** Until no run is under way (a run may chain the next one). */
async function settled() {
  for (let i = 0; i < 10; i++) {
    await managedServerLockSettled();
    await Promise.resolve();
  }
}

beforeEach(() => {
  __resetSessionForTests();
  __resetRegistryForTests();
  __resetWorkspaceTransitionsForTests();
  __resetWorkspaceListForTests();
  __resetNativeSessionForTests();
  __resetManagedServerLockForTests();
  refreshManagedConfig(() => null);
  bindWorkspace(null);
  abortAll();
  fake.values.clear();
  jest.clearAllMocks();
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});

afterEach(() => {
  stop();
  stop = () => {};
  refreshManagedConfig(() => null);
  jest.restoreAllMocks();
});

describe('lockedOrigin', () => {
  it('is the canonical origin of server_url only with lock_server true', () => {
    expect(lockedOrigin(parseManagedConfig({ server_url: 'https://A.example.com/', lock_server: true }), false)).toBe(ORIGIN_A);
    expect(lockedOrigin(parseManagedConfig({ server_url: 'a.example.com', lock_server: 1 }), false)).toBe(ORIGIN_A);
    expect(lockedOrigin(parseManagedConfig({ server_url: ORIGIN_A, lock_server: false }), false)).toBeNull();
    expect(lockedOrigin(parseManagedConfig({ server_url: ORIGIN_A }), false)).toBeNull();
    expect(lockedOrigin(NO_MANAGED_CONFIG, false)).toBeNull();
  });

  it('locks nothing without a usable server_url (never wipes everything on a typo)', () => {
    expect(lockedOrigin(parseManagedConfig({ lock_server: true }), false)).toBeNull();
    expect(lockedOrigin(parseManagedConfig({ server_url: 'http://a.example.com', lock_server: true }), false)).toBeNull();
    expect(lockedOrigin(parseManagedConfig({ server_url: 'not a url', lock_server: true }), false)).toBeNull();
    // A wrong-typed lock_server is ignored, not read as true.
    expect(lockedOrigin(parseManagedConfig({ server_url: ORIGIN_A, lock_server: 'yes' }), false)).toBeNull();
  });
});

describe('keepOnlyWorkspace', () => {
  it('wipes every workspace on another server like a logout; the managed one keeps everything', async () => {
    threeWorkspaces();
    const beforeA = itemsOf(A);
    await expect(keepOnlyWorkspace(ORIGIN_A)).resolves.toEqual([B, C]);
    expect(itemsOf(A)).toEqual(beforeA);
    expect(itemsOf(B)).toEqual({});
    expect(itemsOf(C)).toEqual({});
    expect(registry()).toEqual({ v: 1, active: A, ids: [A] });
    expect(getSession()).toMatchObject({ serverOrigin: ORIGIN_A, workspaceId: A });
  });

  it('signs the ACTIVE workspace out when it is on another server (the app lands on the connect screen)', async () => {
    threeWorkspaces();
    await expect(keepOnlyWorkspace(ORIGIN_B)).resolves.toEqual([A, C]);
    expect(itemsOf(A)).toEqual({});
    expect(itemsOf(C)).toEqual({});
    expect(registry()).toEqual({ v: 1, active: null, ids: [B] });
    expect(getSession()).toMatchObject({ serverOrigin: null, credential: null, endReason: 'managed_server' });
  });

  it('removes every workspace when none is on the managed server', async () => {
    threeWorkspaces();
    await expect(keepOnlyWorkspace('https://managed.example.com')).resolves.toEqual([A, B, C]);
    expect([...fake.values.keys()].filter((k) => k.startsWith('az.ws.'))).toEqual([]);
  });

  it('leaves a workspace whose origin cannot be read (never guessed)', async () => {
    threeWorkspaces();
    const original = fake.getItemAsync.getMockImplementation()!;
    fake.getItemAsync.mockImplementation(async (key: string) => {
      if (key === wsKey(SECURE_KEYS.serverOrigin, C)) throw new Error('keychain locked');
      return original(key);
    });
    await expect(keepOnlyWorkspace(ORIGIN_A)).resolves.toEqual([B]);
    fake.getItemAsync.mockImplementation(original);
    expect(itemsOf(C)).not.toEqual({});
  });
});

describe('startManagedServerLock', () => {
  it('applies once hydrated, on a change while running, and restores nothing when the key goes', async () => {
    threeWorkspaces();
    setSession({ hydrated: false });
    stop = startManagedServerLock({ allowPrivateHttp: false });

    refreshManagedConfig(() => ({ server_url: ORIGIN_A, lock_server: true }));
    await settled();
    // Not before hydration: the resumed wipes and the registry come first.
    expect(registry().ids).toEqual([A, B, C]);

    setSession({ hydrated: true });
    await settled();
    expect(registry().ids).toEqual([A]);

    // The MDM removes lock_server: the wiped workspaces do not come back.
    refreshManagedConfig(() => ({ server_url: ORIGIN_A }));
    await settled();
    expect(registry().ids).toEqual([A]);
    expect(itemsOf(B)).toEqual({});
  });

  it('does nothing for server_url alone (pre-filled, not locked)', async () => {
    threeWorkspaces();
    stop = startManagedServerLock({ allowPrivateHttp: false });
    refreshManagedConfig(() => ({ server_url: ORIGIN_A }));
    await settled();
    expect(registry().ids).toEqual([A, B, C]);
  });

  it('runs again when the registry changes, so a workspace added on another server meanwhile goes too', async () => {
    threeWorkspaces();
    stop = startManagedServerLock({ allowPrivateHttp: false });
    refreshManagedConfig(() => ({ server_url: ORIGIN_A, lock_server: true }));
    await settled();
    expect(registry().ids).toEqual([A]);
    // A slot that appeared afterwards (written by a flow that slipped past the screens).
    slot(B, ORIGIN_B, 'token-b');
    fake.values.set(DEVICE_KEYS.workspaces, registryValue([B, A], A));
    await readRegistry(); // publishes the new snapshot
    await settled();
    expect(registry().ids).toEqual([A]);
    expect(itemsOf(B)).toEqual({});
  });

  it('connectWorkspace refuses another server under the lock', async () => {
    refreshManagedConfig(() => ({ server_url: ORIGIN_A, lock_server: true }));
    const { connectWorkspace } = jest.requireActual<typeof import('@/state/workspace')>('@/state/workspace');
    await expect(connectWorkspace(ORIGIN_B)).resolves.toBe(false);
    expect(fake.values.get(DEVICE_KEYS.workspaces)).toBeUndefined();
  });

  it('re-checks on every managed change, keeping the current managed server', async () => {
    const keepOnly = jest.fn(async () => []);
    setSession({ hydrated: true });
    stop = startManagedServerLock({ keepOnly, allowPrivateHttp: false });
    refreshManagedConfig(() => ({ server_url: ORIGIN_A, lock_server: true }));
    await settled();
    refreshManagedConfig(() => ({ server_url: ORIGIN_A, lock_server: true, idle_lock_seconds: 30 }));
    await settled();
    refreshManagedConfig(() => ({ server_url: ORIGIN_B, lock_server: true }));
    await settled();
    // Every change re-checks (a workspace may have been added meanwhile); the last one keeps B.
    expect(keepOnly.mock.calls).toEqual([[ORIGIN_A], [ORIGIN_A], [ORIGIN_B]]);
  });
});
