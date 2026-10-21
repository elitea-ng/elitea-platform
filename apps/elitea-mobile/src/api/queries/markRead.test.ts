import { QueryClient, type InfiniteData } from '@tanstack/react-query';

import { createApiClient } from '@/api/client';
import { ApiError } from '@/api/errors';
import type { ConversationPage } from '@/api/queries/conversations';
import { queryKeys } from '@/api/queries/keys';
import { isPermanentReadFailure, MARK_READ_REFETCH_DEBOUNCE_MS, markConversationRead, runMarkConversationRead } from '@/api/queries/markRead';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { patchConversationPages } from '@/sync/conversationPages';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const READ = '/elitea_core/conversation_read/prompt_lib/7/41';
const row = (id: number, unread: boolean) => ({ id, name: `c${id}`, created_at: '', updated_at: '', duration: -1, message_groups_count: 1, unread });
const listOf = (...rows: ReturnType<typeof row>[]): InfiniteData<ConversationPage, number> => ({ pages: [{ rows, total: rows.length, offset: 0 }], pageParams: [0] });
const unreadOf = (client: QueryClient, key: readonly unknown[]) =>
  client.getQueryData<InfiniteData<ConversationPage>>(key)?.pages.flatMap((page) => page.rows.map((r) => [r.id, r.unread]));

const clients: QueryClient[] = [];
const withLists = () => {
  const client = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity } } });
  clients.push(client);
  client.setQueryData(queryKeys.conversations(7), listOf(row(41, true), row(42, true)));
  client.setQueryData(queryKeys.localWork(7), listOf(row(41, true)));
  return client;
};

/** A client whose requests wait until `open()`; `sent` resolves once one is made. */
function gatedClient(server: FakeServer) {
  let open: () => void = () => {};
  let sent: () => void = () => {};
  const gate = new Promise<void>((resolve) => (open = resolve));
  const requested = new Promise<void>((resolve) => (sent = resolve));
  const client = createApiClient({
    origin: FAKE_ORIGIN,
    auth: { kind: 'session' },
    fetch: async (request) => {
      sent();
      await gate;
      return server.fetch(request);
    },
  });
  return { client, open: () => open(), requested };
}

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'oauth', token: 'elnat_test' });
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => {
  for (const client of clients.splice(0)) client.clear();
  jest.useRealTimers();
  jest.restoreAllMocks();
});

describe('markConversationRead (client contract 1.12)', () => {
  it('PUTs the read marker with no body and answers the row unread state', async () => {
    const server = fakeServer();
    server.on('PUT', READ, { status: 200, body: { conversation_id: 41, read_at: '2026-10-21T10:00:00Z', unread: false } });
    expect(await markConversationRead(7, 41, server.client)).toEqual({ unread: false });
    expect(server.requests.map((r) => [r.method, r.url.pathname])).toEqual([['PUT', `/api/v2${READ}`]]);
  });

  it('patches only the conversation in both lists', () => {
    const client = withLists();
    patchConversationPages(client, 7, 41, { kind: 'unread', unread: false });
    expect(unreadOf(client, queryKeys.conversations(7))).toEqual([
      [41, false],
      [42, true],
    ]);
    expect(unreadOf(client, queryKeys.localWork(7))).toEqual([[41, false]]);
  });

  it('turns the row read before the answer, and keeps the workspace answer', async () => {
    const client = withLists();
    const server = fakeServer();
    server.on('PUT', READ, { status: 200, body: { conversation_id: 41, read_at: '2026-10-21T10:00:00Z', unread: true } });
    const gated = gatedClient(server);
    const pending = runMarkConversationRead(7, 41, { queryClient: client, getDb: () => null, client: gated.client });
    await gated.requested;
    expect(unreadOf(client, queryKeys.conversations(7))?.[0]).toEqual([41, false]);
    gated.open();
    expect(await pending).toEqual({ kind: 'read', unread: true });
    // The workspace says an answer newer than the marker is still unread.
    expect(unreadOf(client, queryKeys.conversations(7))?.[0]).toEqual([41, true]);
  });

  it('cancels a list read in flight, so its older answer cannot turn the row unread again', async () => {
    const client = withLists();
    let answerList: (data: InfiniteData<ConversationPage, number>) => void = () => {};
    const listing = client
      .fetchQuery({ queryKey: queryKeys.conversations(7), queryFn: () => new Promise<InfiniteData<ConversationPage, number>>((resolve) => (answerList = resolve)) })
      .catch((error: unknown) => error);
    expect(client.isFetching({ queryKey: queryKeys.conversations(7) })).toBe(1);
    const server = fakeServer();
    server.on('PUT', READ, { status: 200, body: { conversation_id: 41, read_at: '2026-10-21T10:00:00Z', unread: false } });
    const invalidate = jest.spyOn(client, 'invalidateQueries');
    expect(await runMarkConversationRead(7, 41, { queryClient: client, getDb: () => null, client: server.client })).toEqual({ kind: 'read', unread: false });
    // The list read answers late, with what it read before the marker moved: unread.
    answerList(listOf(row(41, true), row(42, true)));
    await listing;
    expect(unreadOf(client, queryKeys.conversations(7))?.[0]).toEqual([41, false]);
    // The cancelled list read is sent again (after the read, so it answers read).
    expect(invalidate.mock.calls.map(([filters]) => filters?.queryKey)).toEqual([queryKeys.conversations(7)]);
  });

  it('writes the workspace answer even when it says read, over a list read that landed meanwhile', async () => {
    const client = withLists();
    const server = fakeServer();
    server.on('PUT', READ, { status: 200, body: { conversation_id: 41, read_at: '2026-10-21T10:00:00Z', unread: false } });
    const gated = gatedClient(server);
    const pending = runMarkConversationRead(7, 41, { queryClient: client, getDb: () => null, client: gated.client });
    await gated.requested;
    // A refetch that started before the read answered: the row is unread again.
    client.setQueryData(queryKeys.conversations(7), listOf(row(41, true), row(42, true)));
    gated.open();
    expect(await pending).toEqual({ kind: 'read', unread: false });
    expect(unreadOf(client, queryKeys.conversations(7))?.[0]).toEqual([41, false]);
  });

  it('a refusal for good (403, 404) is reported and does not read the lists again', async () => {
    jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
    for (const status of [403, 404]) {
      const client = withLists();
      const invalidate = jest.spyOn(client, 'invalidateQueries');
      const server = fakeServer();
      server.on('PUT', READ, { status, body: { error: 'nope' } });
      expect(await runMarkConversationRead(7, 41, { queryClient: client, getDb: () => null, client: server.client })).toEqual({ kind: 'refused' });
      jest.advanceTimersByTime(MARK_READ_REFETCH_DEBOUNCE_MS * 2);
      expect(invalidate).not.toHaveBeenCalled();
    }
  });

  it('transient failures read the lists again once per burst', async () => {
    jest.useFakeTimers({ doNotFake: ['nextTick', 'setImmediate', 'queueMicrotask'] });
    const client = withLists();
    const invalidate = jest.spyOn(client, 'invalidateQueries');
    const server = fakeServer();
    server.on('PUT', READ, { status: 503, body: { error: 'busy' } });
    const deps = { queryClient: client, getDb: () => null, client: server.client };
    expect(await runMarkConversationRead(7, 41, deps)).toEqual({ kind: 'failed' });
    expect(await runMarkConversationRead(7, 41, deps)).toEqual({ kind: 'failed' });
    expect(invalidate).not.toHaveBeenCalled();
    jest.advanceTimersByTime(MARK_READ_REFETCH_DEBOUNCE_MS);
    expect(invalidate.mock.calls.map(([filters]) => filters?.queryKey)).toEqual([queryKeys.conversations(7), queryKeys.localWork(7)]);
  });

  it('classifies which failures are for good', () => {
    const http = (status: number) => new ApiError('http', `HTTP ${status}`, { status });
    for (const status of [400, 403, 404, 410, 422]) expect(isPermanentReadFailure(http(status))).toBe(true);
    for (const status of [408, 425, 429, 500, 503]) expect(isPermanentReadFailure(http(status))).toBe(false);
    expect(isPermanentReadFailure(new ApiError('upgrade_required', 'update'))).toBe(true);
    expect(isPermanentReadFailure(new ApiError('network', 'offline'))).toBe(false);
    expect(isPermanentReadFailure(new ApiError('timeout', 'slow'))).toBe(false);
    expect(isPermanentReadFailure(new TypeError('boom'))).toBe(false);
  });
});
