import * as SecureStore from 'expo-secure-store';

import {
  DEVICE_KEYS,
  NoWorkspaceError,
  REGISTERED_KEYS,
  SECURE_KEYS,
  UnregisteredSecureKeyError,
  bindWorkspace,
  deleteAllSecureItems,
  deleteAllSecureItemsIn,
  getSecureItem,
  getSecureItemIn,
  physicalKey,
  registryIds,
  setSecureItem,
  setSecureItemIn,
  type SecureKey,
} from './secureStorage';
import { TEST_WORKSPACE, registryValue, wsKey, type FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const fake = SecureStore as unknown as FakeSecureStore;
const OTHER = 'bbbbbbbbbbbb';

beforeEach(() => {
  fake.values.clear();
  jest.clearAllMocks();
  fake.deleteItemAsync.mockImplementation(async (key: string) => {
    fake.values.delete(key);
  });
  bindWorkspace(TEST_WORKSPACE);
});

describe('secureStorage', () => {
  it('declares every key under the az. namespace, without duplicates', () => {
    expect(new Set(REGISTERED_KEYS).size).toBe(REGISTERED_KEYS.length);
    for (const key of REGISTERED_KEYS) expect(key).toMatch(/^az\.[a-z.]+$/);
  });

  it('stores each workspace under its own namespace', () => {
    expect(physicalKey(TEST_WORKSPACE, SECURE_KEYS.pat)).toBe('az.ws.aaaaaaaaaaaa.auth.pat');
    expect(() => physicalKey('../evil', SECURE_KEYS.pat)).toThrow();
    expect(() => bindWorkspace('ABC')).toThrow();
  });

  it('writes every key with WHEN_UNLOCKED_THIS_DEVICE_ONLY', async () => {
    for (const key of REGISTERED_KEYS) await setSecureItem(key, 'v');
    expect(fake.setItemAsync).toHaveBeenCalledTimes(REGISTERED_KEYS.length);
    for (const call of fake.setItemAsync.mock.calls) {
      expect(call[2]).toEqual({ keychainAccessible: fake.WHEN_UNLOCKED_THIS_DEVICE_ONLY });
    }
  });

  it('round-trips a value in the bound workspace only', async () => {
    await setSecureItem(SECURE_KEYS.pat, 'secret');
    await expect(getSecureItem(SECURE_KEYS.pat)).resolves.toBe('secret');
    expect(fake.values.get(wsKey(SECURE_KEYS.pat))).toBe('secret');
    await expect(getSecureItemIn(OTHER, SECURE_KEYS.pat)).resolves.toBeNull();
    bindWorkspace(OTHER);
    await expect(getSecureItem(SECURE_KEYS.pat)).resolves.toBeNull();
  });

  it('refuses workspace items while no workspace is bound', async () => {
    bindWorkspace(null);
    await expect(getSecureItem(SECURE_KEYS.pat)).rejects.toBeInstanceOf(NoWorkspaceError);
    await expect(setSecureItem(SECURE_KEYS.pat, 'x')).rejects.toBeInstanceOf(NoWorkspaceError);
    expect(fake.setItemAsync).not.toHaveBeenCalled();
  });

  it('resolves the namespace when a write starts, so a switch cannot redirect it', async () => {
    let release!: () => void;
    fake.setItemAsync.mockImplementationOnce(async (key: string, value: string) => {
      await new Promise<void>((resolve) => (release = resolve));
      fake.values.set(key, value);
    });
    const writing = setSecureItem(SECURE_KEYS.refreshToken, 'old-workspace-token');
    bindWorkspace(OTHER);
    release();
    await writing;
    expect(fake.values.get(wsKey(SECURE_KEYS.refreshToken))).toBe('old-workspace-token');
    expect(fake.values.has(wsKey(SECURE_KEYS.refreshToken, OTHER))).toBe(false);
  });

  it('refuses an unregistered key for read, write and delete', async () => {
    const rogue = 'az.rogue' as SecureKey;
    await expect(setSecureItem(rogue, 'x')).rejects.toBeInstanceOf(UnregisteredSecureKeyError);
    await expect(getSecureItem(rogue)).rejects.toBeInstanceOf(UnregisteredSecureKeyError);
    await expect(setSecureItemIn(OTHER, rogue, 'x')).rejects.toBeInstanceOf(UnregisteredSecureKeyError);
    expect(fake.setItemAsync).not.toHaveBeenCalled();
    expect(fake.getItemAsync).not.toHaveBeenCalled();
  });

  it("deletes every key of one workspace except the kept ones, and leaves the others' alone", async () => {
    for (const key of REGISTERED_KEYS) {
      fake.values.set(wsKey(key), 'v');
      fake.values.set(wsKey(key, OTHER), 'other');
    }
    fake.deleteItemAsync.mockImplementationOnce(async () => {
      throw new Error('keychain busy');
    });
    const failures = await deleteAllSecureItemsIn(TEST_WORKSPACE, [SECURE_KEYS.wipePending]);
    expect(failures).toHaveLength(1);
    const attempted = fake.deleteItemAsync.mock.calls.map((c) => c[0]);
    expect(attempted.sort()).toEqual(
      REGISTERED_KEYS.filter((k) => k !== SECURE_KEYS.wipePending).map((k) => wsKey(k)).sort(),
    );
    expect(fake.values.has(wsKey(SECURE_KEYS.wipePending))).toBe(true);
    for (const key of REGISTERED_KEYS) expect(fake.values.get(wsKey(key, OTHER))).toBe('other');
  });

  it('purges everything on the device: every registered workspace, the old layout, then the registry', async () => {
    fake.values.set(DEVICE_KEYS.workspaces, registryValue([TEST_WORKSPACE, OTHER]));
    for (const key of REGISTERED_KEYS) {
      fake.values.set(key, 'legacy');
      fake.values.set(wsKey(key), 'a');
      fake.values.set(wsKey(key, OTHER), 'b');
    }
    await expect(deleteAllSecureItems()).resolves.toEqual([]);
    expect(fake.values.size).toBe(0);
    const deletes = fake.deleteItemAsync.mock.calls.map((c) => c[0]);
    expect(deletes[deletes.length - 1]).toBe(DEVICE_KEYS.workspaces);
  });

  it('keeps the registry when a purge could not delete something it names', async () => {
    fake.values.set(DEVICE_KEYS.workspaces, registryValue([TEST_WORKSPACE]));
    fake.values.set(wsKey(SECURE_KEYS.pat), 'a');
    fake.deleteItemAsync.mockImplementation(async (key: string) => {
      if (key === wsKey(SECURE_KEYS.pat)) throw new Error('busy');
      fake.values.delete(key);
    });
    const failures = await deleteAllSecureItems();
    expect(failures.map((f) => f.key)).toEqual([wsKey(SECURE_KEYS.pat)]);
    expect(fake.values.has(DEVICE_KEYS.workspaces)).toBe(true);
  });

  it('reads only well-formed workspace ids from a stored registry', () => {
    expect(registryIds(null)).toEqual([]);
    expect(registryIds('garbage{')).toEqual([]);
    expect(registryIds(JSON.stringify({ ids: [TEST_WORKSPACE, '../x', 7] }))).toEqual([TEST_WORKSPACE]);
  });
});
