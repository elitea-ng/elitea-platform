import { opaqueToken } from '@/api/__fixtures__/nativeClient';
import { __resetNativeSessionForTests, __setNativeSessionDepsForTests, getNativeTokenSet } from '@/auth/nativeSession';
import { abortAll, registerAbortable } from '@/lib/abortRegistry';
import { endSession, registerStoredWipeHook, registerWipeHook } from '@/security/endSession';
import { DEVICE_KEYS, SECURE_KEYS, bindWorkspace, boundWorkspace } from '@/security/secureStorage';
import { MAX_WORKSPACES, __resetRegistryForTests } from '@/security/workspaceRegistry';
import { __resetSessionForTests, getBearerToken, getSession, setCredential, setSession } from '@/state/session';
import {
  __resetWorkspaceTransitionsForTests,
  beginAddWorkspace,
  classifyIncomingLink,
  connectWorkspace,
  isSwitchingWorkspace,
  leaveSignIn,
  onWorkspaceSwitch,
  removeAllWorkspaces,
  removeWorkspace,
  returnToPreviousWorkspace,
  switchToWorkspace,
  type SwitchEvent,
} from '@/state/workspace';
import { __resetWorkspaceListForTests, getWorkspaceList, refreshWorkspaceList } from '@/state/workspaceList';
import { TEST_WORKSPACE, registryValue, wsKey, type FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');

const OPTS = { connectSchemes: ['agentzefir'], allowPrivateHttp: false };
const A = TEST_WORKSPACE;
const B = 'bbbbbbbbbbbb';
const ORIGIN_A = 'https://a.example.com';
const ORIGIN_B = 'https://b.example.com';

beforeEach(() => {
  __resetSessionForTests();
  __resetRegistryForTests();
  __resetWorkspaceTransitionsForTests();
  __resetWorkspaceListForTests();
  __resetNativeSessionForTests();
  bindWorkspace(null);
  abortAll();
  fake.values.clear();
  jest.clearAllMocks();
  // clearAllMocks keeps implementations: put back the default after a test overrode it.
  fake.deleteItemAsync.mockImplementation(async (key: string) => {
    fake.values.delete(key);
  });
  fake.setItemAsync.mockImplementation(async (key: string, value: string) => {
    fake.values.set(key, value);
  });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});

afterEach(() => jest.restoreAllMocks());

/** A signed-in workspace slot (developer sign-in), as this app version stores it. */
function slot(id: string, origin: string, token: string, project: string) {
  fake.values.set(wsKey(SECURE_KEYS.serverOrigin, id), origin);
  fake.values.set(wsKey(SECURE_KEYS.credentialKind, id), 'pat');
  fake.values.set(wsKey(SECURE_KEYS.pat, id), token);
  fake.values.set(wsKey(SECURE_KEYS.projectSelected, id), project);
  fake.values.set(wsKey(SECURE_KEYS.dbKey, id), id.slice(0, 1).repeat(64));
}

/** Makes slot `id` a native (OAuth) slot with a stored device session. */
function nativeSlot(id: string, origin: string) {
  fake.values.delete(wsKey(SECURE_KEYS.pat, id));
  fake.values.set(wsKey(SECURE_KEYS.credentialKind, id), 'oauth');
  fake.values.set(
    wsKey(SECURE_KEYS.refreshToken, id),
    JSON.stringify({
      v: 1,
      origin,
      clientId: 'ca.zefir.agent',
      tokenEndpoint: `${origin}/api/v2/auth/native/token`,
      revocationEndpoint: `${origin}/api/v2/auth/native/revoke`,
      refreshToken: opaqueToken('elnrt_', 2),
      refreshExpiresAt: 0,
      deviceId: 'd',
    }),
  );
  fake.values.set(
    wsKey(SECURE_KEYS.accessToken, id),
    JSON.stringify({ v: 1, accessToken: opaqueToken('elnat_', 2), accessExpiresAt: 0 }),
  );
}

/** Workspaces A (active, signed in) and B (connected, put aside). */
function twoWorkspaces() {
  slot(A, ORIGIN_A, 'token-a', '3');
  slot(B, ORIGIN_B, 'token-b', '9');
  fake.values.set(DEVICE_KEYS.workspaces, registryValue([A, B], A));
  bindWorkspace(A);
  setSession({ hydrated: true, serverOrigin: ORIGIN_A, workspaceId: A, projectId: 3 });
  setCredential({ kind: 'pat', token: 'token-a' });
}

const itemsOf = (id: string) =>
  Object.fromEntries([...fake.values].filter(([k]) => k.startsWith(`az.ws.${id}.`)));
const registry = () => JSON.parse(fake.values.get(DEVICE_KEYS.workspaces)!);

describe('connectWorkspace', () => {
  it('creates a workspace slot of its own, persisted before the session changes', async () => {
    let sessionAtRegistryWrite: string | null | undefined;
    fake.setItemAsync.mockImplementation(async (key: string, value: string) => {
      if (key === DEVICE_KEYS.workspaces) sessionAtRegistryWrite = getSession().serverOrigin;
      fake.values.set(key, value);
    });
    await expect(connectWorkspace(ORIGIN_A)).resolves.toBe(true);
    const id = boundWorkspace()!;
    expect(id).toMatch(/^[0-9a-f]{12}$/);
    expect(sessionAtRegistryWrite).toBeNull();
    expect(fake.values.get(wsKey(SECURE_KEYS.serverOrigin, id))).toBe(ORIGIN_A);
    expect(JSON.parse(fake.values.get(wsKey(SECURE_KEYS.workspaceMeta, id))!)).toMatchObject({
      v: 1,
      dbName: `az-offline-${id}.db`,
    });
    expect(registry()).toEqual({ v: 1, active: id, ids: [id] });
    expect(getSession()).toMatchObject({ serverOrigin: ORIGIN_A, workspaceId: id });
  });

  it('adds a slot next to the stored ones, which keep everything', async () => {
    slot(A, ORIGIN_A, 'token-a', '3');
    fake.values.set(DEVICE_KEYS.workspaces, registryValue([A], null));
    const before = itemsOf(A);
    await connectWorkspace(ORIGIN_B);
    const id = boundWorkspace()!;
    expect(id).not.toBe(A);
    expect(registry()).toEqual({ v: 1, active: id, ids: [id, A] });
    expect(itemsOf(A)).toEqual(before);
  });

  it('switches to the slot already stored for the same origin instead of adding a second one', async () => {
    slot(A, ORIGIN_A, 'token-a', '3');
    fake.values.set(DEVICE_KEYS.workspaces, registryValue([A], null));
    await expect(connectWorkspace(ORIGIN_A)).resolves.toBe(true);
    expect(boundWorkspace()).toBe(A);
    expect(registry().ids).toEqual([A]);
    expect(getBearerToken()).toBe('token-a');
  });

  // WS-3: a lookup error is not "not connected".
  it('fails instead of adding a duplicate slot when the stored origins cannot be read', async () => {
    slot(A, ORIGIN_A, 'token-a', '3');
    slot(B, ORIGIN_B, 'token-b', '9');
    fake.values.set(DEVICE_KEYS.workspaces, registryValue([B, A], null));
    const read = fake.getItemAsync.getMockImplementation()!;
    fake.getItemAsync.mockImplementation(async (key: string) => {
      if (key === wsKey(SECURE_KEYS.serverOrigin, B)) throw new Error('keychain busy');
      return read(key);
    });
    let connected: boolean;
    try {
      connected = await connectWorkspace(ORIGIN_A);
    } finally {
      fake.getItemAsync.mockImplementation(read);
    }
    expect(connected).toBe(false);
    expect(registry().ids).toEqual([B, A]);
    expect([...fake.values.keys()].filter((k) => k.endsWith('.server.origin'))).toHaveLength(2);
  });

  // F3: a slot with an unfinished wipe keeps its origin for the retry.
  it('finishes the unfinished wipe of a slot for the same origin, then connects a fresh one', async () => {
    slot(B, ORIGIN_B, 'token-b', '9');
    fake.values.set(wsKey(SECURE_KEYS.wipePending, B), JSON.stringify({ reason: 'user_logout' }));
    fake.values.set(DEVICE_KEYS.workspaces, registryValue([B], null));
    await expect(connectWorkspace(ORIGIN_B)).resolves.toBe(true);
    const id = boundWorkspace()!;
    expect(id).not.toBe(B);
    expect(itemsOf(B)).toEqual({});
    expect(registry()).toEqual({ v: 1, active: id, ids: [id] });
  });

  it('adds one slot for an origin connected twice at the same time', async () => {
    const results = await Promise.all([connectWorkspace(ORIGIN_A), connectWorkspace(ORIGIN_A)]);
    expect(results).toEqual([true, true]);
    expect(registry().ids).toHaveLength(1);
    expect([...fake.values.keys()].filter((k) => k.endsWith('.server.origin'))).toHaveLength(1);
  });

  it(`refuses a workspace beyond ${MAX_WORKSPACES}`, async () => {
    const ids = Array.from({ length: MAX_WORKSPACES }, (_, i) => `${i}`.padStart(12, 'c'));
    ids.forEach((id, i) => fake.values.set(wsKey(SECURE_KEYS.serverOrigin, id), `https://w${i}.example.com`));
    fake.values.set(DEVICE_KEYS.workspaces, registryValue(ids, null));
    await expect(connectWorkspace('https://one-too-many.example.com')).resolves.toBe(false);
    expect(registry().ids).toHaveLength(MAX_WORKSPACES);
  });

  it('does not connect when the origin cannot be stored, and leaves nothing behind', async () => {
    fake.setItemAsync.mockImplementation(async (key: string) => {
      throw new Error(`keychain ${key}`);
    });
    await expect(connectWorkspace(ORIGIN_A)).resolves.toBe(false);
    expect(getSession().serverOrigin).toBeNull();
    expect(boundWorkspace()).toBeNull();
    expect(fake.values.size).toBe(0);
  });
});

describe('switchToWorkspace', () => {
  it('puts the active workspace aside without deleting anything and opens the other one signed in', async () => {
    twoWorkspaces();
    const beforeA = itemsOf(A);
    const controller = new AbortController();
    registerAbortable(controller);
    const reset = jest.fn();
    const unregister = registerWipeHook('probe', reset);

    await expect(switchToWorkspace(B)).resolves.toBe(true);

    expect(controller.signal.aborted).toBe(true); // A's in-flight work stops
    expect(reset).toHaveBeenCalledTimes(1); // A's in-memory state is forgotten…
    expect(itemsOf(A)).toEqual(beforeA); // …but nothing of A is deleted
    expect(fake.deleteItemAsync).not.toHaveBeenCalled();
    expect(boundWorkspace()).toBe(B);
    expect(getSession()).toMatchObject({
      serverOrigin: ORIGIN_B,
      workspaceId: B,
      projectId: 9,
      credential: { kind: 'pat' },
      switching: false,
      ending: false,
    });
    expect(getBearerToken()).toBe('token-b');
    expect(registry()).toEqual({ v: 1, active: B, ids: [B, A] });
    unregister();
  });

  it('switches back with the first workspace exactly as it was', async () => {
    twoWorkspaces();
    await switchToWorkspace(B);
    await switchToWorkspace(A);
    expect(getSession()).toMatchObject({ serverOrigin: ORIGIN_A, workspaceId: A, projectId: 3 });
    expect(getBearerToken()).toBe('token-a');
    expect(registry().ids).toEqual([A, B]);
  });

  it('tells listeners before and after (the device lock carries presence)', async () => {
    twoWorkspaces();
    const events: SwitchEvent[] = [];
    onWorkspaceSwitch((event) => events.push(event));
    await switchToWorkspace(B);
    expect(events).toEqual([{ phase: 'before' }, { phase: 'after', signedIn: true }]);
  });

  it('opens a signed-out workspace on its sign-in screen', async () => {
    twoWorkspaces();
    fake.values.delete(wsKey(SECURE_KEYS.credentialKind, B));
    fake.values.delete(wsKey(SECURE_KEYS.pat, B));
    await switchToWorkspace(B);
    expect(getSession()).toMatchObject({ serverOrigin: ORIGIN_B, workspaceId: B, credential: null });
  });

  it('refuses an id that is not connected', async () => {
    twoWorkspaces();
    await expect(switchToWorkspace('dddddddddddd')).resolves.toBe(false);
    expect(getSession()).toMatchObject({ workspaceId: A, credential: { kind: 'pat' } });
  });

  // F1: a switch that fails after the target's credential was already loaded
  // must not leave that half-restored workspace live in memory.
  it('forgets the half-restored workspace when opening it fails (no token, revoke or refresher left)', async () => {
    twoWorkspaces();
    nativeSlot(B, ORIGIN_B);
    const revoke = jest.fn(async () => true);
    __setNativeSessionDepsForTests({ revoke });
    const write = fake.setItemAsync.getMockImplementation()!;
    fake.setItemAsync.mockImplementation(async (key: string, value: string) => {
      if (key === DEVICE_KEYS.workspaces) throw new Error('device locked');
      return write(key, value);
    });
    const reset = jest.fn();
    const off = registerWipeHook('probe', reset);

    await expect(switchToWorkspace(B)).resolves.toBe(false);
    fake.setItemAsync.mockImplementation(write);

    expect(reset).toHaveBeenCalledTimes(2); // A put aside, then B's half-restore forgotten
    expect(getNativeTokenSet()).toBeNull();
    expect(getBearerToken()).toBeNull();
    expect(boundWorkspace()).toBeNull();
    expect(getSession()).toMatchObject({ serverOrigin: null, workspaceId: null, credential: null, switching: false });
    // A later end of whatever is active next must not revoke B's device session.
    await endSession({ reason: 'user_logout' });
    expect(revoke).not.toHaveBeenCalled();
    expect(itemsOf(B)[wsKey(SECURE_KEYS.refreshToken, B)]).toBeDefined();
    off();
  });

  it('puts leftover in-memory state aside even when nothing is bound', async () => {
    const reset = jest.fn();
    const off = registerWipeHook('probe', reset);
    setCredential({ kind: 'pat', token: 'stale' });
    slot(B, ORIGIN_B, 'token-b', '9');
    fake.values.set(DEVICE_KEYS.workspaces, registryValue([B], null));
    await expect(switchToWorkspace(B)).resolves.toBe(true);
    expect(reset).toHaveBeenCalledTimes(1);
    expect(getBearerToken()).toBe('token-b');
    off();
  });

  it('finishes an interrupted wipe of the target instead of opening it', async () => {
    twoWorkspaces();
    fake.values.set(wsKey(SECURE_KEYS.wipePending, B), JSON.stringify({ reason: 'device_revoked' }));
    await expect(switchToWorkspace(B)).resolves.toBe(false);
    expect(itemsOf(B)).toEqual({});
    expect(registry().ids).toEqual([A]);
    expect(getSession()).toMatchObject({ serverOrigin: null, credential: null });
    // A is still stored and can be opened again.
    await expect(switchToWorkspace(A)).resolves.toBe(true);
  });
});

describe('add workspace', () => {
  it('puts the active workspace aside for the connect screen, and Back returns to it', async () => {
    twoWorkspaces();
    await beginAddWorkspace('https://c.example.com');
    expect(getSession()).toMatchObject({
      serverOrigin: null,
      credential: null,
      pendingConnectOrigin: 'https://c.example.com',
      returnWorkspaceId: A,
    });
    expect(boundWorkspace()).toBeNull();
    // A relaunch now still opens A.
    expect(registry().active).toBe(A);
    expect(itemsOf(A)[wsKey(SECURE_KEYS.pat)]).toBe('token-a');

    await expect(returnToPreviousWorkspace()).resolves.toBe(true);
    expect(getSession()).toMatchObject({ workspaceId: A, returnWorkspaceId: null });
    expect(getBearerToken()).toBe('token-a');
  });
});

// F2: a new slot is not the launch default until it has signed in.
describe('add workspace, then back out of its sign-in', () => {
  const ORIGIN_C = 'https://c.example.com';

  it('keeps the previous workspace active and the Back link when the new slot is left unsigned', async () => {
    twoWorkspaces();
    await beginAddWorkspace();
    await expect(connectWorkspace(ORIGIN_C)).resolves.toBe(true);
    const c = boundWorkspace()!;
    // A relaunch on C's sign-in screen still opens A.
    expect(registry()).toEqual({ v: 1, active: A, ids: [c, A, B] });
    expect(getSession().returnWorkspaceId).toBe(A);

    await leaveSignIn(); // "Use a different workspace"

    expect(registry()).toEqual({ v: 1, active: A, ids: [A, B] });
    expect(getSession()).toMatchObject({ serverOrigin: null, returnWorkspaceId: A });
    await expect(returnToPreviousWorkspace()).resolves.toBe(true);
    expect(getBearerToken()).toBe('token-a');
  });

  it('does not make a signed-out workspace the launch default by switching to it', async () => {
    twoWorkspaces();
    fake.values.delete(wsKey(SECURE_KEYS.credentialKind, B));
    fake.values.delete(wsKey(SECURE_KEYS.pat, B));
    await switchToWorkspace(B);
    expect(registry().active).toBe(A);
  });
});

describe('removeWorkspace', () => {
  it('revokes and wipes a workspace that is not active, leaving the active one untouched', async () => {
    twoWorkspaces();
    const revoke = jest.fn(async () => true);
    __setNativeSessionDepsForTests({ revoke });
    fake.values.set(
      wsKey(SECURE_KEYS.refreshToken, B),
      JSON.stringify({
        v: 1,
        origin: ORIGIN_B,
        clientId: 'ca.zefir.agent',
        tokenEndpoint: `${ORIGIN_B}/api/v2/auth/native/token`,
        revocationEndpoint: `${ORIGIN_B}/api/v2/auth/native/revoke`,
        refreshToken: opaqueToken('elnrt_', 1),
        refreshExpiresAt: 0,
        deviceId: 'd',
      }),
    );
    const beforeA = itemsOf(A);

    await expect(removeWorkspace(B)).resolves.toBe(true);

    expect(revoke).toHaveBeenCalledWith(
      ORIGIN_B,
      `${ORIGIN_B}/api/v2/auth/native/revoke`,
      { token: opaqueToken('elnrt_', 1), clientId: 'ca.zefir.agent' },
      expect.objectContaining({ signal: expect.anything() }),
    );
    expect(itemsOf(B)).toEqual({});
    expect(itemsOf(A)).toEqual(beforeA);
    expect(registry()).toEqual({ v: 1, active: A, ids: [A] });
    expect(getBearerToken()).toBe('token-a');
  });

  it('logs out of the active workspace and wipes only it; the others stay connected', async () => {
    twoWorkspaces();
    const beforeB = itemsOf(B);
    await expect(removeWorkspace(A)).resolves.toBe(true);
    expect(itemsOf(A)).toEqual({});
    expect(itemsOf(B)).toEqual(beforeB);
    expect(registry()).toEqual({ v: 1, active: null, ids: [B] });
    expect(getSession()).toMatchObject({ serverOrigin: null, credential: null, endReason: 'user_logout' });
    await refreshWorkspaceList();
    expect(getWorkspaceList().map((w) => w.origin)).toEqual([ORIGIN_B]);
  });

  // WS-2: the device lock ignores policy changes while the ACTIVE session is
  // being replaced; removing another workspace must not mute it.
  it('does not count as replacing the active session while a workspace that is not active is removed', async () => {
    twoWorkspaces();
    const seen: boolean[] = [];
    const off = registerStoredWipeHook('probe', () => {
      seen.push(isSwitchingWorkspace());
    });
    await expect(removeWorkspace(B)).resolves.toBe(true);
    off();
    expect(seen).toEqual([false]);
  });

  it('counts as replacing the active session during a switch', async () => {
    twoWorkspaces();
    const seen: boolean[] = [];
    const off = registerWipeHook('probe', () => {
      seen.push(isSwitchingWorkspace());
    });
    await expect(switchToWorkspace(B)).resolves.toBe(true);
    off();
    expect(seen).toEqual([true]);
    expect(isSwitchingWorkspace()).toBe(false);
  });

  it('"reset" wipes every workspace on the device', async () => {
    twoWorkspaces();
    await expect(removeAllWorkspaces()).resolves.toBe(true);
    expect([...fake.values.keys()].filter((k) => k.startsWith('az.ws.'))).toEqual([]);
    expect(registry().ids).toEqual([]);
  });
});

describe('workspace list', () => {
  it('lists every workspace with its host, brand name and mark, and whether it is signed in', async () => {
    twoWorkspaces();
    fake.values.set(
      wsKey(SECURE_KEYS.brandTheme, B),
      JSON.stringify({
        origin: ORIGIN_B,
        productName: 'Acme AI',
        productDefault: false,
        logoMark: { uri: `${ORIGIN_B}/brand/mark.svg`, kind: 'svg' },
        logoFull: null,
        light: {},
        dark: {},
        fontFaces: [],
        fontFamily: null,
        accentSource: { hue: '#e8a33a', lightAccent: '#14B8A6', darkAccent: 'url(evil)', lightOn: null, darkOn: null },
      }),
    );
    fake.values.set(
      wsKey(SECURE_KEYS.policy, B),
      JSON.stringify({ v: 1, origin: ORIGIN_B, policy: { require_device_lock: true } }),
    );
    fake.values.delete(wsKey(SECURE_KEYS.credentialKind, A));
    await refreshWorkspaceList();
    expect(getWorkspaceList()).toEqual([
      { id: A, origin: ORIGIN_A, name: null, logoMark: null, brand: null, signedIn: false, requiresLock: false },
      {
        id: B,
        origin: ORIGIN_B,
        name: 'Acme AI',
        logoMark: { uri: `${ORIGIN_B}/brand/mark.svg`, kind: 'svg' },
        // Each workspace's own accent, re-validated (a non-colour is dropped), for its row's tile.
        brand: { productDefault: false, hue: '#e8a33a', lightAccent: '#14b8a6', darkAccent: null, lightOn: null, darkOn: null },
        signedIn: true,
        requiresLock: true,
      },
    ]);
  });

  it('never shows a brand mark from another host', async () => {
    twoWorkspaces();
    fake.values.set(
      wsKey(SECURE_KEYS.brandTheme, B),
      JSON.stringify({ origin: ORIGIN_B, productName: 'X', productDefault: false, logoMark: { uri: 'https://evil.example/m.svg', kind: 'svg' } }),
    );
    await refreshWorkspaceList();
    expect(getWorkspaceList()[1].logoMark).toBeNull();
  });
});

describe('classifyIncomingLink', () => {
  const link = (server: string) => `agentzefir://connect?server=${encodeURIComponent(server)}`;

  it('offers to ADD a workspace that is not connected yet (nothing is wiped)', () => {
    expect(classifyIncomingLink(link('https://b.example.com/x'), 'https://a.example.com', OPTS)).toEqual({
      kind: 'add',
      from: 'https://a.example.com',
      origin: 'https://b.example.com',
    });
  });

  it('offers to switch to a workspace already connected on this device', () => {
    const known = [{ id: B, origin: 'https://b.example.com' }];
    expect(classifyIncomingLink(link('https://b.example.com'), 'https://a.example.com', OPTS, known)).toEqual({
      kind: 'switch',
      from: 'https://a.example.com',
      origin: 'https://b.example.com',
      workspaceId: B,
    });
  });

  it('is a no-op for the connected workspace', () => {
    expect(classifyIncomingLink(link('https://a.example.com'), 'https://a.example.com', OPTS)).toEqual({
      kind: 'same',
    });
  });

  it('leaves it to the connect screen when not connected', () => {
    expect(classifyIncomingLink(link('https://b.example.com'), null, OPTS)).toEqual({
      kind: 'connect',
      origin: 'https://b.example.com',
    });
  });

  it('ignores non-connect URLs and invalid links', () => {
    for (const url of [
      null,
      'ca.zefir.agent:/oauth/callback?code=x',
      'exp+agent-zefir://expo-development-client/?url=x',
      'https://b.example.com',
      link('http://b.example.com'),
      'agentzefir://connect',
    ]) {
      expect(classifyIncomingLink(url, 'https://a.example.com', OPTS)).toEqual({ kind: 'ignore' });
    }
  });
});
