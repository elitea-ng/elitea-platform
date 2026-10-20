import { fireEvent, render, screen } from '@testing-library/react-native';

import { discoveryDocument13 } from '@/api/__fixtures__/nativeClient';
import { DiscoveryDocumentSchema } from '@/api/discovery';
import ConversationsScreen from '@/app/(app)/(tabs)/chats/index';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { __resetWorkspaceInfoForTests, setWorkspaceInfo } from '@/state/workspaceInfo';
import { FAKE_ORIGIN } from '@/test/fakeServer';

// The Chats list's "Local work" filter: the desktop's threads are a separate list (the server
// leaves them out of the Chats listing), each with a badge for where it runs.

const P = 90502;
const now = new Date().toISOString();
const row = (id: number, name: string, extra: Record<string, unknown> = {}) => ({ id, name, updated_at: now, created_at: now, duration: -1, message_groups_count: 1, ...extra });
const mockLocalWorkEnabled = jest.fn();
const mockSearchKind = jest.fn();

jest.mock('expo-router', () => ({
  router: { push: jest.fn(), back: jest.fn() },
  useIsFocused: () => true,
  Stack: { Screen: () => null },
}));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));
const query = (rows: unknown[]) => ({
  data: { pages: [{ rows, total: rows.length }] },
  isLoading: false,
  error: null,
  isRefetching: false,
  isFetchingNextPage: false,
  hasNextPage: false,
  refetch: jest.fn(),
  fetchNextPage: jest.fn(),
});
jest.mock('@/api/queries/conversations', () => ({
  useConversations: () => query([row(11, 'Q3 planning')]),
  useLocalWorkConversations: (_project: number | null, enabled: boolean) => {
    mockLocalWorkEnabled(enabled);
    return query([row(61, 'Refactor parser', { workspace_executor: 'cloud' }), row(62, 'Write docs', { workspace_executor: 'local' })]);
  },
  flattenConversationPages: (pages: { rows: unknown[] }[] | undefined) => (pages ?? []).flatMap((page) => page.rows),
}));
jest.mock('@/features/projects/useProjectSelection', () => ({ useProjectSelection: () => ({ projectId: 90502, noProject: false }) }));
jest.mock('@/features/chats/useChatListData', () => ({
  useListActionsAvailable: () => true,
  useSavedPinnedRows: () => [],
  useConversationSearch: (_project: number | null, _query: string, kind?: string) => {
    mockSearchKind(kind);
    return null;
  },
}));
jest.mock('@/sync/appConversationOps', () => ({
  runConversationAction: jest.fn(async () => 'sent'),
  subscribeActionFailures: () => () => {},
  useConversationOverlay: () => new Map(),
}));
jest.mock('@/features/workspaces/WorkspacePill', () => ({ WorkspacePill: () => null }));

beforeEach(() => {
  __resetSessionForTests();
  __resetWorkspaceInfoForTests();
  mockLocalWorkEnabled.mockClear();
  mockSearchKind.mockClear();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
});

const speak = (contract: string) =>
  setWorkspaceInfo({ origin: FAKE_ORIGIN, discovery: DiscoveryDocumentSchema.parse(discoveryDocument13({ client_contract: contract })) });

describe('the Chats list Local work filter', () => {
  it('is not offered by a server older than client contract 1.8, and nothing is fetched for it', async () => {
    speak('1.4');
    await render(<ConversationsScreen />);
    expect(screen.queryByRole('radio', { name: 'Local work' })).toBeNull();
    expect(screen.getByText('Q3 planning')).toBeTruthy();
    expect(mockLocalWorkEnabled).toHaveBeenLastCalledWith(false);
  });

  it('lists the desktop threads with their badges once chosen, and Chats again when switched back', async () => {
    speak('1.8');
    await render(<ConversationsScreen />);
    expect(screen.getByText('Q3 planning')).toBeTruthy();
    expect(screen.queryByTestId('executor-badge')).toBeNull();
    expect(mockLocalWorkEnabled).toHaveBeenLastCalledWith(false);

    await fireEvent.press(screen.getByRole('radio', { name: 'Local work' }));
    expect(mockLocalWorkEnabled).toHaveBeenLastCalledWith(true);
    expect(screen.queryByText('Q3 planning')).toBeNull();
    expect(screen.getByText('Refactor parser')).toBeTruthy();
    expect(screen.getByText('In cloud')).toBeTruthy();
    expect(screen.getByText('On laptop')).toBeTruthy();
    // Pin and rename belong to the Chats list.
    expect(screen.getByRole('button', { name: 'Refactor parser, In cloud, now' }).props.accessibilityActions).toEqual([{ name: 'delete', label: 'Delete' }]);
    // Search stays within this list.
    expect(mockSearchKind).toHaveBeenLastCalledWith('local_work');

    await fireEvent.press(screen.getByRole('radio', { name: 'Chats' }));
    expect(screen.getByText('Q3 planning')).toBeTruthy();
    expect(mockSearchKind).toHaveBeenLastCalledWith('chat');
  });
});
