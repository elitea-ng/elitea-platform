import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react-native';

import { ThreadView } from '@/features/chat/ThreadView';
import { clearLiveTurns } from '@/features/chat/turnController';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

// Issue #15: the same thread drawn as the detail column beside the Chats list (regular width).
// Its title and actions go in the column's own bar, never in the navigation bar, which is the
// list's.

const mockStackOptions: unknown[] = [];
jest.mock('expo-router', () => ({
  router: { replace: jest.fn(), push: jest.fn() },
  useLocalSearchParams: () => ({}),
  Stack: {
    Screen: ({ options }: { options?: unknown }) => {
      mockStackOptions.push(options);
      return null;
    },
  },
}));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-crypto', () => ({ randomUUID: () => '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b' }));

const P = 90107;
const UUID = '7cbc0244-366e-40b9-ba65-35fcbfb50ce1';
const MODEL = 'vllm/E2E-MOCK-MODEL';

let queries: QueryClient | null = null;

beforeEach(() => {
  __resetSessionForTests();
  clearLiveTurns();
  mockStackOptions.length = 0;
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', `/elitea_core/conversation/prompt_lib/${P}/3`, {
    status: 200,
    body: {
      id: '3',
      uuid: UUID,
      project_id: String(P),
      name: 'Q3 planning',
      created_at: '2026-10-05T07:00:00Z',
      updated_at: '2026-10-05T07:00:00Z',
      participants: [{ id: 1, entity_name: 'user', entity_meta: { id: 7 }, entity_settings: {}, meta: {} }],
      message_groups: [],
    },
  });
  server.on('GET', `/elitea_core/messages/prompt_lib/${P}/3`, { status: 200, body: { total: 0, rows: [] } });
  server.on('GET', `/configurations/models/${P}`, {
    status: 200,
    body: { total: 1, items: [{ name: MODEL, display_name: MODEL, project_id: P, shared: false, default: true }], default_model_name: MODEL, default_model_project_id: P },
  });
});

afterEach(() => {
  queries?.clear();
  queries = null;
  clearLiveTurns();
  jest.restoreAllMocks();
});

async function renderThread(chrome: 'stack' | 'pane') {
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  return render(
    <QueryClientProvider client={queries}>
      <ThreadView id="3" chrome={chrome} />
    </QueryClientProvider>,
  );
}

describe('the thread as a detail column', () => {
  it('names the chat in its own bar (a heading) and leaves the navigation bar alone', async () => {
    await renderThread('pane');
    expect(await screen.findByRole('header', { name: 'Q3 planning' })).toBeTruthy();
    expect(screen.getByTestId('pane-header')).toBeTruthy();
    expect(screen.getByLabelText('Message')).toBeTruthy();
    expect(mockStackOptions).toEqual([]);
  });

  it('on its own screen the title still goes in the navigation bar, with no column bar', async () => {
    await renderThread('stack');
    await screen.findByLabelText('Message');
    expect(screen.queryByTestId('pane-header')).toBeNull();
    expect(mockStackOptions).toContainEqual(expect.objectContaining({ title: 'Q3 planning' }));
  });
});
