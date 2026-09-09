/**
 * ISSUE 852, measured at the COMPOSITION ROOT.
 *
 * The rail's load-more path was complete on the client and reachable by
 * nobody. `LoadMoreSentinel` mounts only while `total` exceeds the rows a
 * bucket already holds, and the grouped listing set `total = offset =
 * len(conversations)` for every bucket that has ever existed — so the sentinel
 * `return null`ed, `onLoadMoreInGroup` never fired, and the follow-up
 * `?date_group=…&offset=…` read (served since the folders handler was fixed)
 * had no caller.
 *
 * Each half passes its own unit test today: `LoadMoreSentinel.test.tsx` proves
 * the sentinel fires when told there is more, `Conversations.helpers.test.ts`
 * proves the merge appends. Neither can see that nothing ever tells the
 * sentinel there is more. So this file mounts the real `Conversations` with a
 * bucket the SERVER says is short, and asserts the follow-up request on the
 * wire and the rows that come back.
 */
import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { Conversation } from '@/entities/conversation';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { Conversations, type ConversationsProps } from './Conversations';
import type { ConversationsDateGroup } from './Conversations.types';

class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

/**
 * Fires the moment the sentinel is observed. The real one fires when the
 * element scrolls into view, which jsdom cannot do — and "the sentinel was
 * observed at all" is the fact under test, because the defect was that it was
 * never mounted.
 */
class ImmediateIntersectionObserver implements IntersectionObserver {
  readonly root = null;
  readonly rootMargin = '';
  readonly scrollMargin = '';
  readonly thresholds: readonly number[] = [];
  static observed = 0;
  private readonly callback: IntersectionObserverCallback;

  constructor(callback: IntersectionObserverCallback) {
    this.callback = callback;
  }

  observe(): void {
    ImmediateIntersectionObserver.observed += 1;
    this.callback([{ isIntersecting: true } as IntersectionObserverEntry], this);
  }

  unobserve(): void {}
  disconnect(): void {}
  takeRecords(): IntersectionObserverEntry[] {
    return [];
  }
}

function mkConv(id: string): Conversation {
  return { id, name: `autotest_page_${id}`, isPrivate: true, authorId: 'user-1' };
}

/** The bucket the SERVER reports: two rows delivered out of five. */
const SHORT_GROUP: ConversationsDateGroup = {
  name: 'Today',
  conversations: [mkConv('c1'), mkConv('c2')],
  total: 5,
  offset: 2,
};

function renderConversations(overrides: Partial<ConversationsProps> = {}): ReturnType<typeof renderWithTheme> {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const props: ConversationsProps = {
    conversations: [...SHORT_GROUP.conversations],
    pinnedConversations: [],
    dateGroups: [SHORT_GROUP],
    setDateGroups: vi.fn(),
    ungroupedConversationsCount: 2,
    totalConversationsAmount: 5,
    onSelectConversation: vi.fn(),
    onCollapsed: vi.fn(),
    onEditConversation: vi.fn(),
    onPlaybackConversation: vi.fn(),
    onDeleteConversation: vi.fn(),
    onPinConversation: vi.fn(),
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
  ImmediateIntersectionObserver.observed = 0;
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
  vi.stubGlobal('IntersectionObserver', ImmediateIntersectionObserver);
  Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: 1440 });
  server.use(
    http.get('*/auth/permissions/prompt_lib/:projectId', () => HttpResponse.json([], { status: 200 })),
    http.get('*/social/author', () => HttpResponse.json({ data: { id: 'user-1', name: 'User One' } }, { status: 200 })),
  );
});

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('Conversations — the rail really asks for a second page (issue 852)', () => {
  it('fetches the bucket’s next page at the offset the server reported, and appends what comes back', async () => {
    const requested: string[] = [];
    server.use(
      http.get('*/elitea_core/folder/prompt_lib/:projectId', ({ request }) => {
        requested.push(request.url);
        // The FLAT body the folders handler writes. `eliteaFetch` returns
        // `{data: <body>, status, headers}`, and `foldersApi`'s own
        // `fetchData` reads `.data` off that envelope — so the body itself is
        // the page, not a `{data: …}` wrapper (issue 132's shape, twice).
        return HttpResponse.json({
          date_group: 'Today',
          total: 5,
          limit: 10,
          offset: 2,
          conversations: [
            { id: 'c3', name: 'autotest_page_c3', author_id: 1, is_private: true },
            { id: 'c4', name: 'autotest_page_c4', author_id: 1, is_private: true },
            { id: 'c5', name: 'autotest_page_c5', author_id: 1, is_private: true },
          ],
        });
      }),
    );
    const setDateGroups = vi.fn();

    renderConversations({ setDateGroups });

    // The sentinel mounted at all — the half that was impossible before.
    await waitFor(() => expect(ImmediateIntersectionObserver.observed).toBeGreaterThan(0));
    await waitFor(() => expect(requested).toHaveLength(1));

    const url = new URL(requested[0] ?? '', window.location.origin);
    expect(url.searchParams.get('date_group'), 'the follow-up must name the bucket it is paging').toBe('Today');
    expect(url.searchParams.get('offset'), 'the offset must be the one the listing handed back').toBe('2');
    expect(url.searchParams.get('grouped')).toBe('true');

    // …and the answer is merged onto the bucket, not dropped.
    // The LAST call, not the first: `Conversations` also re-publishes the
    // groups for reasons unrelated to paging, and picking call 0 would measure
    // whichever of those happened to run first.
    await waitFor(() => expect(setDateGroups).toHaveBeenCalled());
    const calls = setDateGroups.mock.calls;
    const updater = calls[calls.length - 1]?.[0] as (prev: readonly ConversationsDateGroup[]) => readonly ConversationsDateGroup[];
    const next = updater([SHORT_GROUP]);
    expect(next[0]?.conversations.map((conversation) => conversation.id)).toEqual(['c1', 'c2', 'c3', 'c4', 'c5']);
    expect(next[0]?.offset).toBe(5);
    expect(next[0]?.exhausted, 'a bucket that has delivered its whole total must stop asking').toBe(true);
  });

  // The old contract, and the reason nothing ever paged: a bucket whose total
  // equals the rows it delivered has no remainder, so no request is made.
  it('asks for nothing when the bucket already carries its whole total', async () => {
    const requested: string[] = [];
    server.use(
      http.get('*/elitea_core/folder/prompt_lib/:projectId', ({ request }) => {
        requested.push(request.url);
        return HttpResponse.json({ conversations: [], total: 2, offset: 2 });
      }),
    );

    renderConversations({ dateGroups: [{ ...SHORT_GROUP, total: 2, offset: 2 }] });

    // Nothing is observed, because the sentinel renders nothing at all.
    await waitFor(() => expect(ImmediateIntersectionObserver.observed).toBe(0));
    expect(requested).toEqual([]);
  });
});
