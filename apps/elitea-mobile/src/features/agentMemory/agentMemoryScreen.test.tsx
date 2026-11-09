import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import type { ReactElement } from 'react';
import { Alert } from 'react-native';

import { discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import AgentMemoryScreen from '@/app/(app)/agent-memory/[id]';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

let mockParams: Record<string, string> = { id: '41', name: 'Mira' };
let mockTitle: unknown = null;
jest.mock('expo-router', () => ({
  useLocalSearchParams: () => mockParams,
  Stack: {
    Screen: ({ options }: { options: { title?: string } }) => {
      mockTitle = options.title;
      return null;
    },
  },
}));
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
const ME = 5;
jest.mock('@/state/selfIdentity', () => ({ useSelfUserId: () => 5 }));

const P = 7;
const LIST = `/elitea_core/agent_memory/prompt_lib/${P}/41`;
const OPT_OUT = `/elitea_core/agent_memory_opt_out/prompt_lib/${P}/41`;

const entry = (id: number, extra: Record<string, unknown> = {}) => ({
  id,
  application_id: 41,
  kind: 'fact',
  content: `Note ${id}`,
  subject_user_id: null,
  pinned: false,
  status: 'active',
  version: 1,
  conversation_uuid: null,
  message_id: null,
  execution_id: null,
  written_by_kind: 'agent',
  written_by_user_id: null,
  created_at: '2026-11-01T10:00:00Z',
  updated_at: '2026-11-01T10:00:00Z',
  ...extra,
});
const list = (rows: unknown[], extra: Record<string, unknown> = {}) => ({
  rows,
  total: rows.length,
  people: [{ id: 6, name: 'Bo' }],
  can_manage: false,
  can_delete: false,
  can_approve: false,
  opted_out: false,
  ...extra,
});

const MINE = entry(1, { content: 'Prefers short answers', kind: 'preference', subject_user_id: ME });
const PROJECT = entry(2, { content: 'The release ships on Thursdays', pinned: true });
const BOS = entry(3, { content: 'Bo works in Berlin', subject_user_id: 6, written_by_kind: 'person', written_by_user_id: 6, status: 'pending' });

function install(contract = '1.44'): FakeServer {
  const server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13({ client_contract: contract })) });
  return server;
}

let queries: QueryClient;
const withQueries = (ui: ReactElement) => <QueryClientProvider client={queries}>{ui}</QueryClientProvider>;
const settle = () => act(() => new Promise<void>((resolve) => setImmediate(() => resolve())));

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  mockParams = { id: '41', name: 'Mira' };
  queries = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, workspaceId: 'aaaaaaaaaaaa', projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.restoreAllMocks();
});
afterEach(() => queries.clear());

it('lists the notes the caller may see, with what each is, who it is about and who wrote it', async () => {
  const server = install();
  server.on('GET', LIST, { status: 200, body: list([MINE, PROJECT, BOS], { can_manage: true }) });
  await render(withQueries(<AgentMemoryScreen />));
  expect(await screen.findByText('Prefers short answers')).toBeTruthy();
  expect(mockTitle).toBe("Mira's memory");
  expect(screen.getByText(/^3 notes$/i)).toBeTruthy();
  expect(screen.getByText(/^Preference · About you · The agent · /)).toBeTruthy();
  expect(screen.getByText(/^Fact · Pinned · The agent · /)).toBeTruthy();
  expect(screen.getByText(/^Fact · Waiting for approval · About Bo · Bo · /)).toBeTruthy();
  expect(server.requests[0].url.searchParams.get('limit')).toBe('50');
});

it('offers Delete only on a note about the caller without can_delete', async () => {
  const server = install();
  server.on('GET', LIST, { status: 200, body: list([MINE, PROJECT]) });
  await render(withQueries(<AgentMemoryScreen />));
  await screen.findByText('Prefers short answers');
  expect(screen.getByRole('button', { name: 'Delete note: Prefers short answers' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Delete note: The release ships on Thursdays' })).toBeNull();
});

it('offers Delete on every note with can_delete', async () => {
  const server = install();
  server.on('GET', LIST, { status: 200, body: list([MINE, PROJECT], { can_delete: true }) });
  await render(withQueries(<AgentMemoryScreen />));
  await screen.findByText('Prefers short answers');
  expect(screen.getByRole('button', { name: 'Delete note: The release ships on Thursdays' })).toBeTruthy();
});

it('deletes a note only after the confirmation, and drops it from the list', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install();
  server.on('GET', LIST, { status: 200, body: list([MINE, PROJECT]) });
  server.on('DELETE', `${LIST}/1`, { status: 204 });
  await render(withQueries(<AgentMemoryScreen />));
  await fireEvent.press(await screen.findByRole('button', { name: 'Delete note: Prefers short answers' }));
  expect(server.requests.some((r) => r.method === 'DELETE')).toBe(false);
  expect(alert.mock.calls[0][0]).toBe('Delete this note?');
  await act(async () => alert.mock.calls[0][2]!.find((b) => b.text === 'Delete')!.onPress!());
  await settle();
  expect(server.requests.find((r) => r.method === 'DELETE')?.url.pathname).toBe(`/api/v2${LIST}/1`);
  await waitFor(() => expect(screen.queryByText('Prefers short answers')).toBeNull());
  expect(screen.getByText(/^1 note$/i)).toBeTruthy();
});

it("shows a refused delete in the server's own words and keeps the note", async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install();
  server.on('GET', LIST, { status: 200, body: list([MINE]) });
  server.on('DELETE', `${LIST}/1`, { status: 403, body: { error: 'You may not delete this note.', code: 'memory_change_forbidden' } });
  await render(withQueries(<AgentMemoryScreen />));
  await fireEvent.press(await screen.findByRole('button', { name: 'Delete note: Prefers short answers' }));
  await act(async () => alert.mock.calls[0][2]!.find((b) => b.text === 'Delete')!.onPress!());
  await settle();
  expect(await screen.findByRole('alert')).toHaveTextContent('You may not delete this note.');
  expect(screen.getByText('Prefers short answers')).toBeTruthy();
});

it('a confirmation answered after a project switch deletes nothing', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install();
  server.on('GET', LIST, { status: 200, body: list([MINE]) });
  await render(withQueries(<AgentMemoryScreen />));
  await fireEvent.press(await screen.findByRole('button', { name: 'Delete note: Prefers short answers' }));
  const buttons = alert.mock.calls[0][2]!;
  setSession({ projectId: 12 });
  await act(async () => buttons.find((b) => b.text === 'Delete')!.onPress!());
  expect(server.requests.some((r) => r.method === 'DELETE')).toBe(false);
});

it('turning "Remember things about me" off asks first, stores it and reads the list again', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install();
  server.on('GET', LIST, { status: 200, body: list([MINE, PROJECT]) }, { status: 200, body: list([PROJECT], { opted_out: true }) });
  server.on('PUT', OPT_OUT, { status: 200, body: { opted_out: true } });
  await render(withQueries(<AgentMemoryScreen />));
  await screen.findByText('Prefers short answers');
  const toggle = screen.getByTestId('agent-memory-opt-out');
  expect(toggle.props.value).toBe(true);
  await fireEvent(toggle, 'valueChange', false);
  expect(server.requests.some((r) => r.method === 'PUT')).toBe(false);
  await act(async () => alert.mock.calls[0][2]!.find((b) => b.text === 'Turn off')!.onPress!());
  await settle();
  const put = server.requests.find((r) => r.method === 'PUT');
  expect(JSON.parse(put?.body ?? '{}')).toEqual({ opted_out: true });
  await waitFor(() => expect(screen.queryByText('Prefers short answers')).toBeNull());
  expect(screen.getByTestId('agent-memory-opt-out').props.value).toBe(false);
  expect(screen.getByText(/Mira doesn't remember anything about you/)).toBeTruthy();
});

it('turning it back on needs no confirmation', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install();
  server.on('GET', LIST, { status: 200, body: list([], { opted_out: true }) });
  server.on('PUT', OPT_OUT, { status: 200, body: { opted_out: false } });
  await render(withQueries(<AgentMemoryScreen />));
  await screen.findByText('Nothing remembered yet');
  await fireEvent(screen.getByTestId('agent-memory-opt-out'), 'valueChange', true);
  await settle();
  expect(alert).not.toHaveBeenCalled();
  expect(JSON.parse(server.requests.find((r) => r.method === 'PUT')?.body ?? '{}')).toEqual({ opted_out: false });
  await waitFor(() => expect(screen.getByTestId('agent-memory-opt-out').props.value).toBe(true));
});

it("shows a refused list in the server's words, with Try again", async () => {
  const server = install();
  server.on('GET', LIST, { status: 403, body: { error: 'insufficient permissions', code: 'forbidden' } });
  await render(withQueries(<AgentMemoryScreen />));
  expect(await screen.findByText('insufficient permissions')).toBeTruthy();
  expect(screen.getByText('Try again')).toBeTruthy();
});

it('loads the next page when scrolled to the end', async () => {
  const server = install();
  const first = Array.from({ length: 50 }, (_, i) => entry(i + 1));
  // The last route registered answers first: the paged one goes after the plain one.
  server.on('GET', LIST, { status: 200, body: list(first, { total: 51 }) });
  server.on('GET', `${LIST}?offset=50`, { status: 200, body: list([entry(51, { content: 'The last note' })], { total: 51 }) });
  await render(withQueries(<AgentMemoryScreen />));
  await screen.findByText('Note 1');
  await fireEvent.scroll(screen.getByTestId('agent-memory-list'), { nativeEvent: { contentOffset: { y: 2000 }, contentSize: { height: 2400 }, layoutMeasurement: { height: 800 } } });
  expect(await screen.findByText('The last note')).toBeTruthy();
});

it('a server before client contract 1.44 shows no agent memory and asks nothing', async () => {
  const server = install('1.43');
  await render(withQueries(<AgentMemoryScreen />));
  expect(screen.getByText("This workspace doesn't show agent memory in the app yet.")).toBeTruthy();
  expect(server.requests).toHaveLength(0);
});

it('no discovery document yet reads as offline', async () => {
  install();
  __resetWorkspaceInfoForTests();
  await render(withQueries(<AgentMemoryScreen />));
  expect(screen.getByText('Agent memory needs a connection to your workspace.')).toBeTruthy();
});

it('a delete answered 404 not_found (already gone) drops the note without a banner', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install();
  server.on('GET', LIST, { status: 200, body: list([MINE, PROJECT]) });
  server.on('DELETE', `${LIST}/1`, { status: 404, body: { error: 'The memory entry does not exist.', code: 'not_found' } });
  await render(withQueries(<AgentMemoryScreen />));
  await fireEvent.press(await screen.findByRole('button', { name: 'Delete note: Prefers short answers' }));
  await act(async () => alert.mock.calls[0][2]!.find((b) => b.text === 'Delete')!.onPress!());
  await settle();
  await waitFor(() => expect(screen.queryByText('Prefers short answers')).toBeNull());
  expect(screen.queryByRole('alert')).toBeNull();
});

it('a pull to refresh clears a failed write banner', async () => {
  const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
  const server = install();
  server.on('GET', LIST, { status: 200, body: list([MINE]) });
  server.on('DELETE', `${LIST}/1`, { status: 403, body: { error: 'You may not delete this note.', code: 'memory_change_forbidden' } });
  await render(withQueries(<AgentMemoryScreen />));
  await fireEvent.press(await screen.findByRole('button', { name: 'Delete note: Prefers short answers' }));
  await act(async () => alert.mock.calls[0][2]!.find((b) => b.text === 'Delete')!.onPress!());
  await settle();
  expect(await screen.findByRole('alert')).toBeTruthy();
  await act(async () => screen.getByTestId('agent-memory-list').props.refreshControl.props.onRefresh());
  await settle();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('a project switch while open shows "moved" and reads nothing from the new project', async () => {
  const server = install();
  server.on('GET', LIST, { status: 200, body: list([MINE]) });
  await render(withQueries(<AgentMemoryScreen />));
  await screen.findByText('Prefers short answers');
  await act(async () => setSession({ projectId: 12 }));
  expect(screen.getByText('You switched project or workspace. Go back and open this agent again.')).toBeTruthy();
  expect(server.requests.every((r) => r.url.pathname.includes(`/prompt_lib/${P}/`))).toBe(true);
});

it('shows a note once when a page boundary shifted between reads', async () => {
  const server = install();
  const first = Array.from({ length: 50 }, (_, i) => entry(i + 1));
  server.on('GET', LIST, { status: 200, body: list(first, { total: 52 }) });
  server.on('GET', `${LIST}?offset=50`, { status: 200, body: list([entry(50), entry(51, { content: 'The last note' })], { total: 52 }) });
  await render(withQueries(<AgentMemoryScreen />));
  await screen.findByText('Note 1');
  await fireEvent.scroll(screen.getByTestId('agent-memory-list'), {
    nativeEvent: { contentOffset: { y: 2000 }, contentSize: { height: 2400 }, layoutMeasurement: { height: 800 } },
  });
  expect(await screen.findByText('The last note')).toBeTruthy();
  expect(screen.getAllByText('Note 50')).toHaveLength(1);
});
