import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useQueryFoldersList } from './useQueryFoldersList.hooks';
import type { FolderListItem } from './conversationListState.types';

const BASE = '/api/v2';

function grantPermission(): void {
  server.use(
    http.get('*/auth/permissions/prompt_lib/:projectId', () =>
      HttpResponse.json([{ name: 'models.chat.folders.get', enabled: true }], { status: 200 }),
    ),
  );
}

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useQueryFoldersList', () => {
  it('happy path: loads the grouped envelope and fans it out to the three setters', async () => {
    grantPermission();
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () =>
        HttpResponse.json({
          pinned: { conversations: [{ id: 'p1', updated_at: '2026-01-05' }] },
          date_groups: [{ name: 'today', conversations: [{ id: 't1', updated_at: '2026-01-04' }] }],
          folders: [{ id: 'f1', name: 'Folder 1', conversations: [{ id: 'c1', updated_at: '2026-01-03' }] }],
          total_folders: 1,
        }),
      ),
    );

    const setFolders = vi.fn();
    const setDateGroups = vi.fn();
    const setPinnedConversations = vi.fn();

    const { result } = renderHook(
      () => useQueryFoldersList({ projectId: '7', toastError: vi.fn(), setFolders, setDateGroups, setPinnedConversations }),
      { wrapper },
    );

    await waitFor(() => expect(result.current.isConversationsLoaded).toBe(true));

    expect(result.current.totalFolderCount).toBe(1);
    expect(setPinnedConversations).toHaveBeenCalledWith([expect.objectContaining({ id: 'p1', isPinned: true })]);
    expect(setDateGroups).toHaveBeenCalledWith([expect.objectContaining({ name: 'today', offset: 1 })]);

    const foldersUpdater = setFolders.mock.calls[0]?.[0] as (prev: readonly FolderListItem[]) => readonly FolderListItem[];
    const applied = foldersUpdater([]);
    expect(applied).toHaveLength(1);
    expect(applied[0]).toMatchObject({ id: 'f1', name: 'Folder 1', offset: 1 });
  });

  /*
   * The gap the rail's load-more fell through.
   *
   * `Conversations.pagination.test.tsx` proves the follow-up read fires once a
   * bucket knows it has a remainder, but it hands `dateGroups` to the
   * component ALREADY carrying `total`. Nothing asserted that the value ever
   * survived the trip from the wire, and it did not: this hook rebuilt each
   * bucket from `name`/`conversations` alone. Every bucket reached
   * `LoadMoreSentinel` with `totalAvailableCount` 0, so the sentinel never
   * mounted and the second page was unreachable in the real app while both
   * unit suites stayed green.
   */
  it('carries each bucket’s total and offset through from the answer, so a remainder is expressible', async () => {
    grantPermission();
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () =>
        HttpResponse.json({
          pinned: { conversations: [] },
          date_groups: [{ name: 'Today', conversations: [{ id: 't1' }, { id: 't2' }], total: 14, offset: 2 }],
          folders: [{ id: 'f1', name: 'Folder 1', conversations: [{ id: 'c1' }], total: 9, offset: 1 }],
          total_folders: 1,
        }),
      ),
    );

    const setFolders = vi.fn();
    const setDateGroups = vi.fn();
    const { result } = renderHook(
      () => useQueryFoldersList({ projectId: '7', toastError: vi.fn(), setFolders, setDateGroups, setPinnedConversations: vi.fn() }),
      { wrapper },
    );
    await waitFor(() => expect(result.current.isConversationsLoaded).toBe(true));

    expect(setDateGroups, 'a bucket must reach the rail knowing how big it really is').toHaveBeenCalledWith([
      expect.objectContaining({ name: 'Today', total: 14, offset: 2 }),
    ]);

    const foldersUpdater = setFolders.mock.calls[0]?.[0] as (prev: readonly FolderListItem[]) => readonly FolderListItem[];
    expect(foldersUpdater([])[0]).toMatchObject({ id: 'f1', total: 9, offset: 1 });
  });

  it('excludes any pinned conversation id from its date group / folder bucket', async () => {
    grantPermission();
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () =>
        HttpResponse.json({
          pinned: { conversations: [{ id: 'shared-id' }] },
          date_groups: [{ name: 'today', conversations: [{ id: 'shared-id' }, { id: 'other' }] }],
          folders: [],
          total_folders: 0,
        }),
      ),
    );

    const setDateGroups = vi.fn();
    const { result } = renderHook(
      () => useQueryFoldersList({ projectId: '7', toastError: vi.fn(), setFolders: vi.fn(), setDateGroups, setPinnedConversations: vi.fn() }),
      { wrapper },
    );

    await waitFor(() => expect(result.current.isConversationsLoaded).toBe(true));

    const groups = setDateGroups.mock.calls[0]?.[0] as readonly { readonly conversations: readonly { readonly id: string }[] }[] | undefined;
    expect(groups?.[0]?.conversations.map((c) => c.id)).toEqual(['other']);
  });

  it('error path: toasts the built error message and clears all three state trees', async () => {
    grantPermission();
    server.use(http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () => HttpResponse.json({ message: 'boom' }, { status: 500 })));

    const setFolders = vi.fn();
    const setDateGroups = vi.fn();
    const setPinnedConversations = vi.fn();
    const toastError = vi.fn();

    renderHook(() => useQueryFoldersList({ projectId: '7', toastError, setFolders, setDateGroups, setPinnedConversations }), { wrapper });

    await waitFor(() => expect(toastError).toHaveBeenCalledWith('boom'));
    expect(setFolders).toHaveBeenCalledWith([]);
    expect(setDateGroups).toHaveBeenCalledWith([]);
    expect(setPinnedConversations).toHaveBeenCalledWith([]);
  });

  it('distinctive rule: preserves isNew local-only folders, and merges local-only conversations the server response omits', async () => {
    grantPermission();
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () =>
        HttpResponse.json({
          pinned: { conversations: [] },
          date_groups: [],
          folders: [{ id: 'f1', name: 'Folder 1', conversations: [{ id: 'server-conv' }] }],
          total_folders: 1,
        }),
      ),
    );

    const setFolders = vi.fn();
    const { result } = renderHook(
      () => useQueryFoldersList({ projectId: '7', toastError: vi.fn(), setFolders, setDateGroups: vi.fn(), setPinnedConversations: vi.fn() }),
      { wrapper },
    );
    await waitFor(() => expect(result.current.isConversationsLoaded).toBe(true));

    const updater = setFolders.mock.calls[0]?.[0] as (prev: readonly FolderListItem[]) => readonly FolderListItem[];
    const draftFolder: FolderListItem = { id: 'draft', name: 'New folder', conversations: [], isNew: true };
    const previousFolderWithLocalOnly: FolderListItem = {
      id: 'f1',
      name: 'Folder 1 (stale name)',
      conversations: [{ id: 'server-conv', name: 'server-conv', isPrivate: true }, { id: 'local-only', name: 'local-only', isPrivate: true }],
    };

    const applied = updater([draftFolder, previousFolderWithLocalOnly]);

    // The isNew draft is preserved verbatim and kept first.
    expect(applied[0]).toEqual(draftFolder);

    // f1 comes from the server (fresh name), but keeps the local-only conversation the server response didn't include.
    const merged = applied.find((f) => f.id === 'f1');
    expect(merged?.name).toBe('Folder 1');
    expect(merged?.conversations.map((c) => c.id).sort()).toEqual(['local-only', 'server-conv']);
  });
});
