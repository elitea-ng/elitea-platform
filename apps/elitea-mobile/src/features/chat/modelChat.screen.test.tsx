import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';

import NewConversationScreen from '@/app/(app)/new';
import ThreadScreen from '@/app/(app)/c/[id]';
import { clearLiveTurns } from '@/features/chat/turnController';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

const mockReplace = jest.fn();
let mockParams: Record<string, string> = {};
jest.mock('expo-router', () => ({
  router: { replace: (...args: unknown[]) => mockReplace(...args) },
  useLocalSearchParams: () => mockParams,
  Stack: { Screen: () => null },
}));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-crypto', () => ({ randomUUID: () => '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b' }));

// DEF-3 (Agent Zefir E2E): in a project with a chat model and no agent, New
// conversation said "No agents found." and a model-only conversation said
// "This conversation has no agent you can message from mobile."

const P = 90107;
const UUID = '7cbc0244-366e-40b9-ba65-35fcbfb50ce1';
const MODEL = 'vllm/E2E-MOCK-MODEL';

const catalogue = {
  total: 1,
  items: [{ name: MODEL, display_name: MODEL, project_id: P, shared: false, default: true }],
  default_model_name: MODEL,
  default_model_project_id: P,
};

function installServer(): FakeServer {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  return server;
}

let queries: QueryClient | null = null;
async function renderWithQueries(ui: ReactElement) {
  // No garbage-collection timers: nothing outlives the test.
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  return render(<QueryClientProvider client={queries}>{ui}</QueryClientProvider>);
}

beforeEach(() => {
  __resetSessionForTests();
  clearLiveTurns();
  mockReplace.mockReset();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});

afterEach(() => {
  queries?.clear();
  queries = null;
  clearLiveTurns();
  jest.restoreAllMocks();
});

describe('chat with a model (no agent)', () => {
  it('New conversation offers the project model and starts a model conversation', async () => {
    const server = installServer();
    server.on('GET', `/elitea_core/applications/prompt_lib/${P}`, { status: 200, body: { total: 0, rows: [] } });
    server.on('GET', `/configurations/models/${P}`, { status: 200, body: catalogue });
    server.on('POST', `/elitea_core/conversations/prompt_lib/${P}`, { status: 201, body: { id: '5', uuid: UUID } });

    await renderWithQueries(<NewConversationScreen />);
    const choice = await screen.findByText(MODEL);
    expect(screen.queryByText(/No agents/)).toBeNull();
    await fireEvent.press(choice);

    await waitFor(() => expect(mockReplace).toHaveBeenCalledWith({ pathname: '/c/[id]', params: { id: '5' } }));
    const created = server.requests.find((r) => r.method === 'POST');
    expect(JSON.parse(created?.body ?? '{}').participants).toEqual([
      {
        entity_name: 'dummy',
        entity_meta: {},
        entity_settings: { llm_settings: { model_name: MODEL, model_project_id: P, stream: true } },
      },
    ]);
  });

  it('a conversation with only its user can be messaged on the default model', async () => {
    const server = installServer();
    mockParams = { id: '3' };
    server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, {
      status: 200,
      body: {
        id: '3',
        uuid: UUID,
        project_id: String(P),
        name: 'curl smoke',
        created_at: '2026-10-05T07:00:00Z',
        updated_at: '2026-10-05T07:00:00Z',
        participants: [{ id: 1, entity_name: 'user', entity_meta: { id: 7 }, entity_settings: {}, meta: {} }],
        message_groups: [],
      },
    });
    server.on('GET', `/elitea_core/messages/prompt_lib/${P}/3`, { status: 200, body: { total: 0, rows: [] } });
    server.on('GET', `/configurations/models/${P}`, { status: 200, body: catalogue });
    // Refused, so no stream is opened; the request is what this test checks.
    server.on('POST', `/elitea_core/messages/prompt_lib/${P}/${UUID}`, {
      status: 422,
      body: { error: 'rejected_for_test' },
    });

    await renderWithQueries(<ThreadScreen />);
    const input = await screen.findByLabelText('Message');
    expect(screen.queryByText(/no agent/i)).toBeNull();
    await fireEvent.changeText(input, 'hello model');
    await fireEvent.press(screen.getByRole('button', { name: /send/i }));

    await waitFor(() => expect(server.requests.some((r) => r.method === 'POST')).toBe(true));
    const send = server.requests.find((r) => r.method === 'POST')!;
    expect(send.url.searchParams.get('execution_contract')).toBe('agent.execute.adhoc.v1');
    expect(JSON.parse(send.body ?? '{}')).toMatchObject({
      participant_id: 0,
      payload: { user_input: 'hello model' },
      llm_settings: { model_name: MODEL, model_project_id: P, stream: true },
    });
  });
});
