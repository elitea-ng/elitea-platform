import { QueryClient } from '@tanstack/react-query';

import { FORK_COPY, forkConversation, forkErrorText, forkStillInScope, runFork } from '@/api/queries/forkConversation';
import { syncManager } from '@/sync/appSync';
import { queryKeys } from '@/api/queries/keys';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { ApiError, FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('@/sync/appSync', () => ({ syncManager: { requestSync: jest.fn() } }));

const FORK = '/elitea_core/conversation_fork/prompt_lib/7/41';
const made = { id: 90, name: 'Plan (fork)', created_at: '2026-10-21T10:00:00Z', updated_at: '2026-10-21T10:00:00Z', duration: -1, message_groups_count: 3, is_pinned: false, is_archived: false };

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'oauth', token: 'elnat_test' });
});

describe('forkConversation', () => {
  it('forks the whole history with an empty body, or at a message with message_id', async () => {
    const server = fakeServer();
    server.on('POST', FORK, { status: 201, body: made });
    expect(await forkConversation(7, 41, {}, server.client)).toEqual(made);
    expect(await forkConversation(7, 41, { messageId: 'uuid-5' }, server.client)).toEqual(made);
    expect(server.requests.map((r) => [r.url.pathname, JSON.parse(r.body!)])).toEqual([
      [`/api/v2${FORK}`, {}],
      [`/api/v2${FORK}`, { message_id: 'uuid-5' }],
    ]);
  });
});

describe('forkErrorText', () => {
  it('tells an unsettled fork point, an unsupported chat, a gone one, a refusal and no connection apart', () => {
    expect(forkErrorText(new ApiError('http', 'x', { status: 409, code: 'fork_point_unsettled' }))).toBe(FORK_COPY.unsettled);
    expect(forkErrorText(new ApiError('http', 'x', { status: 409, code: 'fork_not_supported' }))).toBe(FORK_COPY.notSupported);
    expect(forkErrorText(new ApiError('http', 'x', { status: 404 }))).toBe(FORK_COPY.notFound);
    expect(forkErrorText(new ApiError('http', 'x', { status: 403 }))).toBe(FORK_COPY.forbidden);
    expect(forkErrorText(new ApiError('network', 'x'))).toBe(FORK_COPY.offline);
    expect(forkErrorText(new ApiError('http', 'x', { status: 500 }))).toBe(FORK_COPY.failed);
  });
});

describe('runFork', () => {
  const clients: QueryClient[] = [];
  afterEach(() => {
    for (const client of clients.splice(0)) client.clear();
  });
  const withList = () => {
    const client = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity } } });
    clients.push(client);
    client.setQueryData(queryKeys.conversations(7), { pages: [{ rows: [{ ...made, id: 41, name: 'Plan' }], total: 1, offset: 0 }], pageParams: [0] });
    return client;
  };

  it('puts the new chat on top of the Chats list', async () => {
    const server = fakeServer();
    server.on('POST', FORK, { status: 201, body: made });
    const client = withList();
    expect(await runFork(client, 7, 41, {}, server.client, () => true)).toEqual({ ok: true, row: made });
    const list = client.getQueryData<{ pages: { rows: { id: number }[]; total: number }[] }>(queryKeys.conversations(7));
    expect(list?.pages[0].rows.map((r) => r.id)).toEqual([90, 41]);
    expect(list?.pages[0].total).toBe(2);
  });

  it('keeps the inserted row and asks the delta sync, without refetching every loaded page', async () => {
    const server = fakeServer();
    server.on('POST', FORK, { status: 201, body: made });
    const client = withList();
    const invalidate = jest.spyOn(client, 'invalidateQueries');
    jest.mocked(syncManager.requestSync).mockClear();
    await runFork(client, 7, 41, {}, server.client, () => true);
    expect(invalidate).not.toHaveBeenCalled();
    expect(client.getQueryState(queryKeys.conversations(7))?.isInvalidated).toBe(false);
    expect(syncManager.requestSync).toHaveBeenCalledWith(7);
  });

  it('offline it sends nothing (a fork is never queued) and says it needs a connection', async () => {
    const server = fakeServer();
    expect(await runFork(withList(), 7, 41, {}, server.client, () => false)).toEqual({ ok: false, message: FORK_COPY.offline });
    expect(server.requests).toEqual([]);
  });

  it('a 409 fork_point_unsettled leaves the list alone and says to wait', async () => {
    const server = fakeServer();
    server.on('POST', FORK, { status: 409, body: { error: 'fork_point_unsettled' } });
    const client = withList();
    expect(await runFork(client, 7, 41, { messageId: 'uuid-5' }, server.client, () => true)).toEqual({ ok: false, message: FORK_COPY.unsettled });
    expect(client.getQueryData<{ pages: { rows: unknown[] }[] }>(queryKeys.conversations(7))?.pages[0].rows).toHaveLength(1);
  });

  it('no answer reads as offline', async () => {
    const server = fakeServer();
    server.on('POST', FORK, 'network_error');
    expect(await runFork(withList(), 7, 41, {}, server.client, () => true)).toEqual({ ok: false, message: FORK_COPY.offline });
  });
});

describe('forkStillInScope', () => {
  it('is true only in the workspace and project the fork started in', () => {
    const started = { workspaceId: 'w1', projectId: 7 };
    expect(forkStillInScope(started, { workspaceId: 'w1', projectId: 7 })).toBe(true);
    expect(forkStillInScope(started, { workspaceId: 'w1', projectId: 8 })).toBe(false);
    expect(forkStillInScope(started, { workspaceId: 'w2', projectId: 7 })).toBe(false);
  });
});
