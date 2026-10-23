/**
 * The app's store singleton follows the workspaces: every endSession
 * (logout, device_revoked, …) deletes the ACTIVE workspace's file and key,
 * removing a workspace that is not active deletes its own, and no other
 * workspace's file is touched.
 */
import { offlineStore } from '@/db/offlineStore';
import { endSession, wipeStoredWorkspace } from '@/security/endSession';
import { DEVICE_KEYS, SECURE_KEYS, bindWorkspace, getSecureItemIn } from '@/security/secureStorage';
import { LEGACY_DB_NAME, dbNameFor, migrateLegacyLayout } from '@/security/workspaceRegistry';
import { __resetSessionForTests, setSession } from '@/state/session';
import { TEST_WORKSPACE, type FakeSecureStore } from '@/test/fakeSecureStore';
import { nodeSqliteDriver, tempDir } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const mockDriver = nodeSqliteDriver(tempDir(), { cipher: true });
jest.mock('@/db/expoDriver', () => ({
  openExpoConnection: (name: string) => mockDriver.open(name),
  deleteExpoDatabase: (name: string) => mockDriver.remove(name),
}));

const OTHER = 'bbbbbbbbbbbb';
const A = 'https://ai.example.com';
const B = 'https://other.example.com';

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: A, workspaceId: TEST_WORKSPACE, credential: { kind: 'oauth' } });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

/** Opens (and closes) the other workspace's store once, so its file and key exist. */
async function otherWorkspaceHasAStore() {
  bindWorkspace(OTHER);
  expect(await offlineStore.open(B, OTHER)).not.toBeNull();
  await offlineStore.close();
  bindWorkspace(TEST_WORKSPACE);
}

it.each(['user_logout', 'device_revoked', 'server_switch'] as const)(
  "endSession(%s) deletes the active workspace's store and key, and only those",
  async (reason) => {
    offlineStore.setKeyGate(() => true);
    await otherWorkspaceHasAStore();
    expect(await offlineStore.open(A, TEST_WORKSPACE)).not.toBeNull();
    expect(mockDriver.exists(dbNameFor(TEST_WORKSPACE))).toBe(true);
    expect(await getSecureItemIn(TEST_WORKSPACE, SECURE_KEYS.dbKey)).toMatch(/^[0-9a-f]{64}$/);

    const result = await endSession({ reason });

    expect(result.errors).toEqual([]);
    expect(offlineStore.get()).toBeNull();
    expect(mockDriver.exists(dbNameFor(TEST_WORKSPACE))).toBe(false);
    expect(await getSecureItemIn(TEST_WORKSPACE, SECURE_KEYS.dbKey)).toBeNull();
    expect(mockDriver.exists(dbNameFor(OTHER))).toBe(true);
    expect(await getSecureItemIn(OTHER, SECURE_KEYS.dbKey)).toMatch(/^[0-9a-f]{64}$/);
    bindWorkspace(OTHER);
    await offlineStore.wipe(OTHER);
  },
);

it('removing a workspace that is not active deletes its store and leaves the open one alone', async () => {
  offlineStore.setKeyGate(() => true);
  bindWorkspace(TEST_WORKSPACE);
  await otherWorkspaceHasAStore();
  const db = await offlineStore.open(A, TEST_WORKSPACE);
  expect(db).not.toBeNull();

  const result = await wipeStoredWorkspace(OTHER, { reason: 'user_logout' });

  expect(result.errors).toEqual([]);
  expect(result.wiped).toBe(true);
  expect(mockDriver.exists(dbNameFor(OTHER))).toBe(false);
  expect(await getSecureItemIn(OTHER, SECURE_KEYS.dbKey)).toBeNull();
  expect(offlineStore.get()).toBe(db);
  expect(offlineStore.openWorkspace).toBe(TEST_WORKSPACE);
  await offlineStore.wipe(TEST_WORKSPACE);
});

it('never opens a file for a workspace that is not the bound one', async () => {
  offlineStore.setKeyGate(() => true);
  bindWorkspace(OTHER);
  expect(await offlineStore.open(A, TEST_WORKSPACE)).toBeNull();
  expect(mockDriver.exists(dbNameFor(TEST_WORKSPACE))).toBe(false);
});

// WS-4 (a): an unfinished wipe of the single-workspace layout is discarded at
// upgrade; its database file goes too (the old wipe hook used to delete it).
it('a discarded single-workspace migration deletes the old database file', async () => {
  const raw = await mockDriver.open(LEGACY_DB_NAME);
  await raw.close();
  expect(mockDriver.exists(LEGACY_DB_NAME)).toBe(true);
  const fake = jest.requireMock<FakeSecureStore>('expo-secure-store');
  fake.values.delete(DEVICE_KEYS.workspaces);
  fake.values.set(SECURE_KEYS.serverOrigin, A);
  fake.values.set(SECURE_KEYS.wipePending, JSON.stringify({ reason: 'user_logout' }));
  await expect(migrateLegacyLayout()).resolves.toBe('discarded');
  expect(mockDriver.exists(LEGACY_DB_NAME)).toBe(false);
});
