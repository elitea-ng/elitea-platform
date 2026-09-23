import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { Conversation } from '@/entities/conversation';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { Conversations, type ConversationsFolder, type ConversationsProps } from './Conversations';

// `FolderItem` (rendered whenever `folders` is non-empty) pulls in
// `TypographyWithConditionalTooltip` -> `useTextOverflow`, which needs a
// real `ResizeObserver` — jsdom has none. Same stub-class convention
// `features/agents/ui/ToolCardBody.test.tsx` (and several sibling test
// files) already established for this identical gap.
class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

function mkConv(overrides: Partial<Conversation> & { readonly id: string }): Conversation {
  return { name: overrides.id, isPrivate: true, ...overrides };
}

function mkFolder(overrides: Partial<ConversationsFolder> & { readonly id: string; readonly conversations: readonly Conversation[] }): ConversationsFolder {
  return { name: overrides.id, ...overrides };
}

function renderConversations(overrides: Partial<ConversationsProps> = {}): ReturnType<typeof renderWithTheme> {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const props: ConversationsProps = {
    conversations: [],
    pinnedConversations: [],
    dateGroups: [],
    setDateGroups: vi.fn(),
    ungroupedConversationsCount: 0,
    totalConversationsAmount: 0,
    onSelectConversation: vi.fn(),
    onCollapsed: vi.fn(),
    onEditConversation: vi.fn(),
    onPlaybackConversation: vi.fn(),
    onDeleteConversation: vi.fn(),
    onPinConversation: vi.fn(),
    onDuplicateConversation: vi.fn(),
    onCreateConversation: vi.fn(),
    onCancelCreateConversation: vi.fn(),
    onChangeActiveConversationName: vi.fn(),
    onCreateFolder: vi.fn(),
    onCancelCreateFolder: vi.fn(),
    folders: [],
    setFolders: vi.fn(),
    onDeleteFolder: vi.fn(),
    onEditFolder: vi.fn(),
    onPinFolder: vi.fn(),
    onMoveToFolderConversation: vi.fn(),
    onMoveToNewFolderConversation: vi.fn(),
    moveTargetConversationToNewFolder: vi.fn(),
    cancelMovingTargetConversationToNewFolder: vi.fn(),
    onClickCreateNewFolder: vi.fn(),
    toastError: vi.fn(),
    projectId: 'p1',
    currentUserId: 'user-1',
    ...overrides,
  };
  return renderWithTheme(
    (
      <QueryClientProvider client={queryClient}>
        <Conversations {...props} />
      </QueryClientProvider>
    ) as ReactElement,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
  // `useIsSmallWindow` (this feature's own duplicate, `lib/hooks/
  // useIsSmallWindow.ts`) reads `window.innerWidth` directly against
  // `MIN_LARGE_WINDOW_WIDTH` (1200) — jsdom's default (1024) reads as
  // "small", hiding the collapse-toggle button these tests need.
  Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: 1440 });
  server.use(
    http.get('*/auth/permissions/prompt_lib/:projectId', () =>
      HttpResponse.json(
        [
          { name: 'models.chat.folders.create', enabled: true },
          { name: 'models.chat.folders.update', enabled: true },
          { name: 'models.chat.folders.delete', enabled: true },
        ],
        { status: 200 },
      ),
    ),
    http.get('*/social/author', () => HttpResponse.json({ data: { id: 'user-1', name: 'User One' } }, { status: 200 })),
  );
});

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllGlobals();
});

describe('Conversations', () => {
  it('renders the "Chats" title when not collapsed', () => {
    renderConversations();
    expect(screen.getByText('Chats')).toBeInTheDocument();
  });

  it('renders each pinned conversation via renderConversationItem, and clicking one calls onSelectConversation', async () => {
    const user = userEvent.setup();
    const onSelectConversation = vi.fn();
    renderConversations({ pinnedConversations: [mkConv({ id: 'p1c' })], onSelectConversation });

    const row = screen.getByText('p1c');
    expect(row).toBeInTheDocument();

    await user.click(row);
    expect(onSelectConversation).toHaveBeenCalledWith(expect.objectContaining({ id: 'p1c' }));
  });

  it('calls onCollapsed when the collapse-toggle button is clicked', async () => {
    const user = userEvent.setup();
    const onCollapsed = vi.fn();
    renderConversations({ onCollapsed });

    await user.click(screen.getByRole('button', { name: /collapse sidebar/i }));

    expect(onCollapsed).toHaveBeenCalledTimes(1);
  });

  it('shows the search bar and calls onSearchQueryChange with the (debounced) query after activating search and typing', async () => {
    const user = userEvent.setup();
    const onSearchQueryChange = vi.fn();
    renderConversations({ onSearchQueryChange });

    await user.click(screen.getByTestId('conversation-search-button'));
    const input = await screen.findByTestId('conversation-search-input');

    await user.type(input, 'hello');

    await waitFor(() => expect(onSearchQueryChange).toHaveBeenCalledWith('hello'), { timeout: 1000 });
  });

  it('renders a skeleton loader (not the list) while isLoadConversations is true', () => {
    renderConversations({ isLoadConversations: true, pinnedConversations: [mkConv({ id: 'hidden-conv' })] });

    expect(screen.getAllByTestId('conversations-loading-skeleton').length).toBeGreaterThan(0);
    expect(screen.queryByText('hidden-conv')).not.toBeInTheDocument();
  });

  it('renders a FolderItem for each entry in folders', async () => {
    renderConversations({ folders: [mkFolder({ id: 'f1', name: 'My Folder', conversations: [] })] });

    expect(await screen.findByText('My Folder')).toBeInTheDocument();
  });

  it('shows the empty-search-results state once search is active, the query is non-empty, and nothing matches', async () => {
    const user = userEvent.setup();
    renderConversations({ conversations: [], folders: [] });

    await user.click(screen.getByTestId('conversation-search-button'));
    const input = await screen.findByTestId('conversation-search-input');
    await user.type(input, 'nomatch');

    expect(await screen.findByText('No conversations found')).toBeInTheDocument();
  });
});
