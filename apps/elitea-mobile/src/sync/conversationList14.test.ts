import { migrate } from '@/db/migrations';
import { readPinnedConversationRows, searchConversations } from '@/db/search';
import { Db } from '@/db/sql';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { SyncManager } from '@/sync/syncManager';
import { conversationDelta, conversationRow, messageDelta, messageRow, notificationDelta, tombstone } from '@/sync/__fixtures__/wp6';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';
import { openNodeSqlite } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const CONV = '/elitea_core/conversations/prompt_lib/7';
const NOTIF = '/notifications/notifications/prompt_lib/7';
const msgs = (id: number) => `/elitea_core/messages/prompt_lib/7/${id}`;

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'oauth', token: 'elnat_test' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

/**
 * Issue #6 acceptance through the real sync engine (client contract 1.4): a rename or pin made on
 * the web reaches this device in the next `changes_since` delta, a delete on another device
 * removes the row here, and search (offline) finds titles and message bodies from what synced.
 */
describe('the conversation list over changes_since (contract 1.4)', () => {
  it('a rename, a pin and an unpin made elsewhere arrive with the next delta', async () => {
    const db = new Db(openNodeSqlite());
    await migrate(db);
    const server = fakeServer();
    const manager = new SyncManager({ getDb: () => db, client: () => server.client });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    server.on('GET', msgs(41), { status: 200, body: messageDelta([messageRow(1, { conversation_id: '41', content: 'Pastéis de nata near the castle' })], { next: 'm41-1' }) });
    server.on('GET', msgs(42), { status: 200, body: messageDelta([], { next: 'm42-1' }) });
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41, { name: 'Trip' }), conversationRow(42, { is_pinned: true })], { next: 'c-1' }) });
    await manager.syncProject(7);
    expect((await readPinnedConversationRows(db, 7)).map((r) => r.id)).toEqual([42]);
    expect((await searchConversations(db, 7, 'nata')).map((h) => h.id)).toEqual([41]);

    // On the web: 41 renamed and pinned, 42 unpinned.
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41, { name: 'Lisbon in May', is_pinned: true }), conversationRow(42, { is_pinned: false })], { next: 'c-2' }) });
    server.on('GET', msgs(41), { status: 200, body: messageDelta([], { next: 'm41-2' }) });
    server.on('GET', msgs(42), { status: 200, body: messageDelta([], { next: 'm42-2' }) });
    await manager.syncProject(7);
    expect((await readPinnedConversationRows(db, 7)).map((r) => [r.id, r.name])).toEqual([[41, 'Lisbon in May']]);
    expect((await searchConversations(db, 7, 'lisbon')).map((h) => h.id)).toEqual([41]);
    expect(await searchConversations(db, 7, 'trip')).toEqual([]);

    // Deleted on another device: the tombstone removes the row and its index entries.
    server.on('GET', CONV, { status: 200, body: conversationDelta([], { tombstones: [tombstone(41)], next: 'c-3' }) });
    await manager.syncProject(7);
    expect(await readPinnedConversationRows(db, 7)).toEqual([]);
    expect(await searchConversations(db, 7, 'nata')).toEqual([]);
  });
});
