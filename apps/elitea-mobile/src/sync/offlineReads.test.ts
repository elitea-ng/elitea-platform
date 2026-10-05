import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import { fetchConversationPage, savedFirstPage } from '@/api/queries/conversations';
import { fetchConversation } from '@/api/queries/conversation';
import { fetchNotifications, fetchUnseenCount } from '@/api/queries/notifications';
import { migrate } from '@/db/migrations';
import { offlineStore } from '@/db/offlineStore';
import { Db } from '@/db/sql';
import { agentParticipant, toChatMessages } from '@/features/chat/messageModel';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { syncManager } from '@/sync/appSync';
import {
  OFFLINE_READ_DEADLINE_MS,
  isOfflineCopy,
  offlineConversationRows,
  offlineNotifications,
  offlineUnseenCount,
  readWithFallback,
  toMessageGroup,
} from '@/sync/offlineReads';
import {
  conversationDelta,
  conversationRow,
  messageDelta,
  messageRow,
  notificationDelta,
  notificationRow,
} from '@/sync/__fixtures__/wp6';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';
import { openNodeSqlite } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const UUID = '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c';
const detail = {
  id: '42',
  uuid: UUID,
  project_id: '7',
  name: 'Quarterly plan',
  created_at: '2026-10-01T09:00:00Z',
  updated_at: '2026-10-04T12:00:00Z',
  created_by: '3',
  message_count: 2,
  is_private: true,
  meta: {},
  participants: [
    { id: 1, entity_name: 'user', entity_meta: { id: 3 }, meta: { name: 'Me' } },
    { id: 2, entity_name: 'application', entity_meta: { id: 11, project_id: 7 }, meta: { name: 'Planner' } },
  ],
  message_groups: [],
};

async function storeWith() {
  const db = new Db(openNodeSqlite());
  await migrate(db);
  jest.spyOn(offlineStore, 'get').mockReturnValue(db);
  return db;
}
/** The app's sync manager talks through the session client, i.e. the global fetch. */
function installServer() {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  return server;
}
const flush = async () => {
  for (let i = 0; i < 20; i++) await new Promise((r) => setTimeout(r, 0));
};

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'oauth', token: 'elnat_test' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
const realFetch = globalThis.fetch;
afterEach(() => {
  jest.restoreAllMocks();
  globalThis.fetch = realFetch;
});

describe('toMessageGroup', () => {
  it('turns a transcript row into the thread shape: text first, then attachment items', () => {
    const group = toMessageGroup(
      messageRow(4, { message_items: [{ id: 9, item_type: 'attachment_message', order_index: 0, item_details: {} }] }) as never,
    );
    expect(group).toMatchObject({ id: 4, uuid: expect.any(String), author_participant_id: 2, is_streaming: false });
    expect(group?.message_items.map((i) => i.item_type)).toEqual(['text_message', 'attachment_message']);
    expect(toMessageGroup({ ...messageRow(1), id: 'x' } as never)).toBeNull();
  });
});

describe('offline fallback in the queries', () => {
  it('the conversation list falls back to the store when the workspace does not answer', async () => {
    await storeWith();
    const server = installServer();
    server.on('GET', '/elitea_core/conversations/prompt_lib/7', { status: 200, body: conversationDelta([conversationRow(41), conversationRow(42, { updated_at: '2026-10-04T13:00:00.000000' })]) });
    server.on('GET', '/elitea_core/messages/', { status: 200, body: messageDelta([]) });
    server.on('GET', '/notifications/', { status: 200, body: notificationDelta([]) });
    await syncManager.syncProject(7);

    server.on('GET', '/elitea_core/conversations/prompt_lib/7', 'network_error');
    const page = await fetchConversationPage(7, 0, server.client);
    expect(page.offline).toBe(true);
    expect(page.rows.map((r) => r.id)).toEqual([42, 41]);
    expect(page.total).toBe(2);
  });

  it('without an open store the network error is reported as before', async () => {
    jest.spyOn(offlineStore, 'get').mockReturnValue(null);
    const server = fakeServer();
    server.on('GET', '/elitea_core/conversations/', 'network_error');
    await expect(fetchConversationPage(7, 0, server.client)).rejects.toMatchObject({ kind: 'network' });
  });

  it('an answered error (403, 500) is never replaced by the stored copy', async () => {
    await storeWith();
    const server = fakeServer();
    server.on('GET', '/elitea_core/conversations/', { status: 500, body: {} });
    await expect(fetchConversationPage(7, 0, server.client)).rejects.toMatchObject({ kind: 'http', status: 500 });
  });

  it('a thread read online is kept; offline the thread shows it with the synced transcript and its agent', async () => {
    await storeWith();
    const server = installServer();
    server.on('GET', '/elitea_core/conversation/prompt_lib/7/42', { status: 200, body: detail });
    server.on('GET', '/elitea_core/messages/prompt_lib/7/42', { status: 200, body: messageDelta([messageRow(1), messageRow(2)]) });
    const online = await fetchConversation(7, '42', server.client);
    expect(isOfflineCopy(online)).toBe(false);
    await flush();

    server.on('GET', '/elitea_core/conversation/prompt_lib/7/', 'network_error');
    for (const ref of ['42', UUID]) {
      const offline = await fetchConversation(7, ref, server.client);
      expect(isOfflineCopy(offline)).toBe(true);
      expect(offline.uuid).toBe(UUID);
      expect(offline.message_groups?.map((g) => g.id)).toEqual([2, 1]); // newest first, like the network read
      const messages = toChatMessages(offline);
      expect(messages.map((m) => [m.role, m.text])).toEqual([
        ['user', 'message 1'],
        ['assistant', 'message 2'],
      ]);
      expect(agentParticipant(offline)).toEqual({ ok: true, participantId: 2, name: 'Planner' });
    }
  });

  it('a conversation known only from the list is shown read-only offline', async () => {
    const db = await storeWith();
    await db.run('INSERT INTO conversations (project_id, id, row_json) VALUES (7, 41, ?)', [JSON.stringify(conversationRow(41))]);
    await db.run('INSERT INTO messages (project_id, conversation_id, id, row_json) VALUES (7, 41, 1, ?)', [JSON.stringify(messageRow(1))]);
    const server = fakeServer();
    server.on('GET', '/elitea_core/conversation/', 'network_error');
    const offline = await fetchConversation(7, '41', server.client);
    expect(offline.name).toBe('Conversation 41');
    expect(toChatMessages(offline).map((m) => m.role)).toEqual(['user']);
    expect(agentParticipant(offline).ok).toBe(false);
  });

  it('notifications and the unseen count fall back to the store', async () => {
    const db = await storeWith();
    for (const row of [notificationRow(1), notificationRow(2, { is_seen: true })]) {
      await db.run('INSERT INTO notifications (project_id, id, is_seen, row_json) VALUES (7, ?, ?, ?)', [row.id, row.is_seen ? 1 : 0, JSON.stringify(row)]);
    }
    const server = fakeServer();
    server.on('GET', '/notifications/', 'network_error');
    const rows = await fetchNotifications(7, server.client);
    expect(isOfflineCopy(rows)).toBe(true);
    expect(rows.map((r) => r.id).sort()).toEqual([1, 2]);
    expect(await fetchUnseenCount(7, server.client)).toBe(1);
  });
});

// DEF-R5 (Agent Zefir E2E run 2): with the workspace paused (the edge accepts
// the connection, nothing answers), an offline cold start showed a spinner for
// 15-20 s, the full REST deadline, before the saved list; a saved thread took
// about 23 s.
describe('offline cold start answers from the saved copy quickly (DEF-R5)', () => {
  /** Accepts the request and never answers, like a paused workspace behind its edge. */
  const hanging: BaseFetch = (_input, init) =>
    new Promise<Response>((_, reject) =>
      init?.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError'))),
    );

  const openStore = { isOpen: () => true, subscribe: () => () => {} };
  const closedStore = { isOpen: () => false, subscribe: () => () => {} };

  it('starts the short deadline when the store opens mid-read (cold start: the lock releases it later)', async () => {
    let open = false;
    const listeners = new Set<() => void>();
    const store = {
      isOpen: () => open,
      subscribe: (l: () => void) => {
        listeners.add(l);
        return () => listeners.delete(l);
      },
    };
    const pending = readWithFallback(
      (signal) => new Promise<string>((_, reject) => signal?.addEventListener('abort', () => reject(new Error('aborted')))),
      async () => 'stored',
      { store, deadlineMs: 5 },
    );
    await new Promise((r) => setTimeout(r, 20));
    open = true;
    listeners.forEach((l) => l());
    expect(await pending).toBe('stored');
    expect(listeners.size).toBe(0);
  });

  // The fake-timer test runs last: this environment does not restore setTimeout after it.
  afterEach(() => jest.useRealTimers());

  it('never answers a caller abort with the saved copy', async () => {
    const caller = new AbortController();
    const offline = jest.fn(async () => 'stored');
    const pending = readWithFallback(
      (signal) =>
        new Promise<string>((_, reject) => signal?.addEventListener('abort', () => reject(new Error('aborted')))),
      offline,
      { signal: caller.signal, store: openStore },
    );
    caller.abort();
    await expect(pending).rejects.toThrow('aborted');
    expect(offline).not.toHaveBeenCalled();
  });

  it('keeps the full deadline when there is no saved copy to show', async () => {
    // Answers after 30 ms; like fetch, rejects once its signal aborts.
    const online = jest.fn(
      (signal: AbortSignal | undefined) =>
        new Promise<string>((resolve, reject) => {
          setTimeout(() => resolve('answered'), 30);
          signal?.addEventListener('abort', () => reject(new Error('aborted')));
        }),
    );
    const offline = jest.fn(async () => 'stored');
    expect(await readWithFallback(online, offline, { store: closedStore, deadlineMs: 5 })).toBe('answered');
    expect(await readWithFallback(online, offline, { store: openStore, deadlineMs: 5 })).toBe('stored');
  });

  // F4: the short deadline is for showing a saved copy; with none, the read keeps the full deadline.
  it('keeps waiting for the workspace when the store is open but holds no copy of this read', async () => {
    const online = (signal: AbortSignal | undefined) =>
      new Promise<string>((resolve, reject) => {
        setTimeout(() => resolve('answered'), 30);
        signal?.addEventListener('abort', () => reject(new Error('aborted')));
      });
    const offline = jest.fn(async () => null);
    expect(await readWithFallback(online, offline, { store: openStore, deadlineMs: 5 })).toBe('answered');
    expect(offline).toHaveBeenCalled();
  });

  it('a list or notifications never synced is no copy (null), a synced empty one is', async () => {
    const db = await storeWith();
    expect(await offlineConversationRows(7, 0, 30)).toBeNull();
    expect(await offlineNotifications(7, 30)).toBeNull();
    expect(await offlineUnseenCount(7)).toBeNull();
    const server = installServer();
    server.on('GET', '/elitea_core/conversations/prompt_lib/7', { status: 200, body: conversationDelta([]) });
    server.on('GET', '/elitea_core/messages/', { status: 200, body: messageDelta([]) });
    server.on('GET', '/notifications/', { status: 200, body: notificationDelta([]) });
    await syncManager.syncProject(7);
    expect(await offlineConversationRows(7, 0, 30)).toEqual({ rows: [], total: 0 });
    expect(await offlineNotifications(7, 30)).toEqual([]);
    expect(await offlineUnseenCount(7)).toBe(0);
    expect(db.isClosed).toBe(false);
  });

  it('shows the saved first page as placeholder data, without the offline notice', () => {
    const saved = { rows: [conversationRow(41) as never], total: 1 };
    expect(savedFirstPage(saved)).toEqual({ pages: [{ rows: saved.rows, total: 1, offset: 0 }], pageParams: [0] });
    expect(savedFirstPage(saved)?.pages[0].offline).toBeUndefined();
    expect(savedFirstPage({ rows: [], total: 0 })).toBeUndefined();
    expect(savedFirstPage(null)).toBeUndefined();
  });
  it('falls back to the saved list after the short read deadline, not the 20 s REST deadline', async () => {
    await storeWith();
    const server = installServer();
    server.on('GET', '/elitea_core/conversations/prompt_lib/7', { status: 200, body: conversationDelta([conversationRow(41)]) });
    server.on('GET', '/elitea_core/messages/', { status: 200, body: messageDelta([]) });
    server.on('GET', '/notifications/', { status: 200, body: notificationDelta([]) });
    await syncManager.syncProject(7);

    jest.useFakeTimers();
    const client = createApiClient({ origin: FAKE_ORIGIN, auth: { kind: 'session' }, fetch: hanging });
    let settled = false;
    const pending = fetchConversationPage(7, 0, client).finally(() => {
      settled = true;
    });
    await jest.advanceTimersByTimeAsync(OFFLINE_READ_DEADLINE_MS);
    expect(settled).toBe(true);
    const page = await pending;
    expect(page).toMatchObject({ offline: true, rows: [expect.objectContaining({ id: 41 })] });
    expect(OFFLINE_READ_DEADLINE_MS).toBeLessThanOrEqual(8_000);
  });
});
