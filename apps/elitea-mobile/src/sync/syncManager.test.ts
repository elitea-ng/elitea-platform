import { migrate } from '@/db/migrations';
import { conversationsScope, localWorkScope, messagesScope, readConversationRows, saveConversationDetail } from '@/db/repo';
import { searchConversations } from '@/db/search';
import { Db } from '@/db/sql';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { SyncManager } from '@/sync/syncManager';
import {
  conversationDelta,
  conversationRow,
  cursorExpired,
  messageDelta,
  messageRow,
  notificationDelta,
  notificationRow,
  tombstone,
} from '@/sync/__fixtures__/wp6';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';
import { openNodeSqlite } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const CONV = '/elitea_core/conversations/prompt_lib/7';
const NOTIF = '/notifications/notifications/prompt_lib/7';
const msgs = (id: number) => `/elitea_core/messages/prompt_lib/7/${id}`;

async function setup() {
  const db = new Db(openNodeSqlite());
  await migrate(db);
  const server = fakeServer();
  const onChanged = jest.fn();
  const manager = new SyncManager({ getDb: () => db, client: () => server.client, onChanged });
  return { db, server, manager, onChanged };
}

const ids = async (db: Db, sql: string) => (await db.all<{ id: number }>(sql)).map((r) => r.id);
const requested = (server: FakeServer, prefix: string) =>
  server.requests.filter((r) => r.url.pathname === `/api/v2${prefix}` && r.url.searchParams.get('source') === null).map((r) => r.url.searchParams.get('changes_since'));

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'oauth', token: 'elnat_test' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('SyncManager.syncProject', () => {
  it('syncs the list, the changed transcripts and the notifications', async () => {
    const { db, server, manager, onChanged } = await setup();
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41), conversationRow(42)], { next: 'conv-1' }) });
    server.on('GET', msgs(41), { status: 200, body: messageDelta([messageRow(1)], { next: 'm41-1' }) });
    server.on('GET', msgs(42), { status: 200, body: messageDelta([messageRow(3), messageRow(4)], { next: 'm42-1' }) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([notificationRow(9)], { next: 'n-1' }) });

    expect(await manager.syncProject(7)).toEqual({ conversations: 'ok', transcripts: 2, notifications: 'ok', dropped: false });
    expect(await ids(db, 'SELECT id FROM conversations ORDER BY id')).toEqual([41, 42]);
    expect(await ids(db, 'SELECT id FROM messages ORDER BY id')).toEqual([1, 3, 4]);
    expect(await ids(db, 'SELECT id FROM notifications')).toEqual([9]);
    expect(await db.get('SELECT cursor FROM sync_state WHERE scope = ?', [conversationsScope(7)])).toEqual({ cursor: 'conv-1' });
    expect(await db.get('SELECT cursor FROM sync_state WHERE scope = ?', [messagesScope(7, 42)])).toEqual({ cursor: 'm42-1' });
    expect(onChanged).toHaveBeenCalledWith(7);
  });

  it('re-reads a transcript whenever its conversation row is delivered, from the transcript cursor', async () => {
    const { server, manager } = await setup();
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(42)], { next: 'conv-1' }) });
    server.on('GET', msgs(42), { status: 200, body: messageDelta([messageRow(3)], { next: 'm42-1' }) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    await manager.syncProject(7);

    // The row again: the server re-delivers a conversation whenever one of
    // its message groups changed (trigger bump), and the row itself can be
    // byte-identical. The transcript continues from its cursor.
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(42)], { next: 'conv-2' }) });
    server.on('GET', msgs(42), { status: 200, body: messageDelta([], { next: 'm42-2' }) });
    await manager.syncProject(7);
    expect(requested(server, msgs(42))).toEqual(['0', 'm42-1']);

    // Not delivered: no transcript read.
    server.on('GET', CONV, { status: 200, body: conversationDelta([], { next: 'conv-3' }) });
    await manager.syncProject(7);
    expect(requested(server, msgs(42))).toEqual(['0', 'm42-1']);
    expect(requested(server, CONV)).toEqual(['0', 'conv-1', 'conv-2']);
  });

  it('an answer that finishes streaming reaches the offline copy although the list row is unchanged', async () => {
    const { db, server, manager } = await setup();
    // Synced mid-turn (e.g. right after an outbox send was admitted).
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(42)], { next: 'conv-1' }) });
    server.on('GET', msgs(42), {
      status: 200,
      body: messageDelta([messageRow(3), messageRow(4, { content: '', is_streaming: true, task_id: 'exec-1' })], { next: 'm1' }),
    });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    await manager.syncProject(7);

    // The answer completes: chat_message_group is rewritten, which bumps the
    // conversation's sync_at but not updated_at or message_groups_count, so
    // the list delivers the SAME row.
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(42)], { next: 'conv-2' }) });
    server.on('GET', msgs(42), { status: 200, body: messageDelta([messageRow(4, { content: 'final answer' })], { next: 'm2' }) });
    await manager.syncProject(7);

    const row = await db.get<{ row_json: string }>('SELECT row_json FROM messages WHERE id = 4');
    expect(JSON.parse(row?.row_json ?? '{}')).toMatchObject({ content: 'final answer' });
    expect(JSON.parse(row?.row_json ?? '{}').is_streaming).toBeUndefined();
  });

  it('a transcript the run did not finish (has_more) stays due for the next run', async () => {
    const { db, server, manager } = await setup();
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(42)], { next: 'conv-1' }) });
    // The server keeps answering has_more with the same cursor: the run stops (stalled / page cap) unfinished.
    server.on('GET', msgs(42), { status: 200, body: messageDelta([messageRow(3)], { next: 'm-part', hasMore: true }) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    await manager.syncProject(7);
    expect(await db.get('SELECT messages_dirty FROM conversations WHERE id = 42')).toEqual({ messages_dirty: 1 });

    // Next run, the list has nothing new: the transcript still continues from its cursor and finishes.
    server.on('GET', CONV, { status: 200, body: conversationDelta([], { next: 'conv-2' }) });
    server.on('GET', msgs(42), { status: 200, body: messageDelta([messageRow(5)], { next: 'm-done' }) });
    await manager.syncProject(7);
    expect(requested(server, msgs(42)).slice(-1)).toEqual(['m-part']);
    expect(await ids(db, 'SELECT id FROM messages ORDER BY id')).toEqual([3, 5]);
    expect(await db.get('SELECT messages_dirty FROM conversations WHERE id = 42')).toEqual({ messages_dirty: 0 });
  });

  it('a conversation tombstone (deleted or access_lost) removes it with its transcript', async () => {
    const { db, server, manager } = await setup();
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41), conversationRow(42)]) });
    server.on('GET', msgs(41), { status: 200, body: messageDelta([messageRow(1)]) });
    server.on('GET', msgs(42), { status: 200, body: messageDelta([messageRow(3)]) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    await manager.syncProject(7);

    server.on('GET', CONV, {
      status: 200,
      body: conversationDelta([], { tombstones: [tombstone(41, 'access_lost'), tombstone(42, 'deleted')] }),
    });
    await manager.syncProject(7);
    expect(await ids(db, 'SELECT id FROM conversations')).toEqual([]);
    expect(await ids(db, 'SELECT id FROM messages')).toEqual([]);
    expect(await db.get('SELECT count(*) AS n FROM sync_state WHERE scope LIKE ?', ['msg:%'])).toEqual({ n: 0 });
  });

  it('message tombstones delete groups; a still-streaming group is replaced when it settles', async () => {
    const { db, server, manager } = await setup();
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(42)]) });
    server.on('GET', msgs(42), {
      status: 200,
      body: messageDelta([messageRow(3), messageRow(4, { content: '', is_streaming: true, task_id: 'exec-1' })], { next: 'm1' }),
    });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    await manager.syncProject(7);

    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(42, { message_groups_count: 1 })]) });
    server.on('GET', msgs(42), {
      status: 200,
      body: messageDelta([messageRow(4, { content: 'final answer', updated_at: '2026-10-04T12:01:00Z' })], {
        tombstones: [tombstone(3)],
        next: 'm2',
      }),
    });
    await manager.syncProject(7);
    const rows = await db.all<{ id: number; row_json: string }>('SELECT id, row_json FROM messages');
    expect(rows.map((r) => r.id)).toEqual([4]);
    expect(JSON.parse(rows[0].row_json)).toMatchObject({ content: 'final answer' });
    expect(JSON.parse(rows[0].row_json).is_streaming).toBeUndefined();
  });

  it('403 on the list: the caller lost the project, everything of it is dropped', async () => {
    const { db, server, manager } = await setup();
    await db.run("INSERT INTO conversations (project_id, id, row_json) VALUES (8, 1, '{}')");
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(42)]) });
    server.on('GET', msgs(42), { status: 200, body: messageDelta([messageRow(3)]) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([notificationRow(9)]) });
    await manager.syncProject(7);

    server.on('GET', CONV, { status: 403, body: { error: { code: 'forbidden' } } });
    const result = await manager.syncProject(7);
    expect(result).toMatchObject({ conversations: 'forbidden', dropped: true });
    for (const table of ['conversations', 'messages', 'notifications', 'sync_state']) {
      expect(await db.get(`SELECT count(*) AS n FROM ${table} WHERE project_id = 7`)).toEqual({ n: 0 });
    }
    expect(await ids(db, 'SELECT id FROM conversations WHERE project_id = 8')).toEqual([1]);
  });

  it('403 or 404 on one transcript drops that conversation only', async () => {
    const { db, server, manager } = await setup();
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41), conversationRow(42)]) });
    server.on('GET', msgs(41), { status: 404, body: { error: 'not found' } });
    server.on('GET', msgs(42), { status: 200, body: messageDelta([messageRow(3)]) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    await manager.syncProject(7);
    expect(await ids(db, 'SELECT id FROM conversations')).toEqual([42]);
  });

  it('410 on the list: full resync, and conversations it no longer returns are swept', async () => {
    const { db, server, manager } = await setup();
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41), conversationRow(42)], { next: 'old' }) });
    server.on('GET', '/elitea_core/messages/', { status: 200, body: messageDelta([]) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    await manager.syncProject(7);
    // A conversation opened from a link outside the list (thread read only) survives the sweep.
    await db.run("INSERT INTO conversations (project_id, id, uuid, detail_json, sync_gen) VALUES (7, 99, 'u', '{}', -1)");

    server.on('GET', CONV, { status: 410, body: cursorExpired }, { status: 200, body: conversationDelta([conversationRow(42)], { next: 'new' }) });
    await manager.syncProject(7);
    expect(requested(server, CONV)).toEqual(['0', 'old', '0']);
    expect(await ids(db, 'SELECT id FROM conversations ORDER BY id')).toEqual([42, 99]);
  });

  it('offline: nothing changes and no notification read is attempted', async () => {
    const { db, server, manager, onChanged } = await setup();
    server.on('GET', CONV, 'network_error');
    expect(await manager.syncProject(7)).toEqual({ conversations: 'offline', transcripts: 0, notifications: 'skipped', dropped: false });
    expect(requested(server, NOTIF)).toEqual([]);
    expect(await db.get('SELECT cursor FROM sync_state WHERE scope = ?', [conversationsScope(7)])).toEqual({ cursor: '0' });
    expect(onChanged).not.toHaveBeenCalled();
  });

  it('concurrent requests share one run, and one more run follows', async () => {
    const { server, manager } = await setup();
    server.on('GET', CONV, { status: 200, body: conversationDelta([]) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    const a = manager.syncProject(7);
    const b = manager.syncProject(7);
    expect(a).toBe(b);
    await a;
    expect(requested(server, CONV)).toHaveLength(2);
  });

  it('does nothing while the store is closed', async () => {
    const server = fakeServer();
    const manager = new SyncManager({ getDb: () => null, client: () => server.client });
    expect(await manager.syncProject(7)).toBeNull();
    manager.requestSync(7);
    expect(server.requests).toEqual([]);
  });
});

describe('several workspaces', () => {
  it('never folds a sync into a run of another workspace with the same project id', async () => {
    const a = new Db(openNodeSqlite());
    const b = new Db(openNodeSqlite());
    await migrate(a);
    await migrate(b);
    const serverA = fakeServer();
    const serverB = fakeServer();
    let releaseA!: () => void;
    const gate = new Promise<void>((resolve) => (releaseA = resolve));
    // Workspace A's server answers slowly.
    const slowA = {
      ...serverA.client,
      GET: (async (...args: unknown[]) => {
        await gate;
        return (serverA.client.GET as (...a: unknown[]) => unknown)(...args);
      }) as typeof serverA.client.GET,
    };
    let active: { db: Db; client: typeof serverA.client } = { db: a, client: slowA };
    const manager = new SyncManager({ getDb: () => active.db, client: () => active.client });
    serverA.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41)], { next: 'a-1' }) });
    serverA.on('GET', msgs(41), { status: 200, body: messageDelta([], { next: 'm41' }) });
    serverA.on('GET', NOTIF, { status: 200, body: notificationDelta([], { next: 'na' }) });
    serverB.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(77)], { next: 'b-1' }) });
    serverB.on('GET', msgs(77), { status: 200, body: messageDelta([], { next: 'm77' }) });
    serverB.on('GET', NOTIF, { status: 200, body: notificationDelta([], { next: 'nb' }) });

    const runA = manager.syncProject(7);
    // The user switches workspace while A's run is waiting for its server.
    active = { db: b, client: serverB.client };
    const runB = manager.syncProject(7);
    expect(runB).not.toBe(runA);
    expect(await runB).toMatchObject({ conversations: 'ok' });
    expect(await ids(b, 'SELECT id FROM conversations')).toEqual([77]);
    releaseA();
    await runA;
    // Each workspace's rows stay in its own database.
    expect(await ids(a, 'SELECT id FROM conversations')).toEqual([41]);
    expect(await ids(b, 'SELECT id FROM conversations')).toEqual([77]);
    expect(serverB.requests.every((r) => !r.url.pathname.includes('/41'))).toBe(true);
  });
});

// Local work threads (a desktop agent's, `source=local_work`) are a second list of the same table,
// synced under their own cursor, and never mixed into the Chats list.
describe('SyncManager: the Local work list', () => {
  const LOCAL = `${CONV}?source=local_work`;
  const cloud = (id: number, overrides: Record<string, unknown> = {}) =>
    conversationRow(id, { name: `Thread ${id}`, meta: { local_work: { folder_name: 'api' } }, workspace_executor: 'cloud', ...overrides });

  async function synced() {
    const { db, server, manager, onChanged } = await setup();
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41)], { next: 'conv-1' }) });
    server.on('GET', LOCAL, { status: 200, body: conversationDelta([cloud(61), cloud(62, { workspace_executor: 'local' })], { next: 'lw-1' }) });
    server.on('GET', msgs(41), { status: 200, body: messageDelta([], { next: 'm41' }) });
    server.on('GET', msgs(61), { status: 200, body: messageDelta([messageRow(1, { conversation_id: '61', content: 'refactor the parser' })], { next: 'm61' }) });
    server.on('GET', msgs(62), { status: 200, body: messageDelta([], { next: 'm62' }) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    await manager.syncProject(7);
    return { db, server, manager, onChanged };
  }

  it('asks for the Local work list with mine=true&source=local_work and its own cursor', async () => {
    const { db, server } = await synced();
    const local = server.requests.filter((r) => r.url.searchParams.get('source') === 'local_work');
    expect(local).toHaveLength(1);
    expect(local[0].url.searchParams.get('mine')).toBe('true');
    expect(local[0].url.searchParams.get('changes_since')).toBe('0');
    // The Chats list never carries the source filter.
    const chats = server.requests.filter((r) => r.url.pathname === `/api/v2${CONV}` && r.url.searchParams.get('source') === null);
    expect(chats).toHaveLength(1);
    expect(await db.get('SELECT cursor FROM sync_state WHERE scope = ?', [localWorkScope(7)])).toEqual({ cursor: 'lw-1' });
    expect(await db.get('SELECT cursor FROM sync_state WHERE scope = ?', [conversationsScope(7)])).toEqual({ cursor: 'conv-1' });
  });

  it('keeps the two lists apart in storage, offline reads and search', async () => {
    const { db } = await synced();
    expect((await readConversationRows(db, 7, 10, 0)).rows.map((r) => r.id)).toEqual([41]);
    const local = await readConversationRows(db, 7, 10, 0, 'local_work');
    expect(local.rows.map((r) => r.id).sort()).toEqual([61, 62]);
    expect(local.total).toBe(2);
    expect(local.rows.find((r) => r.id === 61)?.workspace_executor).toBe('cloud');
    // Search stays inside one list; the transcript of a Local work thread is synced too.
    expect((await searchConversations(db, 7, 'parser')).map((h) => h.id)).toEqual([]);
    expect((await searchConversations(db, 7, 'parser', 50, 'local_work')).map((h) => h.id)).toEqual([61]);
    expect((await searchConversations(db, 7, 'Thread', 50, 'local_work')).map((h) => h.id).sort()).toEqual([61, 62]);
  });

  it('a full resync of one list sweeps only its own rows', async () => {
    const { db, server, manager } = await synced();
    // The Local work cursor expires: it resyncs and 62 is gone; the Chats list keeps 41.
    server.on('GET', LOCAL, { status: 410, body: cursorExpired }, { status: 200, body: conversationDelta([cloud(61)], { next: 'lw-2' }) });
    server.on('GET', CONV, { status: 200, body: conversationDelta([], { next: 'conv-2' }) });
    await manager.syncProject(7);
    expect(await ids(db, 'SELECT id FROM conversations ORDER BY id')).toEqual([41, 61]);

    // And the Chats list resyncing never takes a Local work row with it.
    server.on('GET', CONV, { status: 410, body: cursorExpired }, { status: 200, body: conversationDelta([conversationRow(41)], { next: 'conv-3' }) });
    server.on('GET', LOCAL, { status: 200, body: conversationDelta([], { next: 'lw-3' }) });
    await manager.syncProject(7);
    expect(await ids(db, 'SELECT id FROM conversations ORDER BY id')).toEqual([41, 61]);
  });

  it('a handoff re-delivers the row with its new executor, and a tombstone removes it', async () => {
    const { db, server, manager } = await synced();
    server.on('GET', LOCAL, { status: 200, body: conversationDelta([cloud(62, { workspace_executor: 'cloud' })], { tombstones: [tombstone(61)], next: 'lw-2' }) });
    server.on('GET', msgs(62), { status: 200, body: messageDelta([], { next: 'm62-2' }) });
    await manager.syncProject(7);
    const local = await readConversationRows(db, 7, 10, 0, 'local_work');
    expect(local.rows.map((r) => [r.id, r.workspace_executor])).toEqual([[62, 'cloud']]);
  });

  it('a server without Local work (404/error on the filtered list) leaves the Chats list and the run alone', async () => {
    const { db, server, manager } = await setup();
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41)], { next: 'conv-1' }) });
    server.on('GET', LOCAL, { status: 500, body: {} });
    server.on('GET', msgs(41), { status: 200, body: messageDelta([], { next: 'm41' }) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    expect(await manager.syncProject(7)).toEqual({ conversations: 'ok', transcripts: 1, notifications: 'ok', dropped: false });
    expect(await ids(db, 'SELECT id FROM conversations')).toEqual([41]);
  });

  it('asks for no Local work list from a server older than client contract 1.8', async () => {
    const { db } = await setup();
    const server = fakeServer();
    const manager = new SyncManager({ getDb: () => db, client: () => server.client, localWork: () => false });
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41)], { next: 'conv-1' }) });
    server.on('GET', msgs(41), { status: 200, body: messageDelta([], { next: 'm41' }) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    await manager.syncProject(7);
    expect(server.requests.some((r) => r.url.searchParams.get('source') !== null)).toBe(false);
  });

  it('a row delivered by both deltas stays in the Local work list, with its own generation', async () => {
    const { db, server, manager } = await setup();
    server.on('GET', LOCAL, { status: 200, body: conversationDelta([cloud(61)], { next: 'lw-1' }) });
    server.on('GET', CONV, { status: 200, body: conversationDelta([conversationRow(41), conversationRow(61, { name: 'from chat delta' })], { next: 'conv-1' }) });
    for (const id of [41, 61]) server.on('GET', msgs(id), { status: 200, body: messageDelta([], { next: `m${id}` }) });
    server.on('GET', NOTIF, { status: 200, body: notificationDelta([]) });
    await manager.syncProject(7);
    // Chat delta first, then promoted by the Local work delta; never demoted.
    const kinds = await db.all<{ id: number; list_kind: string }>('SELECT id, list_kind FROM conversations ORDER BY id');
    expect(kinds).toEqual([{ id: 41, list_kind: 'chat' }, { id: 61, list_kind: 'local_work' }]);
    // The chat delta again (full resync) must neither demote 61 nor sweep it.
    server.on('GET', CONV, { status: 410, body: cursorExpired }, { status: 200, body: conversationDelta([conversationRow(41), conversationRow(61, { name: 'again' })], { next: 'conv-2' }) });
    server.on('GET', LOCAL, { status: 200, body: conversationDelta([], { next: 'lw-2' }) });
    await manager.syncProject(7);
    expect(await db.all<{ id: number; list_kind: string; name: string }>('SELECT id, list_kind, name FROM conversations ORDER BY id')).toEqual([
      { id: 41, list_kind: 'chat', name: 'Conversation 41' },
      { id: 61, list_kind: 'local_work', name: 'Thread 61' },
    ]);
  });

  it('a thread read of a Local work conversation files it under Local work', async () => {
    const { db } = await setup();
    const detail = { id: '70', uuid: '7cbc0244-366e-40b9-ba65-35fcbfb50ce1', name: 'x', source: 'local_work', meta: {}, participants: [] } as never;
    await saveConversationDetail(db, 7, detail);
    await saveConversationDetail(db, 7, { ...(detail as object), id: '71', source: 'chat' } as never);
    expect(await db.all('SELECT id, list_kind FROM conversations ORDER BY id')).toEqual([
      { id: 70, list_kind: 'local_work' },
      { id: 71, list_kind: 'chat' },
    ]);
  });
});
