import { useCallback, useEffect, useState } from 'react';
import type { Dispatch, SetStateAction } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { folderApi } from '@/entities/folder';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { conversationListErrorMessage } from '../errorMessage';
import { pinnedFoldersFirst } from '../helpers/conversationList.helpers';
import { useHasPermission } from '../useHasPermission';
import type { FolderListItem } from './conversationListState.types';
import { useLatestRef } from './useLatestRef';

export interface UseEditFolderParams {
  readonly projectId: string | undefined;
  readonly activeFolder: FolderListItem | undefined;
  readonly setActiveFolder: (folder: FolderListItem) => void;
  readonly setFolders: Dispatch<SetStateAction<readonly FolderListItem[]>>;
  readonly toastError: (message: string) => void;
}

export interface UseEditFolderResult {
  readonly onEditFolder: (folder: FolderListItem) => Promise<void>;
  /** `is_pinned` (baseline) renamed to `isPinned` at this hook's boundary — camelCase, matching `Folder.isPinned` everywhere else in this domain; translated back to the wire's `is_pinned` only at the `folderApi.updatePin` call site. */
  readonly onPinFolder: (folder: FolderListItem, isPinned: boolean) => Promise<void>;
}

/**
 * Ported from `apps/elitea-ui/src/hooks/chat/useEditFolder.js` (unit C2).
 *
 * **Permission-gate reinterpretation, disclosed.** The baseline's
 * `onEditFolder` passes `{skip: !projectId || !checkPermission(...)}` as an
 * RTK-Query mutation-trigger option — but RTK Query mutation TRIGGER
 * functions (unlike query hooks) don't actually support a `skip` option;
 * passing one has no documented effect on whether the network call fires,
 * so the baseline's own gate is ambiguous-to-actively-broken: a
 * permission-denied `onEditFolder` in the baseline still fires the real PUT,
 * and when the backend also rejects it with a 403 the baseline's own
 * `isError`/`error` `useEffect` (its lines 64-68) calls
 * `toastError(buildErrorMessage(error))`, surfacing a toast. This port
 * mirrors the SAME file's other, unambiguous gate —
 * `onPinFolder`'s `if (!checkPermission(...)) return;`, a real early return
 * that blocks the entire action, network call and local-state update
 * alike — for the SHAPE of the gate (skip the network round trip rather
 * than porting an ambiguous no-op), but — regression fixed here (found by
 * adversarial verify) — `onEditFolder`'s gate additionally calls
 * `toastError` before returning, so a permission-denied rename still
 * surfaces the same user-visible feedback the baseline's real network round
 * trip produced. `onPinFolder`'s own gate is left as a true silent no-op:
 * its baseline early return really was unambiguous and never fired a
 * request, so there is no baseline toast to preserve parity with.
 *
 * **`meta` dropped from the PUT body, disclosed.** The baseline sends
 * `{projectId, id, name: folder.name, meta: folder.meta}`. This codebase's
 * `Folder` domain type has no `meta` field at all — pinned state is
 * normalised to the top-level `isPinned`, updated through the SEPARATE
 * `folderApi.updatePin` endpoint (`onPinFolder` below), not through this
 * rename call. Only `{name}` is sent; whether the backend's PUT handler
 * treats an omitted `meta` as "leave unchanged" or "reset" is a backend
 * contract question outside this port's scope — flagged for whoever wires
 * the real page against a live backend.
 */
export function useEditFolder(params: UseEditFolderParams): UseEditFolderResult {
  const { projectId, activeFolder, setActiveFolder, setFolders, toastError } = params;
  const hasUpdatePermission = useHasPermission(projectId, PERMISSIONS.chat.folders.update);
  /**
   * "Is the folder I just renamed the SELECTED one?" is answered when the
   * rename is confirmed — and, since the answer is used after `await
   * folderApi.update`, a whole round trip after that. Read live, per
   * `processes/chat/model/useConversationSidebar.ts`'s ref doc block.
   *
   * The captured value was wrong in both directions. Stale `undefined` (the
   * closure a `FolderItem` rendered before the folder was selected carries):
   * renaming the selected folder left `activeFolder` holding the OLD name, so
   * every consumer of the active folder kept showing it. Stale
   * `folder-that-was-selected`: renaming that folder after the user moved on
   * called `setActiveFolder` and CLOBBERED the selection they had since made.
   * Both disappear once the comparison happens at call time.
   */
  const activeFolderRef = useLatestRef(activeFolder);
  const [editError, setEditError] = useState<unknown>(undefined);
  const queryClient = useQueryClient();

  const onEditFolder = useCallback(
    async (folder: FolderListItem): Promise<void> => {
      if (projectId === undefined) return;
      if (!hasUpdatePermission) {
        toastError('You do not have permission to edit folders');
        return;
      }

      if (!folder.isPlayback) {
        try {
          await folderApi.update({ projectId, id: folder.id, name: folder.name });
          setEditError(undefined);
          // `folderApi.update` is the plain fetcher — `folderApi.useUpdate`
          // (which invalidates on its own) has no consumers anywhere.
          // Found missing by adversarial verify (same class of gap as
          // `useCreateFolder.hooks.ts`'s own fix — see that file's doc
          // comment).
          void queryClient.invalidateQueries({ queryKey: ['folder', 'list'] });
        } catch (caught) {
          setEditError(caught);
          return;
        }
      }

      // Live, not the render-time `activeFolder`: this line runs after the PUT.
      const active = activeFolderRef.current;
      if (active !== undefined && folder.id === active.id) setActiveFolder(folder);
      setFolders((prev) => prev.map((item) => (item.id === folder.id ? folder : item)));
    },
    [projectId, hasUpdatePermission, activeFolderRef, setActiveFolder, setFolders, queryClient, toastError],
  );

  const onPinFolder = useCallback(
    async (folder: FolderListItem, isPinned: boolean): Promise<void> => {
      if (projectId === undefined || !hasUpdatePermission) return;

      try {
        await folderApi.updatePin({ projectId, id: folder.id, is_pinned: isPinned });
        setFolders((prev) => pinnedFoldersFirst(prev.map((item) => (item.id === folder.id ? { ...item, isPinned } : item))));
        // Pinning needs no read: the folder goes to the front of the pinned ones and the
        // others keep their order. UNpinning does: the folder's own place among the unpinned
        // is the server's (its position), which the client no longer has, so the list is read
        // again and `useQueryFoldersList` applies the same pinned-first rule to it.
        if (!isPinned) void queryClient.invalidateQueries({ queryKey: ['folder', 'list'] });
      } catch (caught) {
        toastError(conversationListErrorMessage(caught));
      }
    },
    [projectId, hasUpdatePermission, setFolders, toastError, queryClient],
  );

  useEffect(() => {
    if (editError !== undefined) toastError(conversationListErrorMessage(editError));
  }, [editError, toastError]);

  return { onEditFolder, onPinFolder };
}
