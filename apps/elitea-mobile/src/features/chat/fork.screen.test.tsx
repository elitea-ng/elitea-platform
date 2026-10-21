import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import { router } from 'expo-router';

import { CAPTURE, conversationDetail13 } from '@/api/__fixtures__/chat13';
import { discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import { FORK_COPY } from '@/api/queries/forkConversation';
import { queryKeys } from '@/api/queries/keys';
import ThreadScreen from '@/app/(app)/c/[id]';
import { clearLiveTurns } from '@/features/chat/turnController';
import { __setOnlineForTests } from '@/lib/network';
import { resetPolicy } from '@/security/policy';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

// Fork (client contract 1.11, issue #1351) from the thread: "Fork from here" on a settled message
// opens the new chat at once; a fork point that is not settled (409), and no connection, are told
// in the thread; a fork shows "Forked from <name>" with a link back.

jest.mock('expo-router', () => ({ useLocalSearchParams: () => ({ id: '3' }), Stack: { Screen: () => null }, router: { replace: jest.fn(), push: jest.fn() } }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const P = CAPTURE.projectId;
const FORK = `/elitea_core/conversation_fork/prompt_lib/${P}/3`;
const forkedRow = { id: 77, name: 'MOCK chat (fork)', created_at: '2026-10-21T10:00:00Z', updated_at: '2026-10-21T10:00:00Z', duration: -1, message_groups_count: 2, is_pinned: false, is_archived: false };

let server: FakeServer;
let queries: QueryClient | null = null;

async function open(detail: unknown = conversationDetail13, contract = '1.11') {
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13({ client_contract: contract })) });
  server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, { status: 200, body: detail });
  server.on('GET', `/elitea_core/message_traces/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  queries.setQueryData(queryKeys.conversations(P), { pages: [{ rows: [], total: 0, offset: 0 }], pageParams: [0] });
  await render(
    <QueryClientProvider client={queries}>
      <ThreadScreen />
    </QueryClientProvider>,
  );
}

async function forkFromAnswer() {
  await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
  await fireEvent.press(screen.getByRole('button', { name: 'Fork from here' }));
}

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  __setOnlineForTests(true);
  clearLiveTurns();
  resetPolicy();
  jest.mocked(router.push).mockClear();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'debug').mockImplementation(() => {});
});
afterEach(() => {
  queries?.clear();
  queries = null;
  __setOnlineForTests(true);
  clearLiveTurns();
  jest.restoreAllMocks();
});

describe('Fork from here', () => {
  it('forks at the message, puts the new chat on the Chats list and opens it at once', async () => {
    server.on('POST', FORK, { status: 201, body: forkedRow });
    await open();
    await forkFromAnswer();
    await waitFor(() => expect(router.push).toHaveBeenCalledWith({ pathname: '/c/[id]', params: { id: '77' } }));
    const post = server.requests.find((r) => r.method === 'POST')!;
    expect(post.url.pathname).toBe(`/api/v2${FORK}`);
    expect(JSON.parse(post.body!)).toEqual({ message_id: CAPTURE.answerUuid });
    const list = queries!.getQueryData<{ pages: { rows: { id: number }[] }[] }>(queryKeys.conversations(P));
    expect(list?.pages[0].rows.map((r) => r.id)).toEqual([77]);
  });

  it('a fork point that is not settled (409 fork_point_unsettled) is told in the thread, nothing opens', async () => {
    server.on('POST', FORK, { status: 409, body: { error: 'fork_point_unsettled' } });
    await open();
    await forkFromAnswer();
    expect(await screen.findByText(FORK_COPY.unsettled)).toBeTruthy();
    expect(router.push).not.toHaveBeenCalled();
  });

  it('offline it says a fork needs a connection and sends nothing (it is not queued)', async () => {
    await open();
    __setOnlineForTests(false);
    await forkFromAnswer();
    expect(await screen.findByText(FORK_COPY.offline)).toBeTruthy();
    expect(server.requests.filter((r) => r.method === 'POST')).toEqual([]);
    expect(router.push).not.toHaveBeenCalled();
  });

  it('is not offered by a server before client contract 1.11', async () => {
    await open(conversationDetail13, '1.10');
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    expect(screen.queryByRole('button', { name: 'Fork from here' })).toBeNull();
  });

  it('forks at a question too (the history up to it)', async () => {
    server.on('POST', FORK, { status: 201, body: forkedRow });
    await open();
    await screen.findByRole('button', { name: 'Answer actions' });
    await fireEvent(screen.getAllByText(/^Show me code/)[0], 'longPress');
    await fireEvent.press(screen.getByRole('button', { name: 'Fork from here' }));
    await waitFor(() => expect(router.push).toHaveBeenCalledWith({ pathname: '/c/[id]', params: { id: '77' } }));
    expect(JSON.parse(server.requests.find((r) => r.method === 'POST')!.body!)).toEqual({ message_id: CAPTURE.questionUuid });
  });

  it('is not offered from the newest question on while its answer still streams', async () => {
    const streaming = {
      ...conversationDetail13,
      message_groups: conversationDetail13.message_groups.map((g) => (g.uuid === CAPTURE.answerUuid ? { ...g, is_streaming: true } : g)),
    };
    await open(streaming);
    await fireEvent(await screen.findByText(/^Show me code/), 'longPress');
    expect(screen.getByRole('button', { name: 'Copy' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Fork from here' })).toBeNull();
  });
});

describe('Forked from', () => {
  it('names the source and opens it', async () => {
    await open({ ...conversationDetail13, forked_from: { conversation_id: 12, name: 'Budget review', message_id: 5 } });
    const link = await screen.findByRole('link', { name: 'Forked from Budget review' });
    await fireEvent.press(link);
    expect(router.push).toHaveBeenCalledWith({ pathname: '/c/[id]', params: { id: '12' } });
  });

  it('shows no link when the reader can no longer see the source (no name)', async () => {
    await open({ ...conversationDetail13, forked_from: { conversation_id: 12 } });
    await screen.findByRole('button', { name: 'Answer actions' });
    expect(screen.queryByTestId('chat-forked-from')).toBeNull();
  });
});
