import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { Conversation } from '@/entities/conversation';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getPermissionListQueryKey } from '@/shared/api/generated/auth/auth';
import { getGetCurrentAuthorQueryKey } from '@/shared/api/generated/social/social';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { createTestQueryClient, renderWithProviders } from '../../__tests__/testUtils';
import type { FolderListItem } from '../../lib/hooks/conversationListState.types';
import type { FolderItemCallbacks, FolderMoveTargetCallbacks } from '../folders/FolderItem';
import { useRenderConversationItem, useRenderFoldersSection } from './Conversations.renderers';
import type { UseRenderConversationItemParams, UseRenderFoldersSectionParams } from './Conversations.renderers';

// `FolderItem` (rendered by `useRenderFoldersSection`) pulls in
// `TypographyWithConditionalTooltip` -> `useTextOverflow`, which needs a
// real `ResizeObserver` — jsdom has none. Same stub-class convention
// `FolderItem.test.tsx`/`Conversations.test.tsx` already established.
class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

function mkConv(overrides: Partial<Conversation> & { readonly id: string }): Conversation {
  return { name: overrides.id, isPrivate: true, ...overrides };
}

// `Record<string, unknown>` (alongside `Partial<FolderListItem>`) matches
// `FolderItem.test.tsx`'s own `mkFolder` signature: `owner_id` is read off
// folders via a permissive cast (`FolderItem.tsx`'s own `readFolderOwnerId`
// doc comment), not a real field on `FolderListItem`/`Folder`.
function mkFolder(overrides: Partial<FolderListItem> & Record<string, unknown> & { readonly id: string }): FolderListItem {
  return { name: overrides.id, conversations: [], ...overrides };
}

function baseConversationItemParams(overrides: Partial<UseRenderConversationItemParams> = {}): UseRenderConversationItemParams {
  return {
    selectedConversationId: undefined,
    onSelectConversation: vi.fn(),
    onEditConversation: vi.fn(),
    onPlaybackConversation: vi.fn(),
    onDeleteConversation: vi.fn(),
    onPinConversation: vi.fn(),
    onCreateConversation: vi.fn().mockResolvedValue(undefined),
    onCancelCreateConversation: vi.fn(),
    onChangeActiveConversationName: vi.fn(),
    getMoveConversationToFoldersMenuItems: () => [],
    isEditingCanvas: false,
    enableDragAndDrop: false,
    projectId: 'p1',
    currentUserId: 'user-1',
    personalProjectId: undefined,
    publicProjectId: undefined,
    basename: '',
    onShareLinkCopied: undefined,
    onExportConversation: vi.fn(),
    ...overrides,
  };
}

function ConversationItemHarness(props: { readonly params: UseRenderConversationItemParams; readonly conversation: Conversation }): ReactElement {
  const renderConversationItem = useRenderConversationItem(props.params);
  return (
    <>
      {renderConversationItem(
        props.conversation,
        () => {},
        false,
      )}
    </>
  );
}

function mkCallbacks(overrides: Partial<FolderItemCallbacks> = {}): FolderItemCallbacks {
  return { onCreateFolder: vi.fn(), onCancelCreateFolder: vi.fn(), onEditFolder: vi.fn(), onPinFolder: vi.fn(), onDeleteFolder: vi.fn(), ...overrides };
}

function mkMoveTarget(overrides: Partial<FolderMoveTargetCallbacks> = {}): FolderMoveTargetCallbacks {
  return { moveTargetConversationToNewFolder: vi.fn(), cancelMovingTargetConversationToNewFolder: vi.fn(), ...overrides };
}

function baseFoldersSectionParams(overrides: Partial<UseRenderFoldersSectionParams> = {}): UseRenderFoldersSectionParams {
  return {
    hoveredFolderId: null,
    selectedConversationId: undefined,
    ungroupedConversationsCount: 0,
    enableDragAndDrop: false,
    isSearchMode: false,
    isFolderOperationInProgress: false,
    getDropAreaState: () => ({ isValidDropTarget: false, isActive: false }),
    onFolderHover: () => {},
    projectId: 'p1',
    renderConversationItem: (conversation) => <div key={conversation.id}>{conversation.name}</div>,
    loadingFolders: new Set<string>(),
    onLoadMoreInFolder: () => {},
    callbacks: mkCallbacks(),
    moveTarget: mkMoveTarget(),
    ...overrides,
  };
}

function FoldersSectionHarness(props: { readonly params: UseRenderFoldersSectionParams; readonly folders: readonly FolderListItem[] }): ReactElement {
  const renderFoldersSection = useRenderFoldersSection(props.params);
  return <>{renderFoldersSection(props.folders, false)}</>;
}

// Same `[aria-label="Folder actions"]` + `fireEvent` (not `userEvent`)
// convention `FolderItem.test.tsx`'s own `openFolderMenu` doc comment
// establishes: the menu trigger is only CSS-`:hover`-visible, which jsdom
// never evaluates from dispatched mouse events.
function openFolderMenu(container: HTMLElement): void {
  const trigger = container.querySelector('[aria-label="Folder actions"]');
  expect(trigger).not.toBeNull();
  fireEvent.click(trigger as Element);
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
  server.use(
    http.get('*/auth/permissions/prompt_lib/:projectId', () =>
      HttpResponse.json([{ name: 'models.chat.folders.create', enabled: true }, { name: 'models.chat.folders.update', enabled: true }], { status: 200 }),
    ),
    http.get('*/social/author', () => HttpResponse.json({ data: { id: 'user-1', name: 'User One' } }, { status: 200 })),
  );
});

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllGlobals();
});

describe('useRenderConversationItem', () => {
  function renderHarness(params: UseRenderConversationItemParams, conversation: Conversation): ReturnType<typeof renderWithTheme> {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    return renderWithTheme(
      (
        <QueryClientProvider client={queryClient}>
          <ConversationItemHarness
            params={params}
            conversation={conversation}
          />
        </QueryClientProvider>
      ) as ReactElement,
    );
  }

  it('renders the conversation via ConversationItem', () => {
    renderHarness(baseConversationItemParams(), mkConv({ id: 'conv-1', name: 'My conversation' }));
    expect(screen.getByText('My conversation')).toBeInTheDocument();
  });

  // The regression this guards: `useRenderConversationItem` bundles its
  // inputs into a `useLatestRef` (see that file's own module doc) purely to
  // stay under the §3.5 `hook-deps` budget — a caller-visible behaviour
  // change would be the returned function's IDENTITY going stable (harmless)
  // while silently keeping the FIRST render's callbacks forever (a real,
  // caller-visible bug this test would catch).
  it('calls the LATEST onSelectConversation after a rerender, not the first render\'s', async () => {
    const user = userEvent.setup();
    const first = vi.fn();
    const second = vi.fn();
    const conversation = mkConv({ id: 'conv-1', name: 'My conversation' });

    const { rerender } = renderHarness(baseConversationItemParams({ onSelectConversation: first }), conversation);
    await user.click(screen.getByText('My conversation'));
    expect(first).toHaveBeenCalledTimes(1);

    rerender(
      (
        <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
          <ConversationItemHarness
            params={baseConversationItemParams({ onSelectConversation: second })}
            conversation={conversation}
          />
        </QueryClientProvider>
      ) as ReactElement,
    );

    await user.click(screen.getByText('My conversation'));
    expect(second).toHaveBeenCalledTimes(1);
    expect(first).toHaveBeenCalledTimes(1);
  });
});

describe('useRenderFoldersSection', () => {
  function renderHarness(params: UseRenderFoldersSectionParams, folders: readonly FolderListItem[]) {
    return renderWithProviders(
      (
        <FoldersSectionHarness
          params={params}
          folders={folders}
        />
      ) as ReactElement,
      createTestQueryClient(),
    );
  }

  it('renders each folder in the section by name', () => {
    renderHarness(baseFoldersSectionParams(), [mkFolder({ id: 'f1', name: 'My Folder' })]);
    expect(screen.getByRole('button', { name: 'My Folder' })).toBeInTheDocument();
  });

  // Same regression class as `useRenderConversationItem` above:
  // `projectId` flows into `FolderItem`'s own `useHasPermission(projectId,
  // ...)` calls, so a stale ref would keep gating the menu on the FIRST
  // render's `projectId` forever, even after a rerender supplies a fresh
  // one.
  it('reflects the LATEST projectId after a rerender (menu re-enables once a real projectId is supplied)', () => {
    const client = createTestQueryClient();
    client.setQueryData(getPermissionListQueryKey('p1'), { data: [{ name: 'models.chat.folders.delete', enabled: true }], status: 200, headers: new Headers() });
    client.setQueryData(getGetCurrentAuthorQueryKey(), {
      data: { id: 'user-1', name: 'User One', email: 'user1@example.com', avatar: '', description: '', personal_project_id: '1' },
      status: 200,
      headers: new Headers(),
    });

    const folder = mkFolder({ id: 'f1', name: 'My Folder', owner_id: 'user-1' });
    const { container, rerender } = renderWithProviders(
      (
        <FoldersSectionHarness
          params={baseFoldersSectionParams({ projectId: undefined })}
          folders={[folder]}
        />
      ) as ReactElement,
      client,
    );

    openFolderMenu(container);
    expect(screen.getByRole('menuitem', { name: 'Delete' })).toHaveAttribute('aria-disabled', 'true');

    rerender(
      (
        <FoldersSectionHarness
          params={baseFoldersSectionParams({ projectId: 'p1' })}
          folders={[folder]}
        />
      ) as ReactElement,
    );

    openFolderMenu(container);
    expect(screen.getByRole('menuitem', { name: 'Delete' })).not.toHaveAttribute('aria-disabled');
  });

  /**
   * Regression (found by adversarial verify): baseline `Folders.jsx:140-152`
   * force-remounts every `FolderItem` when search mode is exited, because
   * `FolderAccordion`'s own `expanded` state only ever syncs FROM
   * `defaultExpanded` going true (see that component's own doc comment) —
   * without the remount, a folder auto-expanded during a search (here, via
   * `computeFolderActivity`'s `ungroupedConversationsCount === 0` branch,
   * the same mechanism that can flip true mid-search as filtered results
   * change) stays visibly expanded forever after the search ends.
   */
  it('collapses a search-auto-expanded folder back down once search mode is exited', () => {
    const folder = mkFolder({ id: 'f1', name: 'My Folder', conversations: [mkConv({ id: 'c1', name: 'Inside folder' })] });

    const { rerender } = renderHarness(baseFoldersSectionParams({ isSearchMode: true, ungroupedConversationsCount: 0 }), [folder]);
    expect(screen.getByText('Inside folder')).toBeVisible();

    // Leaving search mode AND losing the condition that expanded it
    // (ungroupedConversationsCount goes back above 0) in the same rerender —
    // without the force-remount, FolderAccordion's one-way sync would leave
    // it expanded regardless of `defaultExpanded` now being `false`.
    rerender(
      (
        <FoldersSectionHarness
          params={baseFoldersSectionParams({ isSearchMode: false, ungroupedConversationsCount: 5 })}
          folders={[folder]}
        />
      ) as ReactElement,
    );

    expect(screen.getByText('Inside folder')).not.toBeVisible();
  });
});
