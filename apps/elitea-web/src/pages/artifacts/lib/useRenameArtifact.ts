/**
 * Rename of one artifact file (#2015), split out of `Artifacts.tsx` to keep it
 * under the 400-line budget.
 *
 * - Offered only to a caller holding BOTH `artifacts.create` and
 *   `artifacts.delete`: the server's move route needs the pair (it writes the
 *   new key and removes the old one), so showing Rename to anyone else only
 *   produced a 403. While the permission list loads, nothing is offered.
 * - The folder part of the key stays; only the file name changes.
 * - A selection that no longer exists (deleted or renamed elsewhere since the
 *   list was read) is refused up front instead of sent to the server.
 * - After a successful rename, an open preview or canvas that points at the
 *   old key follows the file (preview) or is closed (canvas), so saving it
 *   cannot re-create the old name.
 */
import { useCallback, useMemo, useState } from 'react';

import type { ArtifactListItem } from '@/features/artifacts';
import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';

interface RenameMutation {
  readonly mutateAsync: (input: { bucket: string; from: string; to: string }) => Promise<unknown>;
  readonly isPending: boolean;
}

interface UseRenameArtifactInput {
  readonly projectId: string | undefined;
  readonly bucket: string | undefined;
  readonly existingKeys: readonly string[];
  readonly rename: RenameMutation;
  /** The key the preview is showing, if any. */
  readonly previewKey: string | undefined;
  readonly onPreviewMoved: (newKey: string) => void;
  readonly onKeyRenamed: (oldKey: string) => void;
}

export interface UseRenameArtifactResult {
  /** Undefined when the caller may not rename. */
  readonly open: ((item: ArtifactListItem) => void) | undefined;
  readonly pending: ArtifactListItem | undefined;
  readonly pendingMutation: boolean;
  readonly error: string | undefined;
  readonly staleError: string | undefined;
  readonly close: () => void;
  readonly confirm: (name: string) => void;
}

export function useRenameArtifact(input: UseRenameArtifactInput): UseRenameArtifactResult {
  const permissions = usePermissionList(input.projectId ?? '', { query: { enabled: input.projectId !== undefined } });
  const allowed = useMemo(() => {
    const list = permissions.data?.data as Permission[] | undefined;
    const granted = new Set((list ?? []).filter((entry) => entry.enabled).map((entry) => entry.name));
    return granted.has(PERMISSIONS.artifacts.create) && granted.has(PERMISSIONS.artifacts.delete);
  }, [permissions.data]);
  const [pending, setPending] = useState<ArtifactListItem>();
  const [error, setError] = useState<string>();
  const [staleError, setStaleError] = useState<string>();
  const { bucket, existingKeys, rename, previewKey, onPreviewMoved, onKeyRenamed } = input;

  const stale = useCallback(
    (key: string): boolean => {
      if (existingKeys.includes(key)) return false;
      setStaleError(t('artifacts.rename.stale', 'That file no longer exists, so it cannot be renamed.'));
      return true;
    },
    [existingKeys],
  );

  const open = useCallback(
    (item: ArtifactListItem): void => {
      setStaleError(undefined);
      if (stale(item.key)) return;
      setError(undefined);
      setPending(item);
    },
    [stale],
  );

  const confirm = useCallback(
    (name: string): void => {
      if (bucket === undefined || pending === undefined) return;
      if (stale(pending.key)) {
        setPending(undefined);
        return;
      }
      // The folder part of the key stays; only the file name changes (#2015).
      const folder = pending.key.slice(0, pending.key.lastIndexOf('/') + 1);
      const from = pending.key;
      const to = `${folder}${name}`;
      setError(undefined);
      void rename
        .mutateAsync({ bucket, from, to })
        .then(() => {
          setPending(undefined);
          if (previewKey === from) onPreviewMoved(to);
          onKeyRenamed(from);
        })
        .catch((cause: unknown) => {
          setError(cause instanceof Error ? cause.message : 'Failed to rename the file.');
        });
    },
    [bucket, onKeyRenamed, onPreviewMoved, pending, previewKey, rename, stale],
  );

  return {
    open: allowed ? open : undefined,
    pending,
    pendingMutation: rename.isPending,
    error,
    staleError,
    close: () => setPending(undefined),
    confirm,
  };
}
