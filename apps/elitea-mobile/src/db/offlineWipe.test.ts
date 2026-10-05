/**
 * The app's store singleton is wiped by every endSession (logout,
 * device_revoked, server switch): file and key gone.
 */
import { DB_NAME, offlineStore } from '@/db/offlineStore';
import { endSession } from '@/security/endSession';
import { SECURE_KEYS, getSecureItem } from '@/security/secureStorage';
import { __resetSessionForTests, setSession } from '@/state/session';
import { nodeSqliteDriver, tempDir } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const mockDriver = nodeSqliteDriver(tempDir(), { cipher: true });
jest.mock('@/db/expoDriver', () => ({
  openExpoConnection: (name: string) => mockDriver.open(name),
  deleteExpoDatabase: (name: string) => mockDriver.remove(name),
}));

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: 'https://ai.example.com', credential: { kind: 'oauth' } });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

it.each(['user_logout', 'device_revoked', 'server_switch'] as const)('endSession(%s) deletes the store and its key', async (reason) => {
  offlineStore.setKeyGate(() => true);
  expect(await offlineStore.open('https://ai.example.com')).not.toBeNull();
  expect(mockDriver.exists(DB_NAME)).toBe(true);
  expect(await getSecureItem(SECURE_KEYS.dbKey)).toMatch(/^[0-9a-f]{64}$/);

  const result = await endSession({ reason });

  expect(result.errors).toEqual([]);
  expect(offlineStore.get()).toBeNull();
  expect(mockDriver.exists(DB_NAME)).toBe(false);
  expect(await getSecureItem(SECURE_KEYS.dbKey)).toBeNull();
});
