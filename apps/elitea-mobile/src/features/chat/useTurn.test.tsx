import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react-native';
import type { ReactNode } from 'react';

import { prepareTurn } from '@/api/queries/sendMessage';
import { migrate } from '@/db/migrations';
import { offlineStore } from '@/db/offlineStore';
import { Db } from '@/db/sql';
import { clearLiveTurns, getLiveTurn, turnKey } from '@/features/chat/turnController';
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
});
