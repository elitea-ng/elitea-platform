import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react-native';
import type { ReactNode } from 'react';

import { prepareTurn } from '@/api/queries/sendMessage';
import { migrate } from '@/db/migrations';
import { offlineStore } from '@/db/offlineStore';
import { Db } from '@/db/sql';
import { clearLiveTurns, getLiveTurn, setLiveTurn, turnKey, type TurnController } from '@/features/chat/turnController';
import { useTurn } from '@/features/chat/useTurn';
import { __setOnlineForTests } from '@/lib/network';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { outboxDrainer } from '@/sync/appOutbox';
import { enqueue, listOutbox } from '@/sync/outbox';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';
import { openNodeSqlite } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-crypto', () => ({ randomUUID: () => '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b' }));

const P = 7;
const UUID = '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c';
const QA = '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f';

let queries: QueryClient | null = null;
function wrapper({ children }: { children: ReactNode }) {
  return <QueryClientProvider client={queries!}>{children}</QueryClientProvider>;
}

const renderTurn = () =>
  renderHook(() => useTurn({ projectId: P, conversationRef: '42', conversationUuid: UUID, participantId: 2 }), { wrapper });

async function openStore(): Promise<Db> {
  const db = new Db(openNodeSqlite());
  await migrate(db);
  jest.spyOn(offlineStore, 'get').mockReturnValue(db);
  return db;
}

beforeEach(() => {
  __resetSessionForTests();
  clearLiveTurns();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});

afterEach(() => {
  queries?.clear();
  queries = null;
  clearLiveTurns();
  __setOnlineForTests(true);
  jest.restoreAllMocks();
});

describe('useTurn.send', () => {
  // F3: an earlier message still waiting in the outbox must not be overtaken by a live send.
  it('queues behind an unsent outbox message of the same conversation even when online', async () => {
    const db = await openStore();
    const server = fakeServer();
    globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
    // A: handed off after a lost answer, now in backoff.
    await enqueue(db, prepareTurn({ projectId: P, conversationUuid: UUID, participantId: 2, questionId: QA, userInput: 'A' }), '42', Date.now(), {
      failedAttempts: 1,
    });
    const drain = jest.spyOn(outboxDrainer, 'drain').mockResolvedValue({ sent: 0, failed: 0, waiting: 2 });
    const { result } = await renderTurn();
    let problem: string | null = 'unset';
    await act(async () => {
      problem = await result.current.send('B');
    });
    expect(problem).toBeNull();
    expect(getLiveTurn(turnKey(P, UUID))).toBeUndefined();
    expect((await listOutbox(db, P, UUID)).map((i) => i.userText)).toEqual(['A', 'B']);
    expect(server.requests.filter((r) => r.method === 'POST')).toEqual([]);
    expect(drain).toHaveBeenCalled(); // online: B goes out right after A, in order
  });

  it('sends live when nothing of this conversation is waiting', async () => {
    await openStore();
    const server = fakeServer();
    globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
    const { result } = await renderTurn();
    await act(async () => {
      await result.current.send('hello');
    });
    expect(getLiveTurn(turnKey(P, UUID))).toBeDefined();
  });

  // F5: a queueing failure must not look like a successful send (the Composer clears the text on null).
  it('reports an error when the offline queue cannot store the message', async () => {
    const db = await openStore();
    __setOnlineForTests(false);
    await db.close();
    const { result } = await renderTurn();
    let problem: string | null = null;
    await act(async () => {
      problem = await result.current.send('written offline');
    });
    expect(problem).toEqual(expect.any(String));
    expect(getLiveTurn(turnKey(P, UUID))).toBeUndefined();
  });

  // A4: the composer's mentions reach the wire as user_ids.
  it('sends the mentions as user_ids and is_mentioning_everyone', async () => {
    const db = await openStore();
    jest.spyOn(outboxDrainer, 'drain').mockResolvedValue({ sent: 0, failed: 0, waiting: 1 });
    const server = fakeServer();
    server.on('POST', `/elitea_core/messages/prompt_lib/${P}/${UUID}`, { status: 503 });
    globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
    const { result } = await renderTurn();
    await act(async () => {
      await result.current.send('@Anna @Everyone hi', { mentions: { userIds: [42], everyone: true } });
    });
    // The workspace answered 503, so the turn went to the outbox; the bytes sent are the stored ones.
    const post = server.requests.find((r) => r.method === 'POST');
    expect(JSON.parse(post!.body!)).toMatchObject({ user_ids: [42], is_mentioning_everyone: true });
    const [queued] = await listOutbox(db, P, UUID);
    expect(queued.bodyJson).toBe(post!.body);
  });

  it('refuses a send that mentions more than 64 people, with its own text', async () => {
    await openStore();
    const { result } = await renderTurn();
    let problem: string | null = null;
    await act(async () => {
      problem = await result.current.send('hi', { mentions: { userIds: Array.from({ length: 65 }, (_, i) => i + 1), everyone: false } });
    });
    expect(problem).toBe('This message mentions too many people.');
  });

  it('continueWith refuses offline and never queues a continuation', async () => {
    const db = await openStore();
    __setOnlineForTests(false);
    const { result } = await renderTurn();
    let problem: string | null = null;
    await act(async () => {
      problem = await result.current.continueWith('agent.continue.output-limit.v1', {
        project_id: P,
        conversation_uuid: UUID,
        message_id: 'resp-1',
      });
    });
    expect(problem).toBe("You're offline.");
    expect(getLiveTurn(turnKey(P, UUID))).toBeUndefined();
    expect(await listOutbox(db, P, UUID)).toEqual([]);
  });

  it('continueWith starts a live continuation turn online', async () => {
    await openStore();
    const server = fakeServer();
    globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
    const { result } = await renderTurn();
    await act(async () => {
      await result.current.continueWith('agent.continue.output-limit.v1', { project_id: P, conversation_uuid: UUID, message_id: 'resp-1' });
    });
    expect(getLiveTurn(turnKey(P, UUID))?.getSnapshot().continuation).toBe(true);
  });
});

describe('useTurn settling', () => {
  it('reads the tool history again when a turn (or a continuation of it) has settled', async () => {
    await openStore();
    // The answer's tool history as read while it was paused: one call. The
    // approved continuation writes a second call into the SAME message group,
    // so the group ids — the traces query's key — do not change.
    const traces = ['p', P, 'traces', 42, '34'];
    const conversation = ['p', P, 'conversation', '42'];
    queries!.setQueryData(traces, new Map([[34, [{ id: 'trace-19' }]]]));
    queries!.setQueryData(conversation, { id: 42 });
    const snapshot = { settled: true, turn: { phase: 'done' } };
    const ended = {
      getSnapshot: () => snapshot,
      subscribe: () => () => {},
      reattach: async () => {},
      detach: () => {},
    } as unknown as TurnController;
    setLiveTurn(turnKey(P, UUID), ended);

    await renderTurn();

    expect(queries!.getQueryState(conversation)?.isInvalidated).toBe(true);
    expect(queries!.getQueryState(traces)?.isInvalidated).toBe(true);
  });
});
