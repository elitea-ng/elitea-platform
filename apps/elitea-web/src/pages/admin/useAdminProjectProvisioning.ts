/**
 * Create/delete state for the admin Projects page — the front half of the
 * provisioning pipeline (#333).
 *
 * Separate from `./useAdminProjectsPage` rather than folded into it, for the
 * reason that hook's own header gives about the page component: these two
 * controls carry more state than the other five writes combined (a selection, a
 * dialog each, an in-flight flag each, a step list, and a per-project failure
 * list from a loop), and none of it is shared with the listing. Merging them
 * would put the page's paging state and its destructive path in one reducer.
 *
 * ## Selection is cleared whenever the listing changes
 *
 * Ids are the whole of the selection, and the rows behind them come from ONE
 * page of ONE tab of ONE search. Keeping a selection across a tab change would
 * arm the delete dialog with ids whose rows are no longer on screen — the
 * operator would confirm a list they cannot see. `clearSelection` is called from
 * every control that changes which rows are listed.
 *
 * ## The delete loop
 *
 * There is no batch route: the page sends one DELETE per project and collects
 * the failures. It runs SEQUENTIALLY, not with `Promise.all`, because each call
 * drops a schema and runs a migration-ledger cleanup against the same database;
 * firing five at once buys nothing an operator can perceive and makes a partial
 * failure harder to attribute.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { t } from '@/shared/i18n';

import { ADMIN_ITEM } from './adminItemIds';
import { useAdminWritable } from './adminWriteAccess';
import type { ProjectDeleteFailure } from './AdminProjectDeleteFacts';
import type { AdminReason } from './adminDangerousAction';
import {
  failedProvisioningSteps,
  provisioningStepsFromError,
  useCreateAdminProject,
  useDeleteAdminProject,
  NO_PROVISIONING_FAILURE,
  type CreateProjectInput,
  type ProvisioningFailure,
} from './api/adminProjectProvisioningApi';
import { adminProjectsKeys, type AdminProjectRow } from './api/adminProjectsApi';

/*
 * Create and delete are offered to every caller the projects item opens to;
 * at its `read` level the page disables them (R3.4b, issue 2613) and both
 * writes go through the read-only guard. The server gates each on its own
 * permission (`projects.projects.project.{create,delete}`) regardless.
 */

export interface AdminProjectProvisioningState {
  readonly selectedIds: readonly number[];
  readonly selectedProjects: readonly AdminProjectRow[];
  /** `undefined` ⇒ no checkbox column: this operator may not delete. */
  readonly onSelectionChange: ((ids: number[]) => void) | undefined;
  readonly clearSelection: () => void;

  /** `undefined` ⇒ the control is not rendered for this operator. */
  readonly onOpenCreate: (() => void) | undefined;
  readonly isCreateOpen: boolean;
  readonly isCreating: boolean;
  readonly createError: string | undefined;
  /** The failed forward steps and the failed rollback steps, kept apart. */
  readonly createFailure: ProvisioningFailure;
  readonly onCloseCreate: () => void;
  readonly onCreate: (input: CreateProjectInput) => void;

  /** `undefined` ⇒ the control is not rendered for this operator. */
  readonly onOpenDelete: (() => void) | undefined;
  readonly isDeleteOpen: boolean;
  readonly isDeleting: boolean;
  readonly deleteFailures: readonly ProjectDeleteFailure[];
  readonly onCloseDelete: () => void;
  readonly onConfirmDelete: (reason: AdminReason) => void;
}

function messageOf(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

export function useAdminProjectProvisioning(
  rows: readonly AdminProjectRow[],
): AdminProjectProvisioningState {
  const [selectedIds, setSelectedIds] = useState<readonly number[]>([]);
  const [isCreateOpen, setCreateOpen] = useState(false);
  const [isDeleteOpen, setDeleteOpen] = useState(false);
  const [createError, setCreateError] = useState<string | undefined>(undefined);
  const [createFailure, setCreateFailure] = useState<ProvisioningFailure>(NO_PROVISIONING_FAILURE);
  const [deleteFailures, setDeleteFailures] = useState<readonly ProjectDeleteFailure[]>([]);
  const [isDeleting, setDeleting] = useState(false);

  const access = useAdminWritable(ADMIN_ITEM.projects);
  const createProject = access.guard(useCreateAdminProject());
  const deleteProject = access.guard(useDeleteAdminProject());
  const queryClient = useQueryClient();

  /**
   * The rows behind the selected ids, in listing order.
   *
   * Derived from `rows` rather than stored when the checkbox was ticked, so a
   * refetch that renamed or removed a project cannot leave the confirmation
   * dialog listing a stale name. An id whose row is gone drops out of the list
   * — and out of what the dialog offers to delete.
   */
  const selectedProjects = useMemo(() => {
    const wanted = new Set(selectedIds);
    return rows.filter((row) => wanted.has(row.id));
  }, [rows, selectedIds]);

  const clearSelection = useCallback(() => setSelectedIds([]), []);

  /*
   * An empty selection CLOSES the delete dialog, rather than merely hiding it.
   *
   * The dialog's open flag lives here while the dialog itself is mounted by the
   * page only when something is selected. Without this reset the two can
   * disagree: a background refetch (`refetchOnWindowFocus` is on, with a 30s
   * stale time) that drops the selected rows — because another operator deleted
   * or displaced them — unmounts the dialog and leaves the flag set. The next
   * checkbox the operator ticks would then pop the destructive confirmation
   * open by itself, listing a project they never asked to delete.
   *
   * Resetting the STATE is the fix; masking the flag on the way out would leave
   * it armed to fire again on the next selection.
   */
  useEffect(() => {
    if (selectedProjects.length === 0) setDeleteOpen(false);
  }, [selectedProjects.length]);

  /*
   * The three handlers whose identity reaches a memoized child, or a dependency
   * array, are stabilised here rather than written inline at the return. An
   * inline arrow is a new identity every render, which defeats
   * `AdminProjectsTable`'s `memo` on every keystroke in the search box.
   */
  const onSelectionChange = useCallback((ids: number[]) => setSelectedIds(ids), []);

  const onOpenCreate = useCallback(() => {
    setCreateError(undefined);
    setCreateFailure(NO_PROVISIONING_FAILURE);
    setCreateOpen(true);
  }, []);

  const onOpenDelete = useCallback(() => {
    setDeleteFailures([]);
    setDeleteOpen(true);
  }, []);

  const onCloseCreate = useCallback(() => {
    setCreateOpen(false);
    setCreateError(undefined);
    setCreateFailure(NO_PROVISIONING_FAILURE);
  }, []);

  const onCreate = useCallback(
    (input: CreateProjectInput) => {
      setCreateError(undefined);
      setCreateFailure(NO_PROVISIONING_FAILURE);
      createProject.mutate(input, {
        onSuccess: () => {
          setCreateOpen(false);
        },
        onError: (error) => {
          setCreateError(
            messageOf(
              error,
              t('pages.admin.projects.error.create', 'Failed to create the project.'),
            ),
          );
          // The steps come off the REJECTED response's body, which is the only
          // place they exist — the dialog reports the position in the pipeline,
          // not just that something went wrong.
          setCreateFailure(failedProvisioningSteps(provisioningStepsFromError(error)));
        },
      });
    },
    [createProject],
  );

  const onCloseDelete = useCallback(() => {
    setDeleteOpen(false);
    setDeleteFailures([]);
  }, []);

  /**
   * Delete every selected project, one request at a time.
   *
   * A project that fails does NOT stop the ones after it: the operator asked for
   * five and there is no reason a refusal on the second should silently spare
   * the last three. Failures are collected and reported by name; the dialog
   * stays open when there are any, and the ids that DID succeed leave the
   * selection so that a retry re-runs only what is left.
   */
  const onConfirmDelete = useCallback((reason: AdminReason) => {
    const targets = selectedProjects;
    if (targets.length === 0) return;
    setDeleting(true);
    setDeleteFailures([]);
    void (async () => {
      const failures: ProjectDeleteFailure[] = [];
      for (const project of targets) {
        try {
          await deleteProject.mutateAsync({ projectId: project.id, reason });
        } catch (error) {
          failures.push({
            projectId: project.id,
            name: project.name,
            message: messageOf(
              error,
              t('pages.admin.projects.error.delete', 'the server refused the delete'),
            ),
          });
        }
      }
      // ONE invalidation for the whole loop. Doing it per mutation would make
      // each iteration await a full listing refetch before the next DELETE went
      // out; see `useDeleteAdminProject`.
      await queryClient.invalidateQueries({ queryKey: adminProjectsKeys.all });
      setDeleting(false);
      setDeleteFailures(failures);
      const stillSelected = new Set(failures.map((failure) => failure.projectId));
      setSelectedIds((previous) => previous.filter((id) => stillSelected.has(id)));
      if (failures.length === 0) setDeleteOpen(false);
    })();
  }, [selectedProjects, deleteProject, queryClient]);

  return {
    selectedIds,
    selectedProjects,
    onSelectionChange,
    clearSelection,

    onOpenCreate,
    isCreateOpen,
    isCreating: createProject.isPending,
    createError,
    createFailure,
    onCloseCreate,
    onCreate,

    onOpenDelete,
    isDeleteOpen,
    isDeleting,
    deleteFailures,
    onCloseDelete,
    onConfirmDelete,
  };
}
