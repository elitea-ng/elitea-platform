/**
 * foldersApi.test.tsx — contract + hook coverage for `./foldersApi.ts`.
 * Every assertion goes through a real `eliteaFetch` call (MSW-mocked, no
 * `vi.mock()` of application code — R-M1). Wire fixtures below deliberately
 * use snake_case field names (`date_groups`, `total_folders`,
 * `selected_conversation_id`, `updated_at`, `meta.is_pinned`) to prove the
 * wire→domain normalisation the module doc discloses.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import {
  dateGroupConversations,
  deleteFolder,
  folderConversations,
  folderCreate,
  folderPinUpdate,
  foldersList,
  folderUpdate,
  useDeleteFolderMutation,
  useFolderCreateMutation,
  useFolderPinUpdateMutation,
  useFoldersListQuery,
  useFolderUpdateMutation,
} from './foldersApi';

const BASE = '/api/v2';

function createWrapper(): { wrapper: ({ children }: { children: ReactNode }) => ReactNode; client: QueryClient } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  }
  return { wrapper: Wrapper, client };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('folderCreate', () => {
  it('POSTs to elitea_core/folder/prompt_lib/{projectId} with the body sans projectId, and synthesises an empty conversations array', async () => {
    let capturedBody: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/folder/prompt_lib/7`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ id: 'f1', name: 'New folder' });
      }),
    );
    const result = await folderCreate({ projectId: 7, name: 'New folder' });
    expect(capturedBody).toEqual({ name: 'New folder' });
    expect(result).toEqual({ id: 'f1', name: 'New folder', conversations: [] });
  });
});

describe('useFolderCreateMutation', () => {
  it('resolves via the hook', async () => {
    server.use(http.post(`${BASE}/elitea_core/folder/prompt_lib/7`, () => HttpResponse.json({ id: 'f1', name: 'New folder' })));
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useFolderCreateMutation(), { wrapper });
    result.current.mutate({ projectId: 7, name: 'New folder' });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toEqual({ id: 'f1', name: 'New folder', conversations: [] });
  });

  /** Found missing by adversarial verify — every other mutation here already invalidates; this one didn't. */
  it('invalidates every cached foldersList query on success', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () =>
        HttpResponse.json({ pinned: { conversations: [] }, date_groups: [], folders: [], total_folders: 0 }),
      ),
      http.post(`${BASE}/elitea_core/folder/prompt_lib/7`, () => HttpResponse.json({ id: 'f1', name: 'New folder' })),
    );
    const { wrapper, client } = createWrapper();
    const invalidateSpy = vi.spyOn(client, 'invalidateQueries');
    const { result: listResult } = renderHook(() => useFoldersListQuery({ projectId: 7 }), { wrapper });
    await waitFor(() => expect(listResult.current.isSuccess).toBe(true));

    const { result: createResult } = renderHook(() => useFolderCreateMutation(), { wrapper });
    createResult.current.mutate({ projectId: 7, name: 'New folder' });
    await waitFor(() => expect(createResult.current.isSuccess).toBe(true));

    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['folder', 'list'] });
  });
});

describe('foldersList', () => {
  it('GETs with grouped=true forced regardless of params, and normalises the full grouped envelope', async () => {
    let capturedUrl: URL | undefined;
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, ({ request }) => {
        capturedUrl = new URL(request.url);
        return HttpResponse.json({
          pinned: { conversations: [{ id: 'p1', updated_at: '2026-01-01' }] },
          date_groups: [{ name: 'today', conversations: [{ id: 't1', created_at: '2026-01-02' }] }],
          folders: [{ id: 'f1', name: 'Folder', conversations: [], meta: { is_pinned: true } }],
          selected_conversation_id: 'p1',
          total_folders: 1,
        });
      }),
    );
    const result = await foldersList({ projectId: 7, params: { sort_by: 'updated_at', sort_order: 'desc' } });
    expect(capturedUrl?.searchParams.get('grouped')).toBe('true');
    expect(capturedUrl?.searchParams.get('sort_by')).toBe('updated_at');
    expect(result).toEqual({
      pinned: { conversations: [{ id: 'p1', updatedAt: '2026-01-01' }] },
      dateGroups: [{ name: 'today', conversations: [{ id: 't1', createdAt: '2026-01-02' }] }],
      folders: [{ id: 'f1', name: 'Folder', conversations: [], isPinned: true }],
      selectedConversationId: 'p1',
      totalFolders: 1,
    });
  });

  /*
   * DEFECT: the normaliser dropped the wire's `author_id`, so every row in the
   * chat sidebar reached the UI with no owner. The row menu's author-only
   * guard on Delete and Edit then compared `undefined` with `undefined` and
   * denied nothing. The Go folders handler emits `author_id` as an integer on
   * every conversation item (`conversationItem.AuthorID`, no `omitempty`).
   */
  it('carries the wire `author_id` through as a string `authorId`', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () =>
        HttpResponse.json({
          pinned: { conversations: [{ id: 'p1', author_id: 42 }] },
          date_groups: [{ name: 'today', conversations: [{ id: 't1', author_id: 7 }] }],
          folders: [{ id: 'f1', name: 'Folder', conversations: [{ id: 'c1', author_id: 7 }] }],
          total_folders: 1,
        }),
      ),
    );
    const result = await foldersList({ projectId: 7 });
    expect(result.pinned.conversations[0]?.authorId).toBe('42');
    expect(result.dateGroups[0]?.conversations[0]?.authorId).toBe('7');
    expect(result.folders[0]?.conversations[0]?.authorId).toBe('7');
  });

  /*
   * DEFECT: the bucket wire shape declared only `name`/`conversations`, so the
   * normaliser dropped the `total`/`offset` pair the grouped listing sends per
   * date group — the same pair it already kept for a folder. Downstream, a
   * bucket that does not know its own total can never report a remainder, and
   * the rail's load-more sentinel is gated on exactly that.
   */
  it('carries each date group’s `total`/`offset` through, as it already does for a folder', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () =>
        HttpResponse.json({
          pinned: { conversations: [] },
          date_groups: [{ name: 'Today', conversations: [{ id: 't1' }], total: 14, offset: 10 }],
          folders: [{ id: 'f1', name: 'Folder', conversations: [{ id: 'c1' }], total: 9, offset: 1 }],
          total_folders: 1,
        }),
      ),
    );
    const result = await foldersList({ projectId: 7 });
    expect(result.dateGroups[0]).toMatchObject({ name: 'Today', total: 14, offset: 10 });
    expect(result.folders[0]).toMatchObject({ id: 'f1', total: 9, offset: 1 });
  });

  it('forces grouped=true even when a caller-supplied params object tries to override it', async () => {
    let capturedUrl: URL | undefined;
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, ({ request }) => {
        capturedUrl = new URL(request.url);
        return HttpResponse.json({ pinned: { conversations: [] }, date_groups: [], folders: [], total_folders: 0 });
      }),
    );
    await foldersList({ projectId: 7, params: { grouped: false } });
    expect(capturedUrl?.searchParams.get('grouped')).toBe('true');
  });

  it('defaults totalFolders to 0 and omits selectedConversationId when absent from the wire', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () =>
        HttpResponse.json({ pinned: { conversations: [] }, date_groups: [], folders: [] }),
      ),
    );
    const result = await foldersList({ projectId: 7 });
    expect(result.totalFolders).toBe(0);
    expect(result.selectedConversationId).toBeUndefined();
  });
});

describe('useFoldersListQuery', () => {
  it('keys the query as [folder, list, projectId, params]', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () =>
        HttpResponse.json({ pinned: { conversations: [] }, date_groups: [], folders: [], total_folders: 0 }),
      ),
    );
    const { wrapper, client } = createWrapper();
    const { result } = renderHook(() => useFoldersListQuery({ projectId: 7, params: { search: 'x' } }), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(client.getQueryCache().find({ queryKey: ['folder', 'list', 7, { search: 'x' }] })).toBeDefined();
  });
});

describe('folderConversations', () => {
  it('GETs with grouped=true, folder_id, and default limit/offset, exposed as a plain async fetcher (no hook)', async () => {
    let capturedUrl: URL | undefined;
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, ({ request }) => {
        capturedUrl = new URL(request.url);
        return HttpResponse.json({ conversations: [{ id: 'c1', updated_at: '2026-01-01' }], total: 5, offset: 1 });
      }),
    );
    const result = await folderConversations({ projectId: 7, folderId: 'f1' });
    expect(capturedUrl?.searchParams.get('grouped')).toBe('true');
    expect(capturedUrl?.searchParams.get('folder_id')).toBe('f1');
    expect(capturedUrl?.searchParams.get('limit')).toBe('10');
    expect(capturedUrl?.searchParams.get('offset')).toBe('0');
    expect(result).toEqual({ conversations: [{ id: 'c1', updatedAt: '2026-01-01' }], total: 5, offset: 1 });
  });

  it('honours explicit limit/offset/sort_by/sort_order', async () => {
    let capturedUrl: URL | undefined;
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, ({ request }) => {
        capturedUrl = new URL(request.url);
        return HttpResponse.json({ conversations: [] });
      }),
    );
    await folderConversations({ projectId: 7, folderId: 'f1', limit: 20, offset: 40, sort_by: 'name', sort_order: 'asc' });
    expect(capturedUrl?.searchParams.get('limit')).toBe('20');
    expect(capturedUrl?.searchParams.get('offset')).toBe('40');
    expect(capturedUrl?.searchParams.get('sort_by')).toBe('name');
    expect(capturedUrl?.searchParams.get('sort_order')).toBe('asc');
  });
});

describe('dateGroupConversations', () => {
  it('GETs with grouped=true and date_group instead of folder_id', async () => {
    let capturedUrl: URL | undefined;
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, ({ request }) => {
        capturedUrl = new URL(request.url);
        return HttpResponse.json({ conversations: [{ id: 'c2' }], total: 2 });
      }),
    );
    const result = await dateGroupConversations({ projectId: 7, dateGroup: 'today' });
    expect(capturedUrl?.searchParams.get('grouped')).toBe('true');
    expect(capturedUrl?.searchParams.get('date_group')).toBe('today');
    expect(capturedUrl?.searchParams.has('folder_id')).toBe(false);
    expect(result).toEqual({ conversations: [{ id: 'c2' }], total: 2 });
  });
});

describe('folderUpdate / useFolderUpdateMutation', () => {
  it('PUTs to elitea_core/folder/prompt_lib/{projectId}/{id} with the body sans projectId/id', async () => {
    let capturedBody: unknown;
    server.use(
      http.put(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ id: 'f1', name: 'Renamed' });
      }),
    );
    const result = await folderUpdate({ projectId: 7, id: 'f1', name: 'Renamed' });
    expect(capturedBody).toEqual({ name: 'Renamed' });
    expect(result).toEqual({ id: 'f1', name: 'Renamed' });
  });

  it('invalidates every cached foldersList query on success', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () =>
        HttpResponse.json({ pinned: { conversations: [] }, date_groups: [], folders: [], total_folders: 0 }),
      ),
      http.put(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({ id: 'f1' })),
    );
    const { wrapper, client } = createWrapper();
    const invalidateSpy = vi.spyOn(client, 'invalidateQueries');
    const { result: listResult } = renderHook(() => useFoldersListQuery({ projectId: 7 }), { wrapper });
    await waitFor(() => expect(listResult.current.isSuccess).toBe(true));

    const { result: updateResult } = renderHook(() => useFolderUpdateMutation(), { wrapper });
    updateResult.current.mutate({ projectId: 7, id: 'f1', name: 'Renamed' });
    await waitFor(() => expect(updateResult.current.isSuccess).toBe(true));

    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['folder', 'list'] });
  });
});

describe('deleteFolder / useDeleteFolderMutation', () => {
  it('DELETEs elitea_core/folder/prompt_lib/{projectId}/{id} with no body', async () => {
    server.use(http.delete(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({})));
    await expect(deleteFolder({ projectId: 7, id: 'f1' })).resolves.toEqual({});
  });

  it('invalidates every cached foldersList query on success', async () => {
    server.use(http.delete(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({})));
    const { wrapper, client } = createWrapper();
    const invalidateSpy = vi.spyOn(client, 'invalidateQueries');
    const { result } = renderHook(() => useDeleteFolderMutation(), { wrapper });
    result.current.mutate({ projectId: 7, id: 'f1' });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['folder', 'list'] });
  });
});

describe('folderPinUpdate / useFolderPinUpdateMutation', () => {
  it('PATCHes elitea_core/folder/prompt_lib/{projectId}/{id} with only {is_pinned}', async () => {
    let capturedBody: unknown;
    server.use(
      http.patch(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ id: 'f1' });
      }),
    );
    await folderPinUpdate({ projectId: 7, id: 'f1', is_pinned: true });
    expect(capturedBody).toEqual({ is_pinned: true });
  });

  it('does NOT invalidate the foldersList query on success (matches the old app empty invalidatesTags)', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/folder/prompt_lib/7`, () =>
        HttpResponse.json({ pinned: { conversations: [] }, date_groups: [], folders: [], total_folders: 0 }),
      ),
      http.patch(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({ id: 'f1' })),
    );
    const { wrapper, client } = createWrapper();
    const invalidateSpy = vi.spyOn(client, 'invalidateQueries');
    const { result: listResult } = renderHook(() => useFoldersListQuery({ projectId: 7 }), { wrapper });
    await waitFor(() => expect(listResult.current.isSuccess).toBe(true));

    const { result: pinResult } = renderHook(() => useFolderPinUpdateMutation(), { wrapper });
    pinResult.current.mutate({ projectId: 7, id: 'f1', is_pinned: true });
    await waitFor(() => expect(pinResult.current.isSuccess).toBe(true));

    expect(invalidateSpy).not.toHaveBeenCalled();
  });
});
