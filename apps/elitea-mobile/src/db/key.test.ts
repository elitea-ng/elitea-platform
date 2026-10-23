import { DB_KEY_RE, createDbKey, deleteDbKey, newDbKey, readDbKey } from '@/db/key';
import { SECURE_KEYS, getSecureItemIn, setSecureItemIn } from '@/security/secureStorage';

import { TEST_WORKSPACE } from '@/test/fakeSecureStore';
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

  it("is stored under the workspace's registered secure key and read back", async () => {
    const key = await createDbKey(TEST_WORKSPACE);
    expect(await getSecureItemIn(TEST_WORKSPACE, SECURE_KEYS.dbKey)).toBe(key);
    expect(await readDbKey(TEST_WORKSPACE)).toBe(key);
    // Another workspace has its own key.
    expect(await readDbKey('bbbbbbbbbbbb')).toBeNull();
    await deleteDbKey(TEST_WORKSPACE);
    expect(await readDbKey(TEST_WORKSPACE)).toBeNull();
  });

  it('treats a malformed stored value as no key', async () => {
    await setSecureItemIn(TEST_WORKSPACE, SECURE_KEYS.dbKey, "abc'; DROP TABLE x; --");
    expect(await readDbKey(TEST_WORKSPACE)).toBeNull();
  });
});
