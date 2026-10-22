/**
 * State, data and write handlers for `pages/admin/SchedulesTasks.tsx` (unit A14).
 *
 * Split out of the page for the same reason `useAdminRolesPage` is: the page
 * component stays a render, and the branching that decides WHICH controls exist
 * lives in one place.
 *
 * ## Sorting and filtering are CLIENT-side here, deliberately
 *
 * `centry.schedule` is a platform table with tens of rows, not a tenant table,
 * and the endpoint is unpaginated in the legacy runtime and in the Go handler
 * alike. Adding server-side search would be inventing a contract; sorting a
 * fully-materialised list in the browser is the whole of it. This is the
 * opposite call from the admin Projects page, where the server pages and
 * therefore must also sort.
 */
import { useCallback, useMemo, useState } from 'react';

import { adminUiShowsControlFor } from './adminUiConfig';
import {
  scheduleFailureReason,
  useAdminSchedules,
  useUpdateAdminSchedule,
  type AdminScheduleRow,
} from './api/adminSchedulesApi';

/** The permission the (hardcoded) admin-panel config advertises for editing. */
const PERMISSION_SCHEDULES_EDIT = 'configuration.scheduling.schedules.edit';

export type ScheduleSortField = 'name' | 'rpc_func' | 'last_run';
type SortDirection = 'asc' | 'desc';

export interface ScheduleSort {
  readonly field: ScheduleSortField;
  readonly direction: SortDirection;
}

export interface AdminSchedulesPageState {
  readonly search: string;
  readonly rows: readonly AdminScheduleRow[] | undefined;
  readonly sort: ScheduleSort;
  readonly isFetching: boolean;
  readonly isError: boolean;
  /** The server's own words when the listing is refused. */
  readonly unavailableReason: string | undefined;
  readonly isSaving: boolean;
  readonly errorMessage: string;
  readonly savedMessage: string;

  /** Presentation only — the server authorises the write on its own. */
  readonly canEdit: boolean;

  /** The schedule whose execution history is open, or `null`. */
  readonly historySchedule: AdminScheduleRow | null;

  readonly onSearchChange: (value: string) => void;
  readonly onSort: (field: ScheduleSortField) => void;
  readonly onDismissError: () => void;
  readonly onDismissSaved: () => void;
  readonly onOpenHistory: (schedule: AdminScheduleRow) => void;
  readonly onCloseHistory: () => void;
  /** `undefined` ⇒ the switch renders disabled, because the server would refuse. */
  readonly onToggleActive: ((schedule: AdminScheduleRow) => void) | undefined;
  /** `undefined` ⇒ the cron cell is not editable. */
  readonly onCronChange: ((schedule: AdminScheduleRow, cron: string) => void) | undefined;
}

function compareRows(
  left: AdminScheduleRow,
  right: AdminScheduleRow,
  field: ScheduleSortField,
): number {
  if (field === 'last_run') {
    // A schedule that has never run sorts LAST in both directions rather than
    // as the epoch: "never" is not "a very long time ago", and treating it as
    // the oldest timestamp buries the rows an operator most wants to notice.
    if (left.last_run === null && right.last_run === null) return 0;
    if (left.last_run === null) return 1;
    if (right.last_run === null) return -1;
    return left.last_run.localeCompare(right.last_run);
  }
  return left[field].localeCompare(right[field]);
}

export function useAdminSchedulesPage(): AdminSchedulesPageState {
  const [search, setSearch] = useState('');
  const [sort, setSort] = useState<ScheduleSort>({ field: 'name', direction: 'asc' });
  const [errorMessage, setErrorMessage] = useState('');
  const [savedMessage, setSavedMessage] = useState('');
  const [historySchedule, setHistorySchedule] = useState<AdminScheduleRow | null>(null);

  const schedulesQuery = useAdminSchedules();
  const updateSchedule = useUpdateAdminSchedule();

  const canEdit = adminUiShowsControlFor(PERMISSION_SCHEDULES_EDIT);

  const serverRows = schedulesQuery.data;

  const rows = useMemo((): readonly AdminScheduleRow[] | undefined => {
    if (!serverRows) return undefined;
    const needle = search.trim().toLowerCase();
    const filtered = needle
      ? serverRows.filter(
          (row) =>
            row.name.toLowerCase().includes(needle) || row.rpc_func.toLowerCase().includes(needle),
        )
      : serverRows;
    // A copy: `Array.prototype.sort` mutates, and the array here is the query
    // cache's own.
    const sorted = [...filtered].sort((left, right) => compareRows(left, right, sort.field));
    return sort.direction === 'asc' ? sorted : sorted.reverse();
  }, [serverRows, search, sort]);

  const reportFailure = useCallback((error: unknown) => {
    setErrorMessage(scheduleFailureReason(error) ?? 'save');
  }, []);

  const applyUpdate = useCallback(
    (update: { id: number; cron?: string; active?: boolean }) => {
      setErrorMessage('');
      setSavedMessage('');
      updateSchedule.mutate(update, {
        onSuccess: () => setSavedMessage('saved'),
        onError: reportFailure,
      });
    },
    [updateSchedule, reportFailure],
  );

  const onToggleActive = useCallback(
    (schedule: AdminScheduleRow) => applyUpdate({ id: schedule.id, active: !schedule.active }),
    [applyUpdate],
  );

  const onCronChange = useCallback(
    (schedule: AdminScheduleRow, cron: string) => {
      const next = cron.trim();
      // An unchanged cron is not a save. Sending it anyway would report success
      // for an edit the operator abandoned, and would stamp the audit trail
      // with a change that did not happen.
      if (next === '' || next === schedule.cron) return;
      applyUpdate({ id: schedule.id, cron: next });
    },
    [applyUpdate],
  );

  const onSort = useCallback((field: ScheduleSortField) => {
    setSort((previous) =>
      previous.field === field
        ? { field, direction: previous.direction === 'asc' ? 'desc' : 'asc' }
        : { field, direction: 'asc' },
    );
  }, []);

  return {
    search,
    rows,
    sort,
    isFetching: schedulesQuery.isFetching,
    isError: schedulesQuery.isError,
    unavailableReason: scheduleFailureReason(schedulesQuery.error),
    isSaving: updateSchedule.isPending,
    errorMessage,
    savedMessage,
    canEdit,
    historySchedule,
    onSearchChange: setSearch,
    onSort,
    onDismissError: useCallback(() => setErrorMessage(''), []),
    onDismissSaved: useCallback(() => setSavedMessage(''), []),
    onOpenHistory: useCallback((schedule: AdminScheduleRow) => setHistorySchedule(schedule), []),
    onCloseHistory: useCallback(() => setHistorySchedule(null), []),
    onToggleActive: canEdit ? onToggleActive : undefined,
    onCronChange: canEdit ? onCronChange : undefined,
  };
}
