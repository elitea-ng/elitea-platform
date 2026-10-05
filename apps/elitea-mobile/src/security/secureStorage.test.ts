import * as SecureStore from 'expo-secure-store';

import {
  REGISTERED_KEYS,
  SECURE_KEYS,
  UnregisteredSecureKeyError,
  deleteAllSecureItems,
  getSecureItem,
  setSecureItem,
  type SecureKey,
} from './secureStorage';
import type { FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const fake = SecureStore as unknown as FakeSecureStore;

beforeEach(() => {
  fake.values.clear();
  jest.clearAllMocks();
});

describe('secureStorage', () => {
  it('declares every key under the az. namespace, without duplicates', () => {
    expect(new Set(REGISTERED_KEYS).size).toBe(REGISTERED_KEYS.length);
    for (const key of REGISTERED_KEYS) expect(key).toMatch(/^az\.[a-z.]+$/);
  });

  it('writes every key with WHEN_UNLOCKED_THIS_DEVICE_ONLY', async () => {
    for (const key of REGISTERED_KEYS) await setSecureItem(key, 'v');
    expect(fake.setItemAsync).toHaveBeenCalledTimes(REGISTERED_KEYS.length);
    for (const call of fake.setItemAsync.mock.calls) {
      expect(call[2]).toEqual({ keychainAccessible: fake.WHEN_UNLOCKED_THIS_DEVICE_ONLY });
    }
  });

  it('round-trips a value', async () => {
    await setSecureItem(SECURE_KEYS.pat, 'secret');
    await expect(getSecureItem(SECURE_KEYS.pat)).resolves.toBe('secret');
  });

  it('refuses an unregistered key for read, write and delete', async () => {
    const rogue = 'az.rogue' as SecureKey;
    await expect(setSecureItem(rogue, 'x')).rejects.toBeInstanceOf(UnregisteredSecureKeyError);
    await expect(getSecureItem(rogue)).rejects.toBeInstanceOf(UnregisteredSecureKeyError);
    expect(fake.setItemAsync).not.toHaveBeenCalled();
    expect(fake.getItemAsync).not.toHaveBeenCalled();
  });

  it('deletes every registered key except the kept ones, and reports failures without throwing', async () => {
    for (const key of REGISTERED_KEYS) fake.values.set(key, 'v');
    fake.deleteItemAsync.mockImplementationOnce(async () => {
      throw new Error('keychain busy');
    });
    const failures = await deleteAllSecureItems([SECURE_KEYS.wipePending]);
    expect(failures).toHaveLength(1);
    const attempted = fake.deleteItemAsync.mock.calls.map((c) => c[0]);
    expect(attempted.sort()).toEqual(
      REGISTERED_KEYS.filter((k) => k !== SECURE_KEYS.wipePending).sort(),
    );
    expect(fake.values.has(SECURE_KEYS.wipePending)).toBe(true);
  });
});
