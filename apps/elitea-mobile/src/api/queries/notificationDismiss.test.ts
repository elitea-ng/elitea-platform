import { QueryClient } from '@tanstack/react-query';

import {
  SETTINGS_CAPTURE,
  notificationInvalidId,
  notificationNotFound,
  notificationsDeltaAfterDelete,
  notificationsDeltaFull,
  notificationsPage,
} from '@/api/__fixtures__/settings13';
import { queryKeys } from '@/api/queries/keys';
import { deleteNotification, dropFromCache, forgetNotification, type Notification } from '@/api/queries/notifications';
import { migrate } from '@/db/migrations';
import { Db } from '@/db/sql';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { conversationDelta } from '@/sync/__fixtures__/wp6';
import { SyncManager } from '@/sync/syncManager';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';
import { openNodeSqlite } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const P = SETTINGS_CAPTURE.projectId;
const ONE = (id: number) => `/notifications/notification/prompt_lib/${P}/${id}`;
const LIST = `/notifications/notifications/prompt_lib/${P}`;

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'oauth', token: 'elnat_test' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('deleteNotification (client contract 1.3, captured answers)', () => {
  it('a 204 is a dismissal', async () => {
    const server = fakeServer();
    server.on('DELETE', ONE(8), { status: 204 });
    await expect(deleteNotification(P, 8, server.client)).resolves.toBe(true);
    expect(server.requests.map((r) => `${r.method} ${r.url.pathname}`)).toEqual([`DELETE /api/v2${ONE(8)}`]);
  });

  it('an id that is already gone (400 "Notification is not found") counts as dismissed', async () => {
    const server = fakeServer();
    server.on('DELETE', ONE(8), { status: 400, body: notificationNotFound });
    await expect(deleteNotification(P, 8, server.client)).resolves.toBe(true);
  });

  it('any other refusal or no answer keeps the row', async () => {
    const server = fakeServer();
    server.on('DELETE', ONE(8), { status: 400, body: notificationInvalidId }, { status: 403, body: {} }, 'network_error');
    await expect(deleteNotification(P, 8, server.client)).rejects.toMatchObject({ kind: 'http', status: 400 });
    await expect(deleteNotification(P, 8, server.client)).rejects.toMatchObject({ kind: 'http', status: 403 });
    await expect(deleteNotification(P, 8, server.client)).rejects.toMatchObject({ kind: 'network' });
  });

  it('refuses an id that is not a positive integer before any request', async () => {
    const server = fakeServer();
    await expect(deleteNotification(P, 0, server.client)).rejects.toThrow();
    await expect(deleteNotification(P, 1.5, server.client)).rejects.toThrow();
    expect(server.requests).toHaveLength(0);
  });
});

describe('a dismissed notification does not come back', () => {
  it('leaves the offline copy at once, and the tombstone keeps it out after the next sync', async () => {
    const db = new Db(openNodeSqlite());
    await migrate(db);
    const server = fakeServer();
    const manager = new SyncManager({ getDb: () => db, client: () => server.client });
    server.on('GET', `/elitea_core/conversations/prompt_lib/${P}`, { status: 200, body: conversationDelta([]) });
    server.on('GET', LIST, { status: 200, body: notificationsDeltaFull }, { status: 200, body: notificationsDeltaAfterDelete });
    const ids = async () => (await db.all<{ id: number }>('SELECT id FROM notifications ORDER BY id')).map((r) => r.id);

    await manager.syncProject(P);
    expect(await ids()).toEqual([8, 9, 10]);

    // Swipe on 8: the server delete succeeded, the row leaves this device now.
    await forgetNotification(db, P, 8);
    expect(await ids()).toEqual([9, 10]);

    // The next delta carries tombstones for 8 (this device) and 9 (another device).
    await manager.syncProject(P);
    expect(await ids()).toEqual([10]);
    const cursors = server.requests.filter((r) => r.url.pathname === `/api/v2${LIST}`).map((r) => r.url.searchParams.get('changes_since'));
    expect(cursors).toEqual(['0', notificationsDeltaFull.next_cursor]);
  });

  it('drops the row from every cached list of the project', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity } } });
    const key = [...queryKeys.notifications(P), 'list'];
    client.setQueryData(key, notificationsPage.rows as unknown as Notification[]);
    await dropFromCache(client, P, 9);
    expect((client.getQueryData(key) as Notification[]).map((n) => n.id)).toEqual([10, 8]);
    client.clear();
  });

  // F5: a refetch the server answered before the DELETE must not land after the
  // cache edit and bring the dismissed row back.
  it('a list refetch already in flight does not bring the dismissed row back', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity, retry: false } } });
    const key = [...queryKeys.notifications(P), 'list'];
    const rows = notificationsPage.rows as unknown as Notification[];
    client.setQueryData(key, rows);
    let answer: (rows: Notification[]) => void = () => {};
    const inFlight = client.fetchQuery({ queryKey: key, queryFn: () => new Promise<Notification[]>((resolve) => (answer = resolve)), staleTime: 0 }).catch(() => null);
    await dropFromCache(client, P, 9);
    answer(rows); // the pre-delete answer arrives late
    await inFlight;
    expect((client.getQueryData(key) as Notification[]).map((n) => n.id)).toEqual([10, 8]);
    client.clear();
  });

  it('forgetting with the offline store closed is a no-op', async () => {
    await expect(forgetNotification(null, P, 8)).resolves.toBeUndefined();
  });
});
