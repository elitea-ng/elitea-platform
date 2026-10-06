import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import { CAPTURE, REGENERATION_ID, answerGroupAfterRegenerate, conversationDetail13, regenerateStarted } from '@/api/__fixtures__/chat13';
import { discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import ThreadScreen from '@/app/(app)/c/[id]';
import { loadFixtureText } from '@/features/chat/__fixtures__/loadFixture';
import { clearLiveTurns } from '@/features/chat/turnController';
import { resetPolicy } from '@/security/policy';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';
import { fakeStreamTransport } from '@/test/fakeStreamTransport';

jest.mock('expo-router', () => ({ useLocalSearchParams: () => ({ id: '3' }), Stack: { Screen: () => null }, router: { replace: jest.fn() } }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
// The regeneration id the captured stream and answer carry.
jest.mock('expo-crypto', () => ({ randomUUID: () => require('@/api/__fixtures__/chat13').REGENERATION_ID }));
let mockStream = fakeStreamTransport();
jest.mock('@/api/sse/transport', () => ({
  expoStreamTransport: (url: string, init: unknown) => mockStream.transport(url, init as never),
}));

const P = CAPTURE.projectId;
const before = conversationDetail13;
const after = {
  ...conversationDetail13,
  message_groups: conversationDetail13.message_groups.map((g) => (g.uuid === CAPTURE.answerUuid ? answerGroupAfterRegenerate : g)),
};

function installServer(detailAfter: unknown, statusAfter = 200, regenerateAnswer: { status: number; body: unknown } = { status: 200, body: regenerateStarted }): FakeServer {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, { status: 200, body: before }, { status: statusAfter, body: detailAfter });
  server.on('GET', `/elitea_core/message_traces/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
  server.on('POST', `/elitea_core/regenerate/prompt_lib/${P}/${CAPTURE.answerUuid}`, regenerateAnswer);
  return server;
}

let queries: QueryClient | null = null;
async function renderWithQueries(ui: ReactElement) {
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  return render(<QueryClientProvider client={queries}>{ui}</QueryClientProvider>);
}

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  clearLiveTurns();
  resetPolicy();
  mockStream = fakeStreamTransport();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13()) });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'debug').mockImplementation(() => {});
});
afterEach(() => {
  queries?.clear();
  queries = null;
  clearLiveTurns();
  jest.restoreAllMocks();
});

async function regenerate(server: FakeServer) {
  await renderWithQueries(<ThreadScreen />);
  await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
  await fireEvent.press(screen.getByRole('button', { name: 'Regenerate answer' }));
  await waitFor(() => expect(mockStream.connections).toHaveLength(1));
  const post = server.requests.find((r) => r.method === 'POST')!;
  expect(JSON.parse(post.body!)).toMatchObject({ question_id: CAPTURE.questionUuid, message_id: CAPTURE.answerUuid, regeneration_id: REGENERATION_ID });
  mockStream.connections[0].push(loadFixtureText('regenerate-answer'));
}

describe('Regenerate the newest answer', () => {
  it('streams the new answer and proves the persisted one was replaced (generation = regeneration id)', async () => {
    const server = installServer(after);
    await regenerate(server);
    await waitFor(() => expect(server.requests.filter((r) => r.url.pathname.endsWith(`/conversation/prompt_lib/${P}/3`)).length).toBeGreaterThanOrEqual(2));
    await waitFor(() => expect(screen.queryByText(/Thinking|Sending/)).toBeNull());
    expect(screen.queryByText(/didn't replace this answer/)).toBeNull();
    expect(screen.getAllByRole('button', { name: 'Answer actions' })).toHaveLength(1);
  });

  it("says nothing when the transcript could not be read again (the old copy proves nothing)", async () => {
    const server = installServer({}, 503);
    await regenerate(server);
    await waitFor(() => expect(server.requests.filter((r) => r.url.pathname.endsWith(`/conversation/prompt_lib/${P}/3`)).length).toBeGreaterThanOrEqual(2));
    await waitFor(() => expect(screen.queryByText(/Thinking|Sending/)).toBeNull());
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 50));
    });
    expect(screen.queryByText(/didn't replace this answer/)).toBeNull();
  });

  it('a regeneration the server refused (send failed, nothing written) shows the original answer again', async () => {
    const server = installServer(after, 200, { status: 400, body: { error: 'bad_request' } });
    await renderWithQueries(<ThreadScreen />);
    await fireEvent.press(await screen.findByRole('button', { name: 'Answer actions' }));
    await fireEvent.press(screen.getByRole('button', { name: 'Regenerate answer' }));
    await waitFor(() => expect(server.requests.some((r) => r.method === 'POST')).toBe(true));
    // The failed turn is still live (Dismiss not tapped), and the answer it never touched is back.
    expect(await screen.findByRole('button', { name: 'Dismiss' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Answer actions' })).toBeTruthy();
    expect(screen.getByText(/MOCK: Show me code/)).toBeTruthy();
  });

  it('says so when the transcript still holds the old answer', async () => {
    const server = installServer(before);
    await regenerate(server);
    expect(await screen.findByText("The workspace didn't replace this answer. Try again.")).toBeTruthy();
  });
});
