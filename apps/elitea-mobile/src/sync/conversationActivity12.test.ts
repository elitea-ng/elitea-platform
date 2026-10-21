import { QueryClient } from '@tanstack/react-query';

import { runMarkConversationRead } from '@/api/queries/markRead';
import { migrate } from '@/db/migrations';
import { readConversationRows } from '@/db/repo';
import { Db } from '@/db/sql';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { SyncManager } from '@/sync/syncManager';
import { conversationDelta, conversationRow, messageDelta, notificationDelta } from '@/sync/__fixtures__/wp6';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';
import { openNodeSqlite } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const CONV = '/elitea_core/conversations/prompt_lib/7';
const NOTIF = '/notifications/notifications/prompt_lib/7';
const READ = '/elitea_core/conversation_read/prompt_lib/7/41';
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
 * Client contract 1.12 in the offline copy: the delta's `activity`, `last_activity_at` and
 * `unread` are kept with the row (a turn transition re-delivers it), and a read made on this
 * device is written into the stored row, since a read is never re-delivered by the delta.
 */
describe('conversation activity in the offline copy (contract 1.12)', () => {
  it('keeps the activity fields a delta delivers, and the unread a read on this device clears', async () => {
    const db = new Db(openNodeSqlite());
    await migrate(db);
    const server = fakeServer();
    const manager = new SyncManager({ getDb: () => db, client: () => server.client });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    server.on('GET', msgs(41), { status: 200, body: messageDelta([], { next: 'm41-1' }) });
    const running = { activity: { state: 'running', since: '2026-10-21T10:00:00Z' }, last_activity_at: '2026-10-21T10:00:00Z', unread: false };
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41, running)], { next: 'c-1' }) });
    await manager.syncProject(7);
    expect((await readConversationRows(db, 7, 10, 0)).rows[0]).toMatchObject(running);

    // The turn finished elsewhere: the delta re-delivers the row, now unread.
    const finished = { activity: { state: 'idle', since: '2026-10-21T10:05:00Z' }, last_activity_at: '2026-10-21T10:05:00Z', unread: true };
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41, finished)], { next: 'c-2' }) });
    server.on('GET', msgs(41), { status: 200, body: messageDelta([], { next: 'm41-2' }) });
    await manager.syncProject(7);
    expect((await readConversationRows(db, 7, 10, 0)).rows[0]).toMatchObject(finished);

    // Opened on this device: the read is written into the stored row (and the loaded page).
    const queryClient = new QueryClient();
    queryClient.setQueryData(['p', 7, 'conversations'], { pages: [{ rows: [conversationRow(41, finished)], total: 1, offset: 0 }], pageParams: [0] });
    server.on('PUT', READ, { status: 200, body: { conversation_id: 41, read_at: '2026-10-21T10:06:00Z', unread: false } });
    expect(await runMarkConversationRead(7, 41, { queryClient, getDb: () => db, client: server.client })).toEqual({ kind: 'read', unread: false });
    expect((await readConversationRows(db, 7, 10, 0)).rows[0]).toMatchObject({ ...finished, unread: false, name: 'Conversation 41' });
    queryClient.clear();
  });
});
