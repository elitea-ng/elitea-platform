import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import { discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import NewConversationScreen from '@/app/(app)/new';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

import { createAgent } from './useAgentBuilder';

const mockReplace = jest.fn();
jest.mock('expo-router', () => ({ useIsFocused: () => true, router: { replace: (...args: unknown[]) => mockReplace(...args) }, useLocalSearchParams: () => ({}) }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const P = 90504;

function install(contract: string): FakeServer {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', `/elitea_core/applications/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
  server.on('GET', `/configurations/models/${P}`, { status: 200, body: { total: 0, items: [] } });
  server.on('GET', `/elitea_core/recommendations/prompt_lib/${P}`, { status: 200, body: { applications: [], total: 0 } });
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13({ client_contract: contract })) });
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

describe('Create an agent (client contract 1.28)', () => {
  it('is offered on New chat by a 1.28 workspace and replaces the sheet with the builder', async () => {
    install('1.28');
    await renderWithQueries(<NewConversationScreen />);
    await fireEvent.press(await screen.findByText('Create an agent'));
    expect(mockReplace).toHaveBeenCalledWith('/agent-new');
  });

  it('is not offered by an older workspace', async () => {
    install('1.27');
    await renderWithQueries(<NewConversationScreen />);
    expect(await screen.findByText('No agents or models found')).toBeTruthy();
    expect(screen.queryByText('Create an agent')).toBeNull();
  });

  it('saves the reviewed draft as an agent with a base version', async () => {
    const server = fakeServer();
    server.on('POST', `/elitea_core/applications/prompt_lib/${P}`, { status: 201, body: { id: '12', name: 'Triager' } });
    await expect(
      createAgent(P, { name: ' Triager ', description: 'Triages incidents', instructions: 'Sort by severity.', conversation_starters: ['Triage this', ' '] }, server.client),
    ).resolves.toBe(12);
    const sent = JSON.parse(server.requests[0]?.body ?? '{}');
    expect(sent).toMatchObject({
      name: 'Triager',
      type: 'interface',
      versions: [{ name: 'base', instructions: 'Sort by severity.', conversation_starters: ['Triage this'], meta: { step_limit: 25 } }],
    });
  });
});
