import { QueryClient, type InfiniteData } from '@tanstack/react-query';

import { createApiClient } from '@/api/client';
import type { ConversationPage } from '@/api/queries/conversations';
import { queryKeys } from '@/api/queries/keys';
import { markConversationRead, patchConversationUnread, runMarkConversationRead } from '@/api/queries/markRead';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const READ = '/elitea_core/conversation_read/prompt_lib/7/41';
const row = (id: number, unread: boolean) => ({ id, name: `c${id}`, created_at: '', updated_at: '', duration: -1, message_groups_count: 1, unread });
const unreadOf = (client: QueryClient, key: readonly unknown[]) =>
  client.getQueryData<InfiniteData<ConversationPage>>(key)?.pages.flatMap((page) => page.rows.map((r) => [r.id, r.unread]));

const clients: QueryClient[] = [];
const withLists = () => {
  const client = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity } } });
  clients.push(client);
  client.setQueryData(queryKeys.conversations(7), { pages: [{ rows: [row(41, true), row(42, true)], total: 2, offset: 0 }], pageParams: [0] });
  client.setQueryData(queryKeys.localWork(7), { pages: [{ rows: [row(41, true)], total: 1, offset: 0 }], pageParams: [0] });
  return client;
};

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'oauth', token: 'elnat_test' });
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => {
  for (const client of clients.splice(0)) client.clear();
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
    patchConversationUnread(client, 7, 41, false);
    expect(unreadOf(client, queryKeys.conversations(7))).toEqual([
      [41, false],
      [42, true],
    ]);
    expect(unreadOf(client, queryKeys.localWork(7))).toEqual([[41, false]]);
  });

  it('turns the row read before the answer, and keeps the workspace answer', async () => {
    const client = withLists();
    const server = fakeServer();
    let answer: (value: unknown) => void = () => {};
    const gate = new Promise((resolve) => (answer = resolve));
    server.on('PUT', READ, { status: 200, body: { conversation_id: 41, read_at: '2026-10-21T10:00:00Z', unread: true } });
    // The request waits on the gate, so the optimistic patch is observable.
    const gated = createApiClient({ origin: FAKE_ORIGIN, auth: { kind: 'session' }, fetch: async (request) => (await gate, server.fetch(request)) });
    const pending = runMarkConversationRead(7, 41, { queryClient: client, getDb: () => null, client: gated });
    expect(unreadOf(client, queryKeys.conversations(7))?.[0]).toEqual([41, false]);
    answer(null);
    expect(await pending).toBe(true);
    // The workspace says an answer newer than the marker is still unread.
    expect(unreadOf(client, queryKeys.conversations(7))?.[0]).toEqual([41, true]);
  });

  it('reads the lists again when the workspace refused the read', async () => {
    const client = withLists();
    const invalidate = jest.spyOn(client, 'invalidateQueries');
    const server = fakeServer();
    server.on('PUT', READ, { status: 404, body: { error: 'not found' } });
    expect(await runMarkConversationRead(7, 41, { queryClient: client, getDb: () => null, client: server.client })).toBe(false);
    expect(invalidate.mock.calls.map(([filters]) => filters?.queryKey)).toEqual([queryKeys.conversations(7), queryKeys.localWork(7)]);
  });
});
