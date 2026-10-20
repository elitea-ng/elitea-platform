import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import { applicationDetail } from '@/api/__fixtures__/chat11';
import { recommendationsPersonal, recommendationsTeam } from '@/api/__fixtures__/chat13';
import { discoveryDocument11, discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import { fetchRecommendations, recommendationsOf } from '@/api/queries/recommendations';
import NewConversationScreen from '@/app/(app)/new';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

const mockReplace = jest.fn();
jest.mock('expo-router', () => ({ router: { replace: (...args: unknown[]) => mockReplace(...args) }, useLocalSearchParams: () => ({}) }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const P = 90504;
const UUID = '7cbc0244-366e-40b9-ba65-35fcbfb50ce1';

function install(contract: '1.1' | '1.3'): FakeServer {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', `/elitea_core/applications/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
  server.on('GET', `/configurations/models/${P}`, { status: 200, body: { total: 0, items: [] } });
  server.on('GET', `/elitea_core/recommendations/prompt_lib/${P}`, { status: 200, body: recommendationsTeam });
  server.on('GET', `/elitea_core/application/prompt_lib/${P}/5`, { status: 200, body: applicationDetail(5, 6) });
  server.on('POST', `/elitea_core/conversations/prompt_lib/${P}`, { status: 201, body: { id: '9', uuid: UUID } });
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(contract === '1.3' ? discoveryDocument13() : discoveryDocument11()) });
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
  mockReplace.mockReset();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});
afterEach(() => {
  queries?.clear();
  queries = null;
  jest.restoreAllMocks();
});

describe('getRecommendations (captured)', () => {
  it('reads the envelope with string ids as agents', () => {
    expect(recommendationsOf(recommendationsPersonal)).toEqual([
      { id: 1, name: 'Zefir Tool Agent', description: 'Calls the mock tool; every call needs your approval (HITL).' },
    ]);
    expect(recommendationsOf(recommendationsTeam).map((a) => a.id)).toEqual([2, 1, 5]);
  });

  it('drops rows it cannot start, and treats a failure as no suggestions', async () => {
    expect(recommendationsOf({ applications: [{ id: 'x', name: 'a' }, { id: '0', name: 'b' }, { id: '3', name: ' ' }, { id: 4, name: 'ok' }], total: 4 })).toEqual([
      { id: 4, name: 'ok', description: '' },
    ]);
    expect(recommendationsOf([{ id: 1 }])).toEqual([]);
    const server = fakeServer();
    server.on('GET', `/elitea_core/recommendations/prompt_lib/${P}`, { status: 403, body: {} });
    await expect(fetchRecommendations(P, server.client)).resolves.toEqual([]);
  });
});

describe('Suggested agents on the new-chat screen', () => {
  it('lists the suggestions and starts a conversation with the one tapped', async () => {
    const server = install('1.3');
    await renderWithQueries(<NewConversationScreen />);
    expect(await screen.findByText('Suggested')).toBeTruthy();
    await fireEvent.press(screen.getByRole('button', { name: 'Zefir Concierge, Zefir Concierge: a public agent anyone can find in Discover.' }));
    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith({ pathname: '/c/[id]', params: { id: '9' } }));
    const created = JSON.parse(server.requests.find((r) => r.method === 'POST')!.body!);
    expect(created.participants).toEqual([{ entity_name: 'application', entity_meta: { id: 5, project_id: P }, entity_settings: { version_id: 6 } }]);
  });

  it('hides them while searching', async () => {
    install('1.3');
    await renderWithQueries(<NewConversationScreen />);
    await screen.findByText('Suggested');
    await fireEvent.changeText(screen.getByLabelText('Search agents and models'), 'zef');
    await waitFor(() => expect(screen.queryByText('Suggested')).toBeNull());
  });

  it('a server before 1.3: no suggestions and no request for them', async () => {
    const server = install('1.1');
    await renderWithQueries(<NewConversationScreen />);
    await screen.findByText(/No agents or models found/);
    expect(screen.queryByText('Suggested')).toBeNull();
    expect(server.requests.some((r) => r.url.pathname.includes('recommendations'))).toBe(false);
  });
});
