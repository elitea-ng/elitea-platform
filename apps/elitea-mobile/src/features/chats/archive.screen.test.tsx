import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react-native';
import { router } from 'expo-router';
import { Alert, Text } from 'react-native';

import { discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import ConversationsScreen from '@/app/(app)/(tabs)/chats/index';
import { ARCHIVED_COPY, ArchivedChatsView } from '@/features/chats/ArchivedChatsView';
import { buildArchivedRows, buildChatRows, buildSearchRows } from '@/features/chats/chatListModel';
import { ConversationList, type ChatListItem } from '@/features/chats/ConversationList';
import { __setOnlineForTests } from '@/lib/network';
import { getSelectedChat } from '@/features/chats/selectedChat';
import { __resetSessionForTests, getSession, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { runConversationAction } from '@/sync/appConversationOps';
import { FAKE_ORIGIN, fakeServer, type FakeServer } from '@/test/fakeServer';

// Archive and Archived chats (client contract 1.10, issue #1352): the row action, hidden without
// the permission the web app checks; the queued state over the lists; the Archived chats screen.

const P = 90510;
const now = new Date().toISOString();
const row = (id: number, name: string, extra: Record<string, unknown> = {}) => ({
  id,
  name,
  updated_at: now,
  created_at: now,
  duration: -1,
  message_groups_count: 1,
  ...extra,
});

jest.mock('expo-router', () => ({ router: { push: jest.fn(), back: jest.fn() }, useIsFocused: () => true, Stack: { Screen: () => null } }));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));
let mockOverlay = new Map<number, { archived?: boolean; deleted?: boolean }>();
jest.mock('@/sync/appConversationOps', () => ({
  runConversationAction: jest.fn(async () => 'queued'),
  subscribeActionFailures: () => () => {},
  useConversationOverlay: () => mockOverlay,
  conversationOpsDrainer: { subscribe: () => () => {} },
}));

jest.mock('@/api/queries/conversations', () => {
  const actual = jest.requireActual('@/api/queries/conversations');
  const page = (rows: unknown[]) => ({
    data: { pages: [{ rows, total: rows.length, offset: 0 }] },
    isLoading: false,
    error: null,
    isRefetching: false,
    isFetchingNextPage: false,
    hasNextPage: false,
    refetch: jest.fn(),
    fetchNextPage: jest.fn(),
  });
  const stamp = new Date().toISOString();
  const chat = { id: 11, name: 'Q3 planning', updated_at: stamp, created_at: stamp, duration: -1, message_groups_count: 1 };
  return { ...actual, useConversations: () => page([chat]), useLocalWorkConversations: () => page([]) };
});
jest.mock('@/features/projects/useProjectSelection', () => ({ useProjectSelection: () => ({ projectId: 90510, noProject: false }) }));
jest.mock('@/features/workspaces/WorkspacePill', () => ({ WorkspacePill: () => null }));

let server: FakeServer;
let queries: QueryClient;

function speak(contract: string, permissions: string[] = ['models.chat.conversation.edit']) {
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13({ client_contract: contract })) });
  server.on('GET', `/auth/permissions/prompt_lib/${P}`, { status: 200, body: permissions.map((name) => ({ name, enabled: true })) });
}

async function openChats() {
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  await render(
    <QueryClientProvider client={queries}>
      <ConversationsScreen />
    </QueryClientProvider>,
  );
  await screen.findByText('Q3 planning');
  await act(async () => {});
}

async function openArchived() {
  queries = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  await render(
    <QueryClientProvider client={queries}>
      <ArchivedChatsView />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  __setOnlineForTests(true);
  mockOverlay = new Map();
  jest.mocked(runConversationAction).mockClear();
  jest.mocked(router.push).mockClear();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  server = fakeServer();
  globalThis.fetch = ((input: RequestInfo, init?: RequestInit) => server.fetch(new Request(input, init))) as typeof fetch;
});
afterEach(() => {
  queries?.clear();
  __setOnlineForTests(true);
});

describe('the Chats list leaves archived chats out', () => {
  it('drops rows the server or a queued archive marks archived, and keeps a queued unarchive', () => {
    const loaded = [row(1, 'Kept'), row(2, 'Archived elsewhere', { is_archived: true }), row(3, 'Archived here'), row(4, 'Pinned', { is_pinned: true })];
    const overlay = new Map([
      [3, { archived: true }],
      [4, { archived: true }],
    ]);
    expect(buildChatRows({ loaded, savedPinned: [], overlay }).map((r) => r.id)).toEqual([1]);
    expect(buildSearchRows({ hits: null, loaded, query: 'a', overlay }).map((r) => r.id)).toEqual([]);
    expect(buildSearchRows({ hits: null, loaded, query: 'kept', overlay }).map((r) => r.id)).toEqual([1]);
  });

  it('the Archived list keeps archived rows, drops one unarchived or deleted on this device, and has no Pinned section', () => {
    const loaded = [row(5, 'Old plan', { is_archived: true, is_pinned: true }), row(6, 'Back', { is_archived: true }), row(7, 'Gone', { is_archived: true })];
    const overlay = new Map([
      [6, { archived: false }],
      [7, { deleted: true }],
    ]);
    expect(buildArchivedRows({ loaded, overlay }).map((r) => [r.id, r.pinned, r.archived])).toEqual([[5, false, true]]);
  });
});

describe('the row action', () => {
  const ROWS: ChatListItem[] = [{ id: 1, name: 'Film club', updated_at: now, age: '2h' }];

  it('offers Archive (screen readers and the long-press menu) when the list allows it', async () => {
    const onToggleArchive = jest.fn();
    await render(<ConversationList rows={ROWS} onOpen={jest.fn()} actions={{ onDelete: null, onToggleArchive }} empty={<Text>none</Text>} />);
    const target = screen.getByRole('button', { name: 'Film club, 2h' });
    expect(target.props.accessibilityActions).toEqual([{ name: 'archive', label: 'Archive' }]);
    await fireEvent(target, 'accessibilityAction', { nativeEvent: { actionName: 'archive' } });
    expect(onToggleArchive).toHaveBeenCalledWith(ROWS[0]);
    await fireEvent(target, 'longPress');
    await fireEvent.press(screen.getByRole('button', { name: 'Archive' }));
    expect(onToggleArchive).toHaveBeenCalledTimes(2);
  });

  it('an archived row offers Unarchive', async () => {
    await render(
      <ConversationList rows={[{ ...ROWS[0], archived: true }]} onOpen={jest.fn()} actions={{ onDelete: null, onToggleArchive: jest.fn() }} empty={<Text>none</Text>} />,
    );
    expect(screen.getByRole('button', { name: 'Film club, 2h' }).props.accessibilityActions).toEqual([{ name: 'archive', label: 'Unarchive' }]);
  });
});

describe('the Chats tab', () => {
  it('archives a chat (queued, offline-safe) and links to Archived chats on a 1.10 workspace', async () => {
    speak('1.10');
    await openChats();
    // Offered once the permission read answered.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Q3 planning, now' }).props.accessibilityActions).toContainEqual({ name: 'archive', label: 'Archive' }));
    const target = screen.getByRole('button', { name: 'Q3 planning, now' });
    await act(async () => {
      await fireEvent(target, 'accessibilityAction', { nativeEvent: { actionName: 'archive' } });
    });
    expect(runConversationAction).toHaveBeenCalledWith(P, 11, { kind: 'archive', archived: true });
    await fireEvent.press(screen.getByTestId('archived-chats-link'));
    expect(router.push).toHaveBeenCalledWith('/archived');
  });

  it('a member without the archive permission sees the link but no Archive action', async () => {
    speak('1.10', ['models.chat.conversations.list']);
    await openChats();
    await waitFor(() => expect(queries.getQueryState(['p', P, 'permissions'])?.status).toBe('success'));
    expect(screen.getByRole('button', { name: 'Q3 planning, now' }).props.accessibilityActions).not.toContainEqual({ name: 'archive', label: 'Archive' });
    expect(screen.getByTestId('archived-chats-link')).toBeTruthy();
  });

  it('a server before 1.10 has neither (and no permission read)', async () => {
    speak('1.9');
    await openChats();
    expect(screen.getByRole('button', { name: 'Q3 planning, now' }).props.accessibilityActions).not.toContainEqual({ name: 'archive', label: 'Archive' });
    expect(screen.queryByTestId('archived-chats-link')).toBeNull();
    expect(server.requests.filter((r) => r.url.pathname.includes('/auth/permissions'))).toEqual([]);
  });
});

describe('Fork on a Chats row (client contract 1.11)', () => {
  const FORK = `/elitea_core/conversation_fork/prompt_lib/${P}/11`;

  it('forks the whole chat (no fork point) and opens the new one at once', async () => {
    speak('1.11');
    server.on('POST', FORK, { status: 201, body: row(31, 'Q3 planning (fork)') });
    await openChats();
    const target = screen.getByRole('button', { name: 'Q3 planning, now' });
    expect(target.props.accessibilityActions).toContainEqual({ name: 'fork', label: 'Fork' });
    await act(async () => {
      await fireEvent(target, 'accessibilityAction', { nativeEvent: { actionName: 'fork' } });
    });
    // Opened at once: pushed on a phone, selected in the detail column at regular width.
    await waitFor(() =>
      expect(
        jest.mocked(router.push).mock.calls.some(([to]) => JSON.stringify(to) === JSON.stringify({ pathname: '/c/[id]', params: { id: '31' } })) ||
          getSelectedChat({ workspaceId: getSession().workspaceId, projectId: P }) === 31,
      ).toBe(true),
    );
    expect(JSON.parse(server.requests.find((r) => r.method === 'POST')!.body!)).toEqual({});
  });

  it('offline it says a fork needs a connection and sends nothing', async () => {
    speak('1.11');
    const alert = jest.spyOn(Alert, 'alert').mockImplementation(() => {});
    await openChats();
    __setOnlineForTests(false);
    await act(async () => {
      await fireEvent(screen.getByRole('button', { name: 'Q3 planning, now' }), 'accessibilityAction', { nativeEvent: { actionName: 'fork' } });
    });
    await waitFor(() => expect(alert).toHaveBeenCalledWith("Couldn't fork the chat", expect.stringMatching(/needs a connection/)));
    expect(server.requests.filter((r) => r.method === 'POST')).toEqual([]);
    expect(router.push).not.toHaveBeenCalled();
    alert.mockRestore();
  });

  it('is not offered before 1.11', async () => {
    speak('1.10');
    await openChats();
    expect(screen.getByRole('button', { name: 'Q3 planning, now' }).props.accessibilityActions).not.toContainEqual({ name: 'fork', label: 'Fork' });
  });
});

describe('Archived chats', () => {
  it('lists the project’s archived chats of the caller (archived=only, mine) and unarchives one (queued)', async () => {
    speak('1.10');
    server.on('GET', `/elitea_core/conversations/prompt_lib/${P}?archived=only`, { status: 200, body: { total: 1, rows: [row(21, 'Old plan', { is_archived: true })] } });
    await openArchived();
    const target = await screen.findByRole('button', { name: 'Old plan, now' });
    const list = server.requests.find((r) => r.url.pathname.endsWith(`/conversations/prompt_lib/${P}`))!;
    expect(list.url.searchParams.get('archived')).toBe('only');
    expect(list.url.searchParams.get('mine')).toBe('true');
    // The permission is read before the action shows.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Old plan, now' }).props.accessibilityActions).toEqual([{ name: 'archive', label: 'Unarchive' }]));
    await act(async () => {
      await fireEvent(screen.getByRole('button', { name: 'Old plan, now' }), 'accessibilityAction', { nativeEvent: { actionName: 'archive' } });
    });
    expect(runConversationAction).toHaveBeenCalledWith(P, 21, { kind: 'archive', archived: false });
    await fireEvent.press(target);
    expect(router.push).toHaveBeenCalledWith({ pathname: '/c/[id]', params: { id: '21' } });
  });

  it('hides Unarchive without the archive permission (a project viewer)', async () => {
    speak('1.10', ['models.chat.conversations.list']);
    server.on('GET', `/elitea_core/conversations/prompt_lib/${P}?archived=only`, { status: 200, body: { total: 1, rows: [row(21, 'Old plan', { is_archived: true })] } });
    await openArchived();
    await screen.findByText('Old plan');
    await waitFor(() => expect(queries.getQueryState(['p', P, 'permissions'])?.status).toBe('success'));
    expect(screen.getByRole('button', { name: 'Old plan, now' }).props.accessibilityActions).toBeUndefined();
  });

  it('a chat unarchived on this device leaves the screen at once', async () => {
    speak('1.10');
    mockOverlay = new Map([[21, { archived: false }]]);
    server.on('GET', `/elitea_core/conversations/prompt_lib/${P}?archived=only`, {
      status: 200,
      body: { total: 2, rows: [row(21, 'Old plan', { is_archived: true }), row(22, 'Kept', { is_archived: true })] },
    });
    await openArchived();
    await screen.findByText('Kept');
    expect(screen.queryByText('Old plan')).toBeNull();
  });

  it('says when there is nothing archived', async () => {
    speak('1.10');
    server.on('GET', `/elitea_core/conversations/prompt_lib/${P}?archived=only`, { status: 200, body: { total: 0, rows: [] } });
    await openArchived();
    expect(await screen.findByText(ARCHIVED_COPY.emptyTitle)).toBeTruthy();
  });

  it('needs a connection: offline it says so, with Try again', async () => {
    speak('1.10');
    server.on('GET', `/elitea_core/conversations/prompt_lib/${P}?archived=only`, 'network_error');
    await openArchived();
    expect(await screen.findByText(ARCHIVED_COPY.offlineTitle)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeTruthy();
  });

  it('is not offered by a server before client contract 1.10 (nothing is fetched)', async () => {
    speak('1.9');
    await openArchived();
    expect(screen.getByText(ARCHIVED_COPY.unsupportedTitle)).toBeTruthy();
    expect(server.requests.filter((r) => r.url.pathname.includes('/conversations/'))).toEqual([]);
  });
});
