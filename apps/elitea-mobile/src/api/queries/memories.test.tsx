import { SETTINGS_CAPTURE, memoriesCleared, memoriesEmpty, memoriesList } from '@/api/__fixtures__/settings13';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook } from '@testing-library/react-native';
import type { ReactNode } from 'react';

import { queryKeys } from '@/api/queries/keys';
import {
  MemoryError,
  clearMemories,
  deleteMemory,
  fetchMemories,
  memoriesOf,
  memoryErrorText,
  useForgetMemories,
  type Memory,
} from '@/api/queries/memories';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const P = SETTINGS_CAPTURE.projectId;
const LIST = `/elitea_core/memories/prompt_lib/${P}`;

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
});

describe('memories (client contract 1.3, captured shapes)', () => {
  it('lists the caller\'s memories, most recent first', async () => {
    const server = fakeServer();
    server.on('GET', LIST, { status: 200, body: memoriesList });
    await expect(fetchMemories(P, server.client)).resolves.toEqual([
      { id: '2', content: 'My team ships on Thursdays.', tags: [], enabled: true, sourceConversationId: null, updatedAt: '2026-10-06T23:37:01.921215Z' },
      { id: '1', content: 'I prefer answers in British English.', tags: ['style'], enabled: true, sourceConversationId: null, updatedAt: '2026-10-06T23:37:01.910148Z' },
    ]);
  });

  it('an empty list is empty, not a failure', async () => {
    const server = fakeServer();
    server.on('GET', LIST, { status: 200, body: memoriesEmpty });
    await expect(fetchMemories(P, server.client)).resolves.toEqual([]);
  });

  it('deletes one (204, also for an id already gone) and clears all', async () => {
    const server = fakeServer();
    server.on('DELETE', `/elitea_core/memory/prompt_lib/${P}/1`, { status: 204 });
    server.on('DELETE', LIST, { status: 200, body: memoriesCleared });
    await expect(deleteMemory(P, '1', server.client)).resolves.toBeUndefined();
    await expect(deleteMemory(P, '1', server.client)).resolves.toBeUndefined();
    await expect(clearMemories(P, server.client)).resolves.toBe(1);
    expect(server.requests.map((r) => `${r.method} ${r.url.pathname}`)).toEqual([
      `DELETE /api/v2/elitea_core/memory/prompt_lib/${P}/1`,
      `DELETE /api/v2/elitea_core/memory/prompt_lib/${P}/1`,
      `DELETE /api/v2${LIST}`,
    ]);
  });

  it('refuses an id that is not a plain id before any request', async () => {
    const server = fakeServer();
    await expect(deleteMemory(P, '../memories', server.client)).rejects.toBeInstanceOf(MemoryError);
    expect(server.requests).toHaveLength(0);
  });

  it('names the failure: offline, an older server, no access', async () => {
    const server = fakeServer();
    server.on('GET', LIST, 'network_error', { status: 404, body: {} }, { status: 403, body: {} }, { status: 200, body: { nope: 1 } });
    const reasons: string[] = [];
    for (let i = 0; i < 4; i++) reasons.push(((await fetchMemories(P, server.client).catch((e: unknown) => e)) as MemoryError).reason);
    expect(reasons).toEqual(['offline', 'not_available', 'not_allowed', 'failed']);
    expect(memoryErrorText(new MemoryError('offline'))).toMatch(/needs a connection/);
    expect(memoryErrorText(new MemoryError('failed'), 'clear')).toMatch(/Nothing was cleared/);
  });

  it('drops malformed rows and duplicates, and marks a turned-off memory', () => {
    const rows = memoriesOf({
      items: [{ ...memoriesList.items[0], enabled: false }, memoriesList.items[0], { id: 'x y', content: 'bad id' }, { id: '9' }],
      total: 4,
    });
    expect(rows.map((m) => [m.id, m.enabled])).toEqual([['2', false]]);
  });
});

describe('forgetting deleted memories in the cache', () => {
  // F5: a refetch (pull-to-refresh, focus) the server answered before the
  // DELETE must not land after the cache edit and bring the row back.
  it.each([[['1'] as readonly string[] | 'all', ['2']], ['all' as const, []]])(
    'a list refetch already in flight does not bring %p back',
    async (ids, left) => {
      const client = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity, retry: false } } });
      const key = queryKeys.memories(P);
      const rows = memoriesOf(memoriesList);
      client.setQueryData(key, rows);
      let answer: (rows: Memory[]) => void = () => {};
      const inFlight = client.fetchQuery({ queryKey: key, queryFn: () => new Promise<Memory[]>((resolve) => (answer = resolve)), staleTime: 0 }).catch(() => null);
      const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
      const { result } = await renderHook(() => useForgetMemories(P), { wrapper });
      await result.current(ids);
      answer(rows); // the pre-delete answer arrives late
      await inFlight;
      expect(client.getQueryData<Memory[]>(key)!.map((m) => m.id)).toEqual(left);
      client.clear();
    },
  );
});
