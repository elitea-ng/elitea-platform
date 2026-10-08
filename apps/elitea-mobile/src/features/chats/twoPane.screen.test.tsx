import { act, fireEvent, render, screen } from '@testing-library/react-native';
import { Alert } from 'react-native';

import ConversationsScreen from '@/app/(app)/(tabs)/chats/index';
import ThreadScreen from '@/app/(app)/c/[id]';
import { clearSelectedChat, getSelectedChat, selectChat } from '@/features/chats/selectedChat';
import { isTwoPane, listPaneWidth, TWO_PANE_MIN_WIDTH } from '@/features/chats/twoPane';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN } from '@/test/fakeServer';

// Issue #15: at regular width (iPad) the Chats tab shows the list and the open chat side by
// side; at compact width (a phone, iPad Split View 1/3) the phone stack is unchanged.

const P = 90501;
const WS = 'ws-1';

const mockPush = jest.fn();
const mockOptions: Record<string, unknown>[] = [];
let mockFocused = true;
let mockParams: Record<string, string> = {};
jest.mock('expo-router', () => ({
  router: { push: (...args: unknown[]) => mockPush(...args), back: jest.fn() },
  useIsFocused: () => mockFocused,
  useLocalSearchParams: () => mockParams,
  Stack: {
    Screen: ({ options }: { options?: Record<string, unknown> }) => {
      if (options) mockOptions.push(options);
      return null;
    },
  },
}));
jest.mock('react-native-safe-area-context', () => require('react-native-safe-area-context/jest/mock').default);
jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('@/theme/brandStore', () => ({ useBrandTheme: () => null }));
jest.mock('@/theme/brandFonts', () => ({ useBrandFontFamily: () => undefined }));

// The thread itself has its own screen tests; here only where it is drawn, and how.
jest.mock('@/features/chat/ThreadView', () => {
  const { Text: RNText } = require('react-native');
  return {
    ThreadView: ({ id, chrome = 'stack' }: { id: string; chrome?: string }) => <RNText testID={`thread-${chrome}`}>{`thread ${id}`}</RNText>,
  };
});
const mockRows = [
  { id: 11, name: 'Q3 planning', updated_at: new Date().toISOString(), created_at: new Date().toISOString(), duration: -1, message_groups_count: 2 },
  { id: 12, name: 'Film club', updated_at: new Date().toISOString(), created_at: new Date().toISOString(), duration: -1, message_groups_count: 1 },
];
jest.mock('@/api/queries/conversations', () => ({
  useConversations: () => ({
    data: { pages: [{ rows: mockRows, total: mockRows.length }] },
    isLoading: false,
    error: null,
    isRefetching: false,
    isFetchingNextPage: false,
    hasNextPage: false,
    refetch: jest.fn(),
    fetchNextPage: jest.fn(),
  }),
  flattenConversationPages: (pages: { rows: unknown[] }[] | undefined) => (pages ?? []).flatMap((page) => page.rows),
}));
jest.mock('@/features/projects/useProjectSelection', () => ({ useProjectSelection: () => ({ projectId: 90501, noProject: false }) }));
jest.mock('@/features/chats/useChatListData', () => ({
  useListActionsAvailable: () => true,
  useSavedPinnedRows: () => [],
  useConversationSearch: () => null,
}));
jest.mock('@/sync/appConversationOps', () => ({
  runConversationAction: jest.fn(async () => 'sent'),
  subscribeActionFailures: () => () => {},
  useConversationOverlay: () => new Map(),
}));
jest.mock('@/features/workspaces/WorkspacePill', () => ({ WorkspacePill: () => null }));
// The hooks `endSession` runs (its own tests run them); here, that the selection has one.
jest.mock('@/security/endSession', () => {
  const wipe = new Map();
  const stored = new Map();
  return {
    mockHooks: { wipe, stored },
    registerWipeHook: (name: string, hook: unknown) => {
      wipe.set(name, hook);
      return () => {};
    },
    registerStoredWipeHook: (name: string, hook: unknown) => {
      stored.set(name, hook);
      return () => {};
    },
  };
});
const wipeHooks = () =>
  (require('@/security/endSession') as { mockHooks: { wipe: Map<string, () => unknown>; stored: Map<string, (workspaceId: string) => unknown> } }).mockHooks;

/** Lays the screen out at `width` (its own measured width, as on a device). */
async function layOut(width: number) {
  const target = screen.queryByTestId('chats-two-pane') ?? screen.getByTestId('chats-list');
  await act(async () => {
    fireEvent(target, 'layout', { nativeEvent: { layout: { x: 0, y: 0, width, height: 800 } } });
  });
}

const lastOptions = () => mockOptions[mockOptions.length - 1] ?? {};

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P, workspaceId: WS });
  setCredential({ kind: 'pat', token: 't' });
  clearSelectedChat();
  mockPush.mockReset();
  mockOptions.length = 0;
  mockFocused = true;
  mockParams = {};
});

describe('width classes', () => {
  it('two panes from 700 pt (every iPad full screen and 2/3 split), the phone stack below', () => {
    expect(isTwoPane(390)).toBe(false); // a phone
    expect(isTwoPane(507)).toBe(false); // iPad Split View 1/3 (and most 1/2)
    expect(isTwoPane(TWO_PANE_MIN_WIDTH - 1)).toBe(false);
    expect(isTwoPane(744)).toBe(true); // iPad mini portrait
    expect(isTwoPane(1376)).toBe(true); // iPad Pro 13" landscape
    expect(listPaneWidth(744)).toBe(320);
    expect(listPaneWidth(1024)).toBe(369);
    expect(listPaneWidth(1376)).toBe(400);
  });
});

describe('Chats at compact width (phone)', () => {
  it('is the phone stack: a large title, no detail column, and a tap pushes the chat', async () => {
    await render(<ConversationsScreen />);
    await layOut(390);
    expect(screen.queryByTestId('chats-two-pane')).toBeNull();
    expect(screen.queryByTestId('thread-pane')).toBeNull();
    expect(lastOptions().headerLargeTitle).toBe(true);
    const row = screen.getByRole('button', { name: /^Film club/ });
    // A row that navigates reads as a button, not as a selection.
    expect(row.props.accessibilityState?.selected).toBeUndefined();
    await fireEvent.press(row);
    expect(mockPush).toHaveBeenCalledWith({ pathname: '/c/[id]', params: { id: '12' } });
    // The first layout is not a collapse: nothing else was pushed.
    expect(mockPush).toHaveBeenCalledTimes(1);
  });
});

describe('Chats at regular width (iPad)', () => {
  it('shows the list and a detail column; a tap opens the chat beside the list, selected, without navigating', async () => {
    await render(<ConversationsScreen />);
    await layOut(1024);
    expect(screen.getByTestId('chats-two-pane')).toBeTruthy();
    expect(screen.getByTestId('chats-list-pane')).toHaveStyle({ width: 369 });
    // Nothing chosen yet: an empty detail column with a way to start a chat.
    expect(screen.getByRole('header', { name: 'No chat selected' })).toBeTruthy();
    expect(lastOptions().headerLargeTitle).toBe(false);

    await fireEvent.press(screen.getByRole('button', { name: /^Q3 planning/ }));
    expect(mockPush).not.toHaveBeenCalled();
    expect(screen.getByTestId('thread-pane')).toHaveTextContent('thread 11');
    expect(screen.getByRole('button', { name: /^Q3 planning/ }).props.accessibilityState?.selected).toBe(true);
    expect(screen.getByRole('button', { name: /^Film club/ }).props.accessibilityState?.selected).toBeFalsy();

    await fireEvent.press(screen.getByRole('button', { name: /^Film club/ }));
    expect(screen.getByTestId('thread-pane')).toHaveTextContent('thread 12');
  });

  it('keeps the selection across widths: collapsing pushes the open chat, and expanding shows it again', async () => {
    await render(<ConversationsScreen />);
    await layOut(1024);
    await fireEvent.press(screen.getByRole('button', { name: /^Film club/ }));

    // Split View 1/3 (or a rotation into a narrow window): the phone stack, the chat on top.
    await layOut(507);
    expect(screen.queryByTestId('chats-two-pane')).toBeNull();
    expect(mockPush).toHaveBeenCalledWith({ pathname: '/c/[id]', params: { id: '12' } });

    // Back to full screen: the same chat beside the list.
    await layOut(1024);
    expect(screen.getByTestId('thread-pane')).toHaveTextContent('thread 12');
    expect(mockPush).toHaveBeenCalledTimes(1);
  });

  it('does not push a chat when the width changes while another tab or screen is in front', async () => {
    await render(<ConversationsScreen />);
    await layOut(1024);
    await fireEvent.press(screen.getByRole('button', { name: /^Film club/ }));
    mockFocused = false;
    await layOut(507);
    expect(mockPush).not.toHaveBeenCalled();
  });

  it('a chat opened on its own screen is the one selected when the two panes come back', async () => {
    mockParams = { id: '11' };
    await render(<ThreadScreen />);
    expect(screen.getByTestId('thread-stack')).toHaveTextContent('thread 11');
    expect(getSelectedChat({ workspaceId: WS, projectId: P })).toBe(11);
    await screen.unmount();

    await render(<ConversationsScreen />);
    await layOut(1024);
    expect(screen.getByTestId('thread-pane')).toHaveTextContent('thread 11');
  });

  it("closes the detail column when the open chat is deleted", async () => {
    jest.spyOn(Alert, 'alert').mockImplementation((_title, _body, buttons) => {
      buttons?.find((b) => b.text === 'Delete')?.onPress?.();
    });
    await render(<ConversationsScreen />);
    await layOut(1024);
    await fireEvent.press(screen.getByRole('button', { name: /^Film club/ }));
    expect(screen.getByTestId('thread-pane')).toBeTruthy();
    await act(async () => {
      await fireEvent(screen.getByRole('button', { name: /^Film club/ }), 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
    });
    expect(screen.queryByTestId('thread-pane')).toBeNull();
    expect(screen.getByRole('header', { name: 'No chat selected' })).toBeTruthy();
  });
});

describe('the selection never crosses a workspace, a project or a session', () => {
  it('reads as nothing in another workspace or project', () => {
    selectChat({ workspaceId: WS, projectId: P }, 12);
    expect(getSelectedChat({ workspaceId: WS, projectId: P })).toBe(12);
    expect(getSelectedChat({ workspaceId: 'ws-2', projectId: P })).toBeNull();
    expect(getSelectedChat({ workspaceId: WS, projectId: P + 1 })).toBeNull();
  });

  it('a project switch shows an empty detail column, not the other project’s chat', async () => {
    selectChat({ workspaceId: WS, projectId: P + 1 }, 12);
    await render(<ConversationsScreen />);
    await layOut(1024);
    expect(screen.queryByTestId('thread-pane')).toBeNull();
    expect(screen.getByRole('header', { name: 'No chat selected' })).toBeTruthy();
  });

  it('is wiped with the session (sign-out, wipe), so the next account never opens it', () => {
    selectChat({ workspaceId: WS, projectId: P }, 12);
    wipeHooks().wipe.get('chat_selection')?.();
    expect(getSelectedChat({ workspaceId: WS, projectId: P })).toBeNull();
  });

  it('is wiped with its workspace when that workspace is removed while another is active', () => {
    selectChat({ workspaceId: WS, projectId: P }, 12);
    wipeHooks().stored.get('chat_selection')?.('ws-2');
    expect(getSelectedChat({ workspaceId: WS, projectId: P })).toBe(12);
    wipeHooks().stored.get('chat_selection')?.(WS);
    expect(getSelectedChat({ workspaceId: WS, projectId: P })).toBeNull();
  });
});
