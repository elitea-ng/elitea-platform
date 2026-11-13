import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getPermissionListQueryKey } from '@/shared/api/generated/auth/auth';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useEditFolder } from './useEditFolder';
import type { FolderListItem } from './conversationListState.types';

const BASE = '/api/v2';
const PROJECT_ID = '7';

function mkFolder(overrides: Partial<FolderListItem> & { readonly id: string }): FolderListItem {
  return { name: overrides.id, conversations: [], ...overrides };
}

/**
 * Primes the permission-list query's cache synchronously before the first
 * render, so `useHasPermission` sees the resolved value on its VERY first
 * render — see `useCreateFolder.hooks.test.tsx`'s identical helper for why
 * this is needed instead of `waitFor(() => expect(result.current).
 * toBeDefined())` (which never actually waits for the permission fetch).
 */
function createWrapper(permissionNames: readonly string[]): { readonly wrapper: ({ children }: { children: ReactNode }) => ReactNode; readonly client: QueryClient } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  client.setQueryData(getPermissionListQueryKey(PROJECT_ID), {
    data: permissionNames.map((name) => ({ name, enabled: true })),
    status: 200,
    headers: new Headers(),
  });
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

describe('useEditFolder', () => {
  it('onEditFolder happy path: PUTs the rename and updates the folder (and activeFolder) locally', async () => {
    server.use(http.put(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({ id: 'f1', name: 'Renamed' })));

    const setActiveFolder = vi.fn();
    const setFolders = vi.fn();
    const active = mkFolder({ id: 'f1' });
    const renamed = mkFolder({ id: 'f1', name: 'Renamed' });

    const { wrapper } = createWrapper(['models.chat.folders.update']);
    const { result } = renderHook(
      () => useEditFolder({ projectId: '7', activeFolder: active, setActiveFolder, setFolders, toastError: vi.fn() }),
      { wrapper },
    );

    await result.current.onEditFolder(renamed);

    expect(setActiveFolder).toHaveBeenCalledWith(renamed);
    const updater = setFolders.mock.calls[0]?.[0] as (prev: readonly FolderListItem[]) => readonly FolderListItem[];
    expect(updater([active]).map((f) => f.name)).toEqual(['Renamed']);
  });

  it('onEditFolder error path: toasts and does not touch local state', async () => {
    server.use(http.put(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({ message: 'boom' }, { status: 500 })));

    const setActiveFolder = vi.fn();
    const setFolders = vi.fn();
    const toastError = vi.fn();

    const { wrapper } = createWrapper(['models.chat.folders.update']);
    const { result } = renderHook(
      () => useEditFolder({ projectId: '7', activeFolder: undefined, setActiveFolder, setFolders, toastError }),
      { wrapper },
    );

    await result.current.onEditFolder(mkFolder({ id: 'f1', name: 'Renamed' }));

    expect(setFolders).not.toHaveBeenCalled();
    await waitFor(() => expect(toastError).toHaveBeenCalledWith('boom'));
  });

  it('distinctive rule: without folders.update permission, onEditFolder and onPinFolder both skip the network call and local update', async () => {
    let putHit = false;
    let patchHit = false;
    server.use(
      http.put(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => {
        putHit = true;
        return HttpResponse.json({});
      }),
      http.patch(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => {
        patchHit = true;
        return HttpResponse.json({});
      }),
    );

    const setActiveFolder = vi.fn();
    const setFolders = vi.fn();
    const toastError = vi.fn();

    const { wrapper } = createWrapper([]);
    const { result } = renderHook(
      () => useEditFolder({ projectId: '7', activeFolder: undefined, setActiveFolder, setFolders, toastError }),
      { wrapper },
    );

    await result.current.onEditFolder(mkFolder({ id: 'f1' }));
    await result.current.onPinFolder(mkFolder({ id: 'f1' }), true);

    expect(putHit).toBe(false);
    expect(patchHit).toBe(false);
    expect(setActiveFolder).not.toHaveBeenCalled();
    expect(setFolders).not.toHaveBeenCalled();
  });

  /**
   * Regression (found by adversarial verify): a permission-denied
   * `onEditFolder` used to return with zero user feedback. The baseline's
   * own (real network round-trip) behavior for this scenario surfaced a
   * toast — see this hook's own doc comment for the full citation.
   */
  it('onEditFolder without folders.update permission surfaces a toast (unlike onPinFolder, which stays a true silent no-op)', async () => {
    const setActiveFolder = vi.fn();
    const setFolders = vi.fn();
    const toastError = vi.fn();

    const { wrapper } = createWrapper([]);
    const { result } = renderHook(
      () => useEditFolder({ projectId: '7', activeFolder: undefined, setActiveFolder, setFolders, toastError }),
      { wrapper },
    );

    await result.current.onEditFolder(mkFolder({ id: 'f1' }));
    expect(toastError).toHaveBeenCalledWith('You do not have permission to edit folders');

    toastError.mockClear();
    await result.current.onPinFolder(mkFolder({ id: 'f1' }), true);
    expect(toastError).not.toHaveBeenCalled();
  });

  it('onEditFolder with no projectId stays a silent no-op (nothing to act on yet)', async () => {
    const setActiveFolder = vi.fn();
    const setFolders = vi.fn();
    const toastError = vi.fn();

    const { wrapper } = createWrapper([]);
    const { result } = renderHook(
      () => useEditFolder({ projectId: undefined, activeFolder: undefined, setActiveFolder, setFolders, toastError }),
      { wrapper },
    );

    await result.current.onEditFolder(mkFolder({ id: 'f1' }));

    expect(toastError).not.toHaveBeenCalled();
    expect(setFolders).not.toHaveBeenCalled();
  });

  it('onPinFolder happy path: PATCHes is_pinned and updates isPinned locally', async () => {
    server.use(http.patch(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({ id: 'f1' })));
    const setFolders = vi.fn();

    const { wrapper } = createWrapper(['models.chat.folders.update']);
    const { result } = renderHook(
      () => useEditFolder({ projectId: '7', activeFolder: undefined, setActiveFolder: vi.fn(), setFolders, toastError: vi.fn() }),
      { wrapper },
    );

    await result.current.onPinFolder(mkFolder({ id: 'f1' }), true);

    const updater = setFolders.mock.calls[0]?.[0] as (prev: readonly FolderListItem[]) => readonly FolderListItem[];
    expect(updater([mkFolder({ id: 'f1' })])[0]?.isPinned).toBe(true);
  });

  it('onPinFolder moves the pinned folder ahead of the unpinned ones (issue 2031)', async () => {
    server.use(http.patch(`${BASE}/elitea_core/folder/prompt_lib/7/f2`, () => HttpResponse.json({ id: 'f2' })));
    const setFolders = vi.fn();

    const { wrapper } = createWrapper(['models.chat.folders.update']);
    const { result } = renderHook(
      () => useEditFolder({ projectId: '7', activeFolder: undefined, setActiveFolder: vi.fn(), setFolders, toastError: vi.fn() }),
      { wrapper },
    );

    await result.current.onPinFolder(mkFolder({ id: 'f2' }), true);

    const updater = setFolders.mock.calls[0]?.[0] as (prev: readonly FolderListItem[]) => readonly FolderListItem[];
    expect(updater([mkFolder({ id: 'f1' }), mkFolder({ id: 'f2' })]).map((folder) => folder.id)).toEqual(['f2', 'f1']);
  });

  it('onPinFolder error path: toasts and does not touch local state', async () => {
    server.use(http.patch(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({ message: 'boom' }, { status: 500 })));
    const setFolders = vi.fn();
    const toastError = vi.fn();

    const { wrapper } = createWrapper(['models.chat.folders.update']);
    const { result } = renderHook(
      () => useEditFolder({ projectId: '7', activeFolder: undefined, setActiveFolder: vi.fn(), setFolders, toastError }),
      { wrapper },
    );

    await result.current.onPinFolder(mkFolder({ id: 'f1' }), true);

    expect(setFolders).not.toHaveBeenCalled();
    await waitFor(() => expect(toastError).toHaveBeenCalledWith('boom'));
  });

  /**
   * Regression (found by adversarial verify): `folderApi.update` is the
   * plain fetcher — nothing invalidated the cached `foldersList` query on a
   * successful rename.
   */
  it('onEditFolder invalidates the cached folder list on success', async () => {
    server.use(http.put(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({ id: 'f1', name: 'Renamed' })));
    const { wrapper, client } = createWrapper(['models.chat.folders.update']);
    const invalidateSpy = vi.spyOn(client, 'invalidateQueries');
    const { result } = renderHook(
      () => useEditFolder({ projectId: '7', activeFolder: undefined, setActiveFolder: vi.fn(), setFolders: vi.fn(), toastError: vi.fn() }),
      { wrapper },
    );

    await result.current.onEditFolder(mkFolder({ id: 'f1', name: 'Renamed' }));

    expect(invalidateSpy).toHaveBeenCalledWith({ queryKey: ['folder', 'list'] });
  });

  /**
   * DEFECT (stale row-handler): `onEditFolder` reaches a `FolderItem` built by
   * `Conversations.renderers.tsx`'s render prop, so the closure a folder row
   * carries can predate the click that selected that folder — and the line it
   * feeds runs after `await folderApi.update` on top of that. Deciding "is the
   * folder I just renamed the SELECTED one?" from the captured `activeFolder`
   * answered `undefined`, and the rename never reached `activeFolder`: the list
   * showed the new name while every consumer of the active folder kept the old.
   *
   * Repro shape from `processes/chat/model/useConversationSidebar.test.tsx`:
   * capture the handler while nothing is selected, THEN select, THEN invoke
   * that exact reference.
   */
  it('updates the folder selected at CALL time from a handler captured BEFORE it was selected (stale-closure repro)', async () => {
    server.use(http.put(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({ id: 'f1', name: 'Renamed' })));

    const setActiveFolder = vi.fn();
    const folder = mkFolder({ id: 'f1' });
    const renamed = mkFolder({ id: 'f1', name: 'Renamed' });

    const { wrapper } = createWrapper(['models.chat.folders.update']);
    const { result, rerender } = renderHook(
      ({ activeFolder }: { activeFolder: FolderListItem | undefined }) =>
        useEditFolder({ projectId: '7', activeFolder, setActiveFolder, setFolders: vi.fn(), toastError: vi.fn() }),
      { wrapper, initialProps: { activeFolder: undefined as FolderListItem | undefined } },
    );

    // The reference a row rendered before any folder was selected carries.
    const staleEdit = result.current.onEditFolder;

    // Selected AFTER the handler was captured.
    rerender({ activeFolder: folder });

    await staleEdit(renamed);

    // A pre-fix build skips this entirely: its guard compares `undefined`.
    expect(setActiveFolder).toHaveBeenCalledWith(renamed);
  });

  /**
   * The same read, wrong in the other direction: a captured `activeFolder` that
   * is no longer selected made a rename CLOBBER the selection the user has since
   * made. `setActiveFolder` is the composition root's own `activeFolder` state
   * setter, so this silently moved the sidebar's selection under the user.
   */
  it('leaves a newly selected folder alone when a stale handler renames the previously selected one', async () => {
    server.use(http.put(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({ id: 'f1', name: 'Renamed' })));

    const setActiveFolder = vi.fn();
    const first = mkFolder({ id: 'f1' });
    const second = mkFolder({ id: 'f2' });

    const { wrapper } = createWrapper(['models.chat.folders.update']);
    const { result, rerender } = renderHook(
      ({ activeFolder }: { activeFolder: FolderListItem | undefined }) =>
        useEditFolder({ projectId: '7', activeFolder, setActiveFolder, setFolders: vi.fn(), toastError: vi.fn() }),
      { wrapper, initialProps: { activeFolder: first } },
    );

    const staleEdit = result.current.onEditFolder;
    rerender({ activeFolder: second });

    await staleEdit(mkFolder({ id: 'f1', name: 'Renamed' }));

    // A pre-fix build calls this with the renamed f1, replacing the user's f2.
    expect(setActiveFolder).not.toHaveBeenCalled();
  });

  /**
   * `onPinFolder` must NOT invalidate — matches `folderApi.updatePin`'s own
   * deliberate no-invalidate design (the caller does its own optimistic
   * `isPinned` update instead of relying on a refetch, mirroring the old
   * app's `invalidatesTags: []` for this exact endpoint).
   */
  it('onPinFolder does NOT invalidate the cached folder list', async () => {
    server.use(http.patch(`${BASE}/elitea_core/folder/prompt_lib/7/f1`, () => HttpResponse.json({ id: 'f1' })));
    const { wrapper, client } = createWrapper(['models.chat.folders.update']);
    const invalidateSpy = vi.spyOn(client, 'invalidateQueries');
    const { result } = renderHook(
      () => useEditFolder({ projectId: '7', activeFolder: undefined, setActiveFolder: vi.fn(), setFolders: vi.fn(), toastError: vi.fn() }),
      { wrapper },
    );

    await result.current.onPinFolder(mkFolder({ id: 'f1' }), true);

    expect(invalidateSpy).not.toHaveBeenCalled();
  });
});
