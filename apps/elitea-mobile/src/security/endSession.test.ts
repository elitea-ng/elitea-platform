import { abortAll, inFlightCount, registerAbortable } from '@/lib/abortRegistry';
import {
  REVOKE_TIMEOUT_MS,
  endSession,
  registerRevokeHook,
  registerWipeHook,
  resumePendingWipe,
} from '@/security/endSession';
import { REGISTERED_KEYS, SECURE_KEYS } from '@/security/secureStorage';
import {
  __resetSessionForTests,
  getBearerToken,
  getSession,
  setCredential,
  setSession,
} from '@/state/session';
import type { FakeSecureStore } from '@/test/fakeSecureStore';

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

function signedIn() {
  setSession({ hydrated: true, serverOrigin: 'https://ai.example.com', projectId: 7 });
  setCredential({ kind: 'pat', token: 'tok' });
  for (const key of REGISTERED_KEYS) fake.values.set(key, 'stored');
}

beforeEach(() => {
  __resetSessionForTests();
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
    for (const key of REGISTERED_KEYS) expect(fake.values.has(key)).toBe(false);
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
    expect(writes).toEqual([SECURE_KEYS.wipePending]);
    expect(JSON.parse(fake.setItemAsync.mock.calls[0][1])).toMatchObject({ reason: 'user_logout' });
    const deletes = fake.deleteItemAsync.mock.calls.map((c) => c[0]);
    expect(deletes[deletes.length - 1]).toBe(SECURE_KEYS.wipePending);
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
    expect(fake.values.has(SECURE_KEYS.wipePending)).toBe(true);
    unregister();
  });

  it('keeps the pending marker while any secure delete failed, so the next launch retries the wipe', async () => {
    signedIn();
    fake.deleteItemAsync.mockImplementation(async (key: string) => {
      if (key === SECURE_KEYS.pat) throw new Error('keystore error');
      fake.values.delete(key);
    });

    const result = await endSession({ reason: 'user_logout' });

    expect(result.errors.map((e) => e.step)).toContain(`delete:${SECURE_KEYS.pat}`);
    expect(fake.values.get(SECURE_KEYS.pat)).toBe('stored');
    expect(fake.values.has(SECURE_KEYS.wipePending)).toBe(true);
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
    expect(fake.values.size).toBe(0);
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
      order.push(`set:${key}`);
      fake.values.set(key, value);
    });
    fake.deleteItemAsync.mockImplementation(async (key: string) => {
      order.push(key === SECURE_KEYS.wipePending ? 'delete:marker' : 'delete:key');
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
      for (const key of REGISTERED_KEYS) expect(fake.values.has(key)).toBe(false);
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

describe('resumePendingWipe', () => {
  it('does nothing without a pending marker', async () => {
    signedIn();
    fake.values.delete(SECURE_KEYS.wipePending);
    await expect(resumePendingWipe()).resolves.toBe(false);
    expect(fake.values.has(SECURE_KEYS.pat)).toBe(true);
  });

  it('re-runs an interrupted wipe with the recorded reason', async () => {
    signedIn();
    fake.values.set(SECURE_KEYS.wipePending, JSON.stringify({ reason: 'credential_rejected', startedAt: 1 }));
    await expect(resumePendingWipe()).resolves.toBe(true);
    for (const key of REGISTERED_KEYS) expect(fake.values.has(key)).toBe(false);
    expect(getSession().endReason).toBe('credential_rejected');
  });

  it('rejects, keeping the marker, when the resumed wipe still cannot delete a key', async () => {
    signedIn();
    fake.values.set(SECURE_KEYS.wipePending, JSON.stringify({ reason: 'user_logout', startedAt: 1 }));
    fake.deleteItemAsync.mockImplementation(async (key: string) => {
      if (key === SECURE_KEYS.pat) throw new Error('keystore error');
      fake.values.delete(key);
    });
    await expect(resumePendingWipe()).rejects.toThrow(/wipe/);
    expect(fake.values.has(SECURE_KEYS.wipePending)).toBe(true);
  });

  it('still wipes when the marker is unreadable JSON', async () => {
    signedIn();
    fake.values.set(SECURE_KEYS.wipePending, 'garbage{');
    await expect(resumePendingWipe()).resolves.toBe(true);
    expect(fake.values.size).toBe(0);
    expect(getSession().endReason).toBe('reset');
  });
});
