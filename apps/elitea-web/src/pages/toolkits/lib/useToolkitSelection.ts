import { useMemo, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { deleteApplicationTool } from '@/shared/api/generated/applications/applications';
import { getListToolkitInstancesQueryKey } from '@/shared/api/generated/toolkits/toolkits';
import { t } from '@/shared/i18n';
import { useListSelection } from '@/widgets/page-header';

interface ToolkitSelection {
  readonly selected: ReturnType<typeof useListSelection>;
  readonly confirmingDelete: boolean;
  readonly setConfirmingDelete: (open: boolean) => void;
  readonly deleting: boolean;
  readonly deleteError: string | undefined;
  /** Deletes every selected row, each on its own request; the rows that refuse stay selected and are counted. */
  readonly deleteSelected: () => Promise<void>;
}

/**
 * The toolkits and MCPs list's multi-select (ADR-0051 M1): the rows picked,
 * and Delete over them. A toolkit carries no tags and has no export, so the
 * selection section offers Delete only.
 */
export function useToolkitSelection(projectId: string | undefined, rows: readonly { readonly id: string }[]): ToolkitSelection {
  const queryClient = useQueryClient();
  const visibleIds = useMemo(() => rows.map((item) => item.id), [rows]);
  const selected = useListSelection(visibleIds);
  const [confirmingDelete, setConfirmingDelete] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string>();

  const deleteSelected = async (): Promise<void> => {
    setDeleting(true);
    setDeleteError(undefined);
    let failed = 0;
    for (const id of selected.ids) {
      try {
        await deleteApplicationTool(projectId ?? '', Number(id));
      } catch {
        failed += 1;
      }
    }
    if (failed > 0) setDeleteError(t('pages.toolkits.toolkits.deleteFailed', '{{count}} could not be deleted.', { count: failed }));
    await queryClient.invalidateQueries({ queryKey: getListToolkitInstancesQueryKey(projectId ?? '') }).catch(() => undefined);
    setDeleting(false);
    setConfirmingDelete(false);
  };

  return { selected, confirmingDelete, setConfirmingDelete, deleting, deleteError, deleteSelected };
}
