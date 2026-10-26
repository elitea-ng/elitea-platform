import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react-native';

import { queryKeys } from '@/api/queries/keys';
import PullRequestCredentialScreen from '@/app/(app)/pr-credential';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

// The `/pr-credential` sheet (contract 1.29): the owner picks the GitHub or GitLab configuration
// a cloud thread pushes and opens pull requests through; only the reference is sent.

const mockBack = jest.fn();
jest.mock('expo-router', () => ({
  useLocalSearchParams: () => ({ uuid: '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b' }),
  router: { canGoBack: () => true, back: () => mockBack() },
}));
jest.mock('@/state/selfIdentity', () => ({ usePersonalProjectId: () => 9 }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const P = 7;
const UUID = '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b';
const PUT = `/elitea_core/workspace_handoff_pull_request_credential/prompt_lib/${P}/${UUID}`;
const page = (titles: string[], shared: string[] = []) => ({
  status: 200,
  body: { items: titles.map((t) => ({ elitea_title: t })), limit: 200, offset: 0, total: titles.length, shared: { items: shared.map((t) => ({ elitea_title: t })), total: shared.length } },
});

let server: FakeServer;
let queries: QueryClient;

async function open(answer: { status: number; body: unknown }, current = { type: 'github', elitea_title: 'work', private: false }) {
  server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  server.on('GET', `/configurations/configurations/${P}?type=github`, page(['work'], ['org']));
  server.on('GET', `/configurations/configurations/${P}?type=gitlab`, page([]));
  server.on('GET', `/configurations/configurations/9?type=github`, page([]));
  server.on('GET', `/configurations/configurations/9?type=gitlab`, page(['mine']));
  server.on('PUT', PUT, answer);
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  queries.setQueryData(queryKeys.handoff(P, UUID), { state: 'CLOUD_IDLE', epoch: 3, pull_request_credential: current });
  return await render(
    <QueryClientProvider client={queries}>
      <PullRequestCredentialScreen />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  mockBack.mockClear();
});
afterEach(() => queries.clear());

it('lists the project, shared and own configurations and saves the reference', async () => {
  await open({ status: 200, body: { pull_request_credential: { type: 'gitlab', elitea_title: 'mine', private: true } } });
  expect(await screen.findByText('mine (GitLab, yours)')).toBeTruthy();
  expect(screen.getByText('work (GitHub)')).toBeTruthy();
  expect(screen.getByText('org (GitHub)')).toBeTruthy();
  await fireEvent.press(screen.getByTestId('pr-credential-gitlab:private:mine'));
  await waitFor(() => expect(mockBack).toHaveBeenCalled());
  const put = server.requests.find((r) => r.method === 'PUT')!;
  expect(JSON.parse(put.body ?? '{}')).toEqual({ credential: { type: 'gitlab', elitea_title: 'mine', private: true } });
});

it('keeps the sheet open with the refusal', async () => {
  await open({ status: 403, body: { error: 'pull_requests_disabled' } });
  await fireEvent.press(await screen.findByTestId('pr-credential-none'));
  expect(await screen.findByText('Pushes and pull requests from the cloud are turned off for this project.')).toBeTruthy();
  expect(mockBack).not.toHaveBeenCalled();
  expect(JSON.parse(server.requests.find((r) => r.method === 'PUT')!.body ?? '{}')).toEqual({ credential: null });
});

it('shows a current choice the listing does not, and leaves it as it is', async () => {
  await open({ status: 200, body: {} }, { type: 'github', elitea_title: 'gone', private: true });
  expect(await screen.findByText('gone (GitHub, yours) (current, not listed)')).toBeTruthy();
  await fireEvent.press(screen.getByTestId('pr-credential-unlisted'));
  expect(server.requests.some((r) => r.method === 'PUT')).toBe(false);
});
