import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import { discoveryDocument11, discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import ThreadScreen from '@/app/(app)/c/[id]';
import { loadFixtureText } from '@/features/chat/__fixtures__/loadFixture';
import { clearLiveTurns } from '@/features/chat/turnController';
import { resetPolicy } from '@/security/policy';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';
import { fakeStreamTransport } from '@/test/fakeStreamTransport';

const mockDismissTo = jest.fn();
jest.mock('expo-router', () => ({
  useLocalSearchParams: () => ({ id: '3' }),
  Stack: { Screen: () => null },
  router: { push: jest.fn(), replace: jest.fn(), dismissTo: (...args: unknown[]) => mockDismissTo(...args) },
}));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-crypto', () => ({ randomUUID: () => '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b' }));
let mockStream = fakeStreamTransport();
jest.mock('@/api/sse/transport', () => ({
  expoStreamTransport: (url: string, init: unknown) => mockStream.transport(url, init as never),
}));

// F2: "View usage" opens Settings, whose usage card exists only on client
// contract 1.3. Servers from v3.1.0 already refuse turns with the budget codes,
// so on a 1.1/1.2 server the link would open a Settings screen without usage.

const P = 90107;
const UUID = '7cbc0244-366e-40b9-ba65-35fcbfb50ce1';
const MODEL = 'vllm/E2E-MOCK-MODEL';

function installServer() {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, {
    status: 200,
    body: {
      id: '3',
      uuid: UUID,
      project_id: String(P),
      name: 'budget',
      created_at: '2026-10-05T07:00:00Z',
      updated_at: '2026-10-05T07:00:00Z',
      participants: [{ id: 1, entity_name: 'user', entity_meta: { id: 7 }, entity_settings: {}, meta: {} }],
      message_groups: [],
    },
  });
  server.on('GET', `/elitea_core/messages/prompt_lib/${P}/3`, { status: 200, body: { total: 0, rows: [] } });
  server.on('GET', `/elitea_core/message_traces/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
  server.on('GET', `/configurations/models/${P}`, {
    status: 200,
    body: {
      total: 1,
      items: [{ name: MODEL, display_name: MODEL, project_id: P, shared: false, default: true }],
      default_model_name: MODEL,
      default_model_project_id: P,
    },
  });
  server.on('POST', `/elitea_core/messages/prompt_lib/${P}/${UUID}`, {
    status: 200,
    body: {
      execution_id: 'e1',
      events_url: `/api/v2/executions/${P}/e1/events`,
      response_message_id: '0661c34b-1930-5cfe-9dca-c204d0ed0047',
      created: true,
    },
  });
  return server;
}

let queries: QueryClient | null = null;
async function renderWithQueries(ui: ReactElement) {
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  return render(<QueryClientProvider client={queries}>{ui}</QueryClientProvider>);
}

async function refusedTurn() {
  installServer();
  await renderWithQueries(<ThreadScreen />);
  await fireEvent.changeText(await screen.findByLabelText('Message'), 'hello');
  await fireEvent.press(screen.getByRole('button', { name: /send/i }));
  await waitFor(() => expect(mockStream.connections).toHaveLength(1));
  mockStream.connections[0].push(loadFixtureText('budget-refusal'));
  expect(await screen.findByText(/^Your budget in this project is used up/)).toBeTruthy();
}

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  clearLiveTurns();
  resetPolicy();
  mockStream = fakeStreamTransport();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'debug').mockImplementation(() => {});
});
afterEach(() => {
  queries?.clear();
  queries = null;
  clearLiveTurns();
  jest.restoreAllMocks();
});

it('a budget refusal on a 1.3 server links to the usage card', async () => {
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13()) });
  await refusedTurn();
  expect(screen.getByRole('button', { name: 'View usage' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
  // Settings is a tab beneath the thread: the link goes back down to it, never pushes a second tab bar.
  await fireEvent.press(screen.getByRole('button', { name: 'View usage' }));
  expect(mockDismissTo).toHaveBeenCalledWith('/settings');
});

it('a budget refusal on a pre-1.3 server keeps the message but offers no usage link (Settings has no usage card there)', async () => {
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument11()) });
  await refusedTurn();
  expect(screen.queryByRole('button', { name: 'View usage' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Dismiss' })).toBeTruthy();
});
