import { DEVICE_KEYS, SECURE_KEYS } from '@/security/secureStorage';
import {
  LEGACY_DB_NAME,
  MIGRATED_WORKSPACE_ID,
  __resetRegistryForTests,
  migrateLegacyLayout,
  workspaceDbName,
} from '@/security/workspaceRegistry';
import { wsKey, type FakeSecureStore } from '@/test/fakeSecureStore';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');

beforeEach(() => {
  __resetRegistryForTests();
  fake.values.clear();
  jest.clearAllMocks();
  fake.getItemAsync.mockImplementation(async (key: string) => fake.values.get(key) ?? null);
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

// WS-4 (a): the old layout's wipe used to delete az-offline.db through the
// offline store's wipe hook; a discarded migration must do the same.
describe('migrateLegacyLayout', () => {
  it('deletes the old database file when an unfinished wipe is discarded', async () => {
    fake.values.set(SECURE_KEYS.serverOrigin, 'https://ai.example.com');
    fake.values.set(SECURE_KEYS.wipePending, JSON.stringify({ reason: 'user_logout' }));
    const removeLegacyDatabase = jest.fn(async () => {});
    await expect(migrateLegacyLayout({ removeLegacyDatabase })).resolves.toBe('discarded');
    expect(removeLegacyDatabase).toHaveBeenCalledTimes(1);
    expect([...fake.values.keys()]).toEqual([]);
  });

  it('still discards the old items when the file cannot be deleted', async () => {
    fake.values.set(SECURE_KEYS.pat, 'tok');
    const removeLegacyDatabase = jest.fn(async () => {
      throw new Error('file busy');
    });
    await expect(migrateLegacyLayout({ removeLegacyDatabase })).resolves.toBe('discarded');
    expect([...fake.values.keys()]).toEqual([]);
  });

  it('keeps the old database file for a migrated slot', async () => {
    fake.values.set(SECURE_KEYS.serverOrigin, 'https://ai.example.com');
    const removeLegacyDatabase = jest.fn(async () => {});
    await expect(migrateLegacyLayout({ removeLegacyDatabase })).resolves.toBe('migrated');
    expect(removeLegacyDatabase).not.toHaveBeenCalled();
    expect(fake.values.has(DEVICE_KEYS.workspaces)).toBe(true);
    await expect(workspaceDbName(MIGRATED_WORKSPACE_ID)).resolves.toBe(LEGACY_DB_NAME);
  });
});

// WS-4 (c): a guessed name opens (and fills) a second, empty database.
describe('workspaceDbName', () => {
  it('rejects when the meta cannot be read instead of guessing the file name', async () => {
    fake.getItemAsync.mockImplementation(async (key: string) => {
      if (key === wsKey(SECURE_KEYS.workspaceMeta, MIGRATED_WORKSPACE_ID)) throw new Error('keychain busy');
      return fake.values.get(key) ?? null;
    });
    await expect(workspaceDbName(MIGRATED_WORKSPACE_ID)).rejects.toThrow('keychain busy');
  });
});
