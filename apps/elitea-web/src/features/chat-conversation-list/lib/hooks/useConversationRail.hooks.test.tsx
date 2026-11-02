/**
 * The rail model's guards and follow-up reads:
 *
 * - a folder's Rename, Pin and Delete are its owner's: the listing's
 *   `owner_id` against the caller's id (it was dropped by the normaliser, so
 *   every folder was locked, the caller's own too);
 * - a follow-up page of a bucket is read under the listing's own filter
 *   (search and the Local work switch), not the unfiltered rail;
 * - `enabled: false` reads nothing.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useConversationRail } from './useConversationRail.hooks';

const BASE = '/api/v2';
const at = '2026-11-01T10:00:00Z';
const row = (id: number) => ({ id, name: `chat ${String(id)}`, author_id: 3, updated_at: at, created_at: at });

let listingReads: URL[];
let pageReads: URL[];

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  listingReads = [];
  pageReads = [];
  server.use(
    http.get('*/auth/permissions/prompt_lib/:projectId', () => HttpResponse.json([{ name: 'models.chat.folders.get', enabled: true }])),
    http.get('*/social/author', () => HttpResponse.json({ id: 3, name: 'Me' })),
    http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, ({ request }) => {
      const url = new URL(request.url);
      if (url.searchParams.has('date_group') || url.searchParams.has('folder_id')) {
        pageReads.push(url);
        return HttpResponse.json({ conversations: [row(99)], total: 3 });
      }
      listingReads.push(url);
      return HttpResponse.json({
        pinned: { conversations: [] },
        date_groups: [{ name: 'Today', total: 3, offset: 2, conversations: [row(1), row(2)] }],
        folders: [
          { id: '11', name: 'Mine', owner_id: 3, total: 3, offset: 2, conversations: [row(4), row(5)] },
          { id: '12', name: 'Theirs', owner_id: 8, conversations: [] },
          { id: '13', name: 'Nobody', conversations: [] },
        ],
        total_folders: 3,
      });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useConversationRail', () => {
  it('lets the folder’s owner, and only the owner, modify it', async () => {
    const { result } = renderHook(() => useConversationRail({ projectId: '7' }), { wrapper });
    await waitFor(() => expect(result.current.folders).toHaveLength(3));
    // The caller's identity arrives on its own read; wait for the guard to see it.
    await waitFor(() => expect(result.current.canModifyFolder(result.current.folders[0]!)).toBe(true));
    expect(result.current.canModifyFolder(result.current.folders[1]!)).toBe(false);
    expect(result.current.canModifyFolder(result.current.folders[2]!)).toBe(false);
  });

  it('reads a bucket’s next page under the listing’s search and Local work filter', async () => {
    const { result } = renderHook(() => useConversationRail({ projectId: '7', searchQuery: 'release', localWorkOnly: true }), { wrapper });
    await waitFor(() => expect(result.current.dateGroups).toHaveLength(1));
    expect(listingReads.at(-1)?.searchParams.get('query')).toBe('release');

    act(() => result.current.loadMoreInGroup('Today'));
    await waitFor(() => expect(pageReads).toHaveLength(1));
    act(() => result.current.loadMoreInFolder('11'));
    await waitFor(() => expect(pageReads).toHaveLength(2));

    for (const page of pageReads) {
      expect(page.searchParams.get('query')).toBe('release');
      expect(page.searchParams.get('source')).toBe('local_work');
    }
    expect(pageReads[0]?.searchParams.get('date_group')).toBe('Today');
    expect(pageReads[1]?.searchParams.get('folder_id')).toBe('11');
  });

  it('sends no filter on the pages of the ordinary rail', async () => {
    const { result } = renderHook(() => useConversationRail({ projectId: '7' }), { wrapper });
    await waitFor(() => expect(result.current.dateGroups).toHaveLength(1));
    act(() => result.current.loadMoreInGroup('Today'));
    await waitFor(() => expect(pageReads).toHaveLength(1));
    expect(pageReads[0]?.searchParams.has('query')).toBe(false);
    expect(pageReads[0]?.searchParams.has('source')).toBe(false);
  });

  it('reads nothing while it is not enabled, and reads once it is', async () => {
    const { result, rerender } = renderHook(({ enabled }) => useConversationRail({ projectId: '7', enabled }), { wrapper, initialProps: { enabled: false } });
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(listingReads).toHaveLength(0);
    expect(result.current.folders).toHaveLength(0);

    rerender({ enabled: true });
    await waitFor(() => expect(result.current.folders).toHaveLength(3));
    expect(listingReads).toHaveLength(1);
  });
});
