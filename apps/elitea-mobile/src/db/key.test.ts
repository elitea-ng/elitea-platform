import { DB_KEY_RE, createDbKey, deleteDbKey, newDbKey, readDbKey } from '@/db/key';
import { SECURE_KEYS, getSecureItem, setSecureItem } from '@/security/secureStorage';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

describe('database key', () => {
  it('is 32 random bytes as 64 lowercase hex characters', () => {
    const key = newDbKey((n) => Uint8Array.from({ length: n }, (_, i) => (i * 37 + 250) & 0xff));
    expect(key).toMatch(DB_KEY_RE);
    expect(key.slice(0, 4)).toBe('fa1f');
    expect(newDbKey()).not.toBe(newDbKey());
  });

  it('refuses a short random source', () => {
    expect(() => newDbKey(() => new Uint8Array(16))).toThrow();
  });

  it('is stored under the registered secure key and read back', async () => {
    const key = await createDbKey();
    expect(await getSecureItem(SECURE_KEYS.dbKey)).toBe(key);
    expect(await readDbKey()).toBe(key);
    await deleteDbKey();
    expect(await readDbKey()).toBeNull();
  });

  it('treats a malformed stored value as no key', async () => {
    await setSecureItem(SECURE_KEYS.dbKey, "abc'; DROP TABLE x; --");
    expect(await readDbKey()).toBeNull();
  });
});
