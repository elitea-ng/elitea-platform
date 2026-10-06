import { abortAll, inFlightCount, registerAbortable } from '@/lib/abortRegistry';
import {
  REVOKE_TIMEOUT_MS,
  endSession,
  registerRevokeHook,
  registerStoredWipeHook,
  registerWipeHook,
  resumePendingWipes,
  wipeStoredWorkspace,
} from '@/security/endSession';
import { DEVICE_KEYS, REGISTERED_KEYS, SECURE_KEYS, bindWorkspace, boundWorkspace } from '@/security/secureStorage';
import { __resetRegistryForTests } from '@/security/workspaceRegistry';
import {
  __resetSessionForTests,
  getBearerToken,
  getSession,
  setCredential,
  setSession,
} from '@/state/session';
import { TEST_WORKSPACE, registryValue, wsKey, type FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
// The app's caches directory: Library/Caches on iOS, where NSURLCache keeps Cache.db (DEF-R2).
jest.mock('expo-file-system', () => {
  const cacheEntries: { name: string; delete: jest.Mock }[] = [];
  return {
    __cacheEntries: cacheEntries,
    Paths: { cache: 'file:///Library/Caches/' },
    Directory: jest.fn().mockImplementation((uri: string) => ({
      uri,
      exists: true,
      list: () => [...cacheEntries],
    })),
  };
});

const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');

/** Another connected workspace, which no end of the active session may touch. */
const OTHER = 'bbbbbbbbbbbb';

function signedIn() {
  bindWorkspace(TEST_WORKSPACE);
  setSession({ hydrated: true, serverOrigin: 'https://ai.example.com', projectId: 7 });
  setCredential({ kind: 'pat', token: 'tok' });
  fake.values.set(DEVICE_KEYS.workspaces, registryValue([TEST_WORKSPACE, OTHER]));
  for (const key of REGISTERED_KEYS) {
    if (key !== SECURE_KEYS.wipePending) fake.values.set(wsKey(key), 'stored');
    if (key !== SECURE_KEYS.wipePending) fake.values.set(wsKey(key, OTHER), 'other workspace');
  }
}

/** Only the other workspace and the registry are left. */
function onlyOtherLeft() {
  return [...fake.values.keys()].filter((k) => k !== DEVICE_KEYS.workspaces && !k.startsWith(`az.ws.${OTHER}.`));
}

beforeEach(() => {
  __resetSessionForTests();
  __resetRegistryForTests();
  abortAll();
  fake.values.clear();
  jest.clearAllMocks();
  // clearAllMocks keeps implementations: put back the default after a test overrode it.
  fake.deleteItemAsync.mockImplementation(async (key: string) => {
    fake.values.delete(key);
  });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});

afterEach(() => {
  jest.restoreAllMocks();
});

const cacheEntries = jest.requireMock<{ __cacheEntries: { name: string; delete: jest.Mock }[] }>(
  'expo-file-system',
).__cacheEntries;

describe('endSession', () => {
  it('deletes the caches directory, where the URL cache keeps tokens and transcripts in plaintext (DEF-R2)', async () => {
    signedIn();
    const remaining = new Set(['ca.zefir.agent', 'com.apple.nsurlsessiond']);
    cacheEntries.splice(
      0,
      cacheEntries.length,
      ...[...remaining].map((name) => ({ name, delete: jest.fn(() => remaining.delete(name)) })),
    );

    const result = await endSession({ reason: 'device_revoked' });

    expect(result.errors).toEqual([]);
    expect(remaining.size).toBe(0);
    cacheEntries.splice(0);
  });

  it('reports a cache entry it could not delete and still finishes the wipe', async () => {
    signedIn();
    const kept = { name: 'locked', delete: jest.fn(() => { throw new Error('busy'); }) };
    const gone = { name: 'ca.zefir.agent', delete: jest.fn() };
    cacheEntries.splice(0, cacheEntries.length, kept, gone);

    const result = await endSession({ reason: 'user_logout' });

    expect(result.errors.map((e) => e.step)).toEqual(['clear_caches']);
    expect(gone.delete).toHaveBeenCalled();
    expect(result.storageWiped).toBe(true);
    expect(getBearerToken()).toBeNull();
    cacheEntries.splice(0);
  });

  it('wipes every registered key, aborts in-flight work and resets the session', async () => {
    signedIn();
    const controller = new AbortController();
    registerAbortable(controller);

    const result = await endSession({ reason: 'user_logout' });

    expect(result.errors).toEqual([]);
    expect(controller.signal.aborted).toBe(true);
    expect(inFlightCount()).toBe(0);
    for (const key of REGISTERED_KEYS) expect(fake.values.has(wsKey(key))).toBe(false);
    const session = getSession();
    expect(session.serverOrigin).toBeNull();
    expect(session.credential).toBeNull();
    expect(session.projectId).toBeNull();
    expect(session.ending).toBe(false);
    expect(session.hydrated).toBe(true);
    expect(session.endReason).toBe('user_logout');
    expect(getBearerToken()).toBeNull();
  });

  it('writes the pending marker first and removes it last', async () => {
    signedIn();
    await endSession({ reason: 'user_logout' });
    const writes = fake.setItemAsync.mock.calls.map((c) => c[0]);
    expect(writes).toEqual([wsKey(SECURE_KEYS.wipePending), DEVICE_KEYS.workspaces]);
    expect(JSON.parse(fake.setItemAsync.mock.calls[0][1])).toMatchObject({ reason: 'user_logout' });
    const deletes = fake.deleteItemAsync.mock.calls.map((c) => c[0]);
    expect(deletes[deletes.length - 1]).toBe(wsKey(SECURE_KEYS.wipePending));
    expect(fake.setItemAsync.mock.invocationCallOrder[0]).toBeLessThan(
      Math.min(...fake.deleteItemAsync.mock.invocationCallOrder),
    );
  });

  it('runs once for concurrent callers and returns the same promise', async () => {
    signedIn();
    const hook = jest.fn();
    const unregister = registerWipeHook('probe', hook);
    const calls = Array.from({ length: 10 }, () => endSession({ reason: 'device_revoked' }));
    expect(new Set(calls).size).toBe(1);
    await Promise.all(calls);
    expect(hook).toHaveBeenCalledTimes(1);
    unregister();
  });

  it('keeps going when a step fails and reports the failure', async () => {
    signedIn();
    const unregister = registerWipeHook('broken', () => {
      throw new Error('cache exploded');
    });
    fake.deleteItemAsync.mockImplementationOnce(async () => {
      throw new Error('keychain busy');
    });

    const result = await endSession({ reason: 'user_logout' });

    expect(result.errors.map((e) => e.step)).toEqual(
      expect.arrayContaining(['hook:broken', expect.stringMatching(/^delete:az\./)]),
    );
    expect(getSession().serverOrigin).toBeNull();
    expect(getBearerToken()).toBeNull();
    // A secure delete failed, so the wipe is unfinished: the marker stays.
    expect(fake.values.has(wsKey(SECURE_KEYS.wipePending))).toBe(true);
    unregister();
  });

  it('keeps the pending marker while any secure delete failed, so the next launch retries the wipe', async () => {
    signedIn();
    fake.deleteItemAsync.mockImplementation(async (key: string) => {
      if (key === wsKey(SECURE_KEYS.pat)) throw new Error('keystore error');
      fake.values.delete(key);
    });

    const result = await endSession({ reason: 'user_logout' });

    expect(result.errors.map((e) => e.step)).toContain(`delete:${SECURE_KEYS.pat}`);
    expect(fake.values.get(wsKey(SECURE_KEYS.pat))).toBe('stored');
    // Still registered, so the next launch finds the marker and wipes again.
    expect(JSON.parse(fake.values.get(DEVICE_KEYS.workspaces)!).ids).toContain(TEST_WORKSPACE);
    expect(fake.values.has(wsKey(SECURE_KEYS.wipePending))).toBe(true);
    // The in-memory session is still reset: the user is signed out now.
    expect(getBearerToken()).toBeNull();
  });

  it('clears the pending marker when only a non-storage step failed', async () => {
    signedIn();
    const unregister = registerWipeHook('broken', () => {
      throw new Error('cache exploded');
    });
    const result = await endSession({ reason: 'user_logout' });
    expect(result.errors.map((e) => e.step)).toEqual(['hook:broken']);
    expect(onlyOtherLeft()).toEqual([]);
    unregister();
  });

  it('keeps a non-secret origin hint after credential_rejected only', async () => {
    signedIn();
    await endSession({ reason: 'credential_rejected' });
    expect(getSession().lastOriginHint).toBe('https://ai.example.com');

    signedIn();
    await endSession({ reason: 'device_revoked' });
    expect(getSession().lastOriginHint).toBeNull();
  });

  it('shows the ending state while running', async () => {
    signedIn();
    const seen: boolean[] = [];
    const unregister = registerWipeHook('observe', () => {
      seen.push(getSession().ending);
    });
    await endSession({ reason: 'user_logout' });
    expect(seen).toEqual([true]);
    expect(getSession().ending).toBe(false);
    unregister();
  });

  it('runs the steps in order: marker, ending, abort, revoke, hooks, keys, reset, marker removal', async () => {
    signedIn();
    const order: string[] = [];
    fake.setItemAsync.mockImplementation(async (key: string, value: string) => {
      order.push(`set:${key === wsKey(SECURE_KEYS.wipePending) ? SECURE_KEYS.wipePending : key}`);
      fake.values.set(key, value);
    });
    fake.deleteItemAsync.mockImplementation(async (key: string) => {
      order.push(key === wsKey(SECURE_KEYS.wipePending) ? 'delete:marker' : 'delete:key');
      fake.values.delete(key);
    });
    const controller = new AbortController();
    controller.signal.addEventListener('abort', () => order.push('abort'));
    registerAbortable(controller);
    const unregisterRevoke = registerRevokeHook(async () => {
      order.push(`revoke:token=${getBearerToken()}:ending=${getSession().ending}`);
    });
    const unregisterHook = registerWipeHook('probe', () => {
      order.push('hook');
    });

    await endSession({ reason: 'user_logout' });

    const compact = order.filter((entry, i) => entry !== 'delete:key' || order[i - 1] !== 'delete:key');
    expect(compact).toEqual([
      `set:${SECURE_KEYS.wipePending}`,
      'abort',
      'revoke:token=tok:ending=true',
      'hook',
      'delete:key',
      `set:${DEVICE_KEYS.workspaces}`,
      'delete:marker',
    ]);
    expect(getSession().serverOrigin).toBeNull();
    unregisterRevoke();
    unregisterHook();
  });

  it('caps the server revoke and carries on with the wipe', async () => {
    jest.useFakeTimers();
    try {
      signedIn();
      let revokeSignal: AbortSignal | null = null;
      const unregister = registerRevokeHook((signal) => {
        revokeSignal = signal;
        return new Promise(() => {}); // never answers
      });
      const done = endSession({ reason: 'user_logout' });
      await jest.advanceTimersByTimeAsync(REVOKE_TIMEOUT_MS);
      const result = await done;
      expect(result.errors.map((e) => e.step)).toEqual(['server_revoke']);
      expect(revokeSignal!.aborted).toBe(true);
      for (const key of REGISTERED_KEYS) expect(fake.values.has(wsKey(key))).toBe(false);
      unregister();
    } finally {
      jest.useRealTimers();
    }
  });

  it('does not call the server revoke when the server already revoked the device', async () => {
    signedIn();
    const revoke = jest.fn(async () => {});
    const unregister = registerRevokeHook(revoke);
    await endSession({ reason: 'device_revoked' });
    expect(revoke).not.toHaveBeenCalled();
    unregister();
  });
});

describe('the other connected workspaces', () => {
  it('ends only the active workspace: the others keep every item and stay registered', async () => {
    signedIn();
    const result = await endSession({ reason: 'device_revoked' });
    expect(result.storageWiped).toBe(true);
    expect(onlyOtherLeft()).toEqual([]);
    for (const key of REGISTERED_KEYS) {
      if (key !== SECURE_KEYS.wipePending) expect(fake.values.get(wsKey(key, OTHER))).toBe('other workspace');
    }
    expect(JSON.parse(fake.values.get(DEVICE_KEYS.workspaces)!)).toEqual({ v: 1, active: null, ids: [OTHER] });
    expect(boundWorkspace()).toBeNull();
    expect(getSession()).toMatchObject({ endReason: 'device_revoked', endedOrigin: 'https://ai.example.com' });
  });

  it("deletes the active workspace's files through the stored-wipe hooks", async () => {
    signedIn();
    const removed: string[] = [];
    const unregister = registerStoredWipeHook('probe', (id) => {
      removed.push(id);
    });
    await endSession({ reason: 'user_logout' });
    expect(removed).toEqual([TEST_WORKSPACE]);
    unregister();
  });
});

describe('wipeStoredWorkspace (a workspace that is not active)', () => {
  it('revokes, deletes its files and items and forgets it, leaving the active session alone', async () => {
    signedIn();
    const removed: string[] = [];
    const unregister = registerStoredWipeHook('probe', (id) => {
      removed.push(id);
    });
    const revoke = jest.fn(async () => {});
    const result = await wipeStoredWorkspace(OTHER, { reason: 'user_logout', revoke });
    expect(result).toEqual({ wiped: true, errors: [] });
    expect(revoke).toHaveBeenCalled();
    expect(removed).toEqual([OTHER]);
    expect([...fake.values.keys()].some((k) => k.startsWith(`az.ws.${OTHER}.`))).toBe(false);
    expect(fake.values.get(wsKey(SECURE_KEYS.pat))).toBe('stored');
    expect(JSON.parse(fake.values.get(DEVICE_KEYS.workspaces)!).ids).toEqual([TEST_WORKSPACE]);
    expect(getBearerToken()).toBe('tok');
    expect(boundWorkspace()).toBe(TEST_WORKSPACE);
    unregister();
  });

  it('keeps the marker and the registry entry when an item could not be deleted', async () => {
    signedIn();
    fake.deleteItemAsync.mockImplementation(async (key: string) => {
      if (key === wsKey(SECURE_KEYS.pat, OTHER)) throw new Error('keystore error');
      fake.values.delete(key);
    });
    const result = await wipeStoredWorkspace(OTHER, { reason: 'user_logout' });
    expect(result.wiped).toBe(false);
    expect(fake.values.has(wsKey(SECURE_KEYS.wipePending, OTHER))).toBe(true);
    expect(JSON.parse(fake.values.get(DEVICE_KEYS.workspaces)!).ids).toContain(OTHER);
  });
});

describe('resumePendingWipes', () => {
  it('does nothing without a pending marker', async () => {
    signedIn();
    await expect(resumePendingWipes()).resolves.toEqual([]);
    expect(fake.values.has(wsKey(SECURE_KEYS.pat))).toBe(true);
  });

  it('re-runs an interrupted wipe with the recorded reason, for that workspace only', async () => {
    signedIn();
    fake.values.set(wsKey(SECURE_KEYS.wipePending), JSON.stringify({ reason: 'credential_rejected', startedAt: 1 }));
    await expect(resumePendingWipes()).resolves.toEqual([
      { workspaceId: TEST_WORKSPACE, reason: 'credential_rejected', wiped: true },
    ]);
    expect(onlyOtherLeft()).toEqual([]);
    expect(fake.values.get(wsKey(SECURE_KEYS.pat, OTHER))).toBe('other workspace');
  });

  it('reports a resumed wipe that still cannot delete a key, keeping the marker', async () => {
    signedIn();
    fake.values.set(wsKey(SECURE_KEYS.wipePending), JSON.stringify({ reason: 'user_logout', startedAt: 1 }));
    fake.deleteItemAsync.mockImplementation(async (key: string) => {
      if (key === wsKey(SECURE_KEYS.pat)) throw new Error('keystore error');
      fake.values.delete(key);
    });
    await expect(resumePendingWipes()).resolves.toEqual([
      { workspaceId: TEST_WORKSPACE, reason: 'user_logout', wiped: false },
    ]);
    expect(fake.values.has(wsKey(SECURE_KEYS.wipePending))).toBe(true);
  });

  it('still wipes when the marker is unreadable JSON', async () => {
    signedIn();
    fake.values.set(wsKey(SECURE_KEYS.wipePending), 'garbage{');
    const [resumed] = await resumePendingWipes();
    expect(resumed).toMatchObject({ reason: 'reset', wiped: true });
    expect(onlyOtherLeft()).toEqual([]);
  });

  it('rejects when a marker cannot be read (the caller retries once unlocked)', async () => {
    signedIn();
    fake.getItemAsync.mockImplementationOnce(async () => {
      throw new Error('interaction not allowed');
    });
    await expect(resumePendingWipes()).rejects.toThrow();
  });
});

// WS-4 (b): the slot's meta names its database file (the migrated slot keeps
// az-offline.db). Deleted before the file, a retried wipe guessed the name
// and reported success with the real file still on disk.
describe('a database file that could not be deleted', () => {
  it('keeps the workspace meta for the retry (endSession)', async () => {
    signedIn();
    const unregister = registerStoredWipeHook('probe', () => {
      throw new Error('file busy');
    });
    const result = await endSession({ reason: 'user_logout' });
    unregister();
    expect(result.storageWiped).toBe(false);
    expect(fake.values.get(wsKey(SECURE_KEYS.workspaceMeta))).toBe('stored');
    expect(fake.values.has(wsKey(SECURE_KEYS.wipePending))).toBe(true);
    expect(fake.values.has(wsKey(SECURE_KEYS.refreshToken))).toBe(false);
  });

  it('keeps the workspace meta for the retry (wipeStoredWorkspace)', async () => {
    signedIn();
    const unregister = registerStoredWipeHook('probe', () => {
      throw new Error('file busy');
    });
    const result = await wipeStoredWorkspace(OTHER, { reason: 'user_logout' });
    unregister();
    expect(result.wiped).toBe(false);
    expect(fake.values.get(wsKey(SECURE_KEYS.workspaceMeta, OTHER))).toBe('other workspace');
    expect(fake.values.has(wsKey(SECURE_KEYS.refreshToken, OTHER))).toBe(false);
  });
});
