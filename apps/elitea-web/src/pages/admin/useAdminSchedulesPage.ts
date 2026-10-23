/**
 * State, data and write handlers for `pages/admin/SchedulesTasks.tsx`
 * (issue #1369).
 *
 * Sorting and filtering are CLIENT-side: the registry holds a dozen rows, and
 * the endpoint is unpaginated.
 */
import { useCallback, useMemo, useState } from 'react';

import { adminUiShowsControlFor } from './adminUiConfig';
import {
  scheduleFailureReason,
  useAdminRoutines,
  useUpdateAdminRoutine,
  type AdminRoutineRow,
} from './api/adminSchedulesApi';

/** The permission the (hardcoded) admin-panel config advertises for editing. */
const PERMISSION_SCHEDULES_EDIT = 'configuration.scheduling.schedules.edit';

export type ScheduleSortField = 'id' | 'last_run_at' | 'next_run_at';
type SortDirection = 'asc' | 'desc';

export interface ScheduleSort {
  readonly field: ScheduleSortField;
  readonly direction: SortDirection;
}

export interface AdminSchedulesPageState {
  readonly search: string;
  readonly rows: readonly AdminRoutineRow[] | undefined;
  readonly sort: ScheduleSort;
  readonly isFetching: boolean;
  readonly isError: boolean;
  /** The server's own words when the listing is refused. */
  readonly unavailableReason: string | undefined;
  readonly isSaving: boolean;
  readonly errorMessage: string;
  readonly savedMessage: string;

  /** Presentation only - the server authorises the write on its own. */
  readonly canEdit: boolean;

  readonly onSearchChange: (value: string) => void;
  readonly onSort: (field: ScheduleSortField) => void;
  readonly onDismissError: () => void;
  readonly onDismissSaved: () => void;
  /** `undefined` => the switch renders disabled, because the server would refuse. */
  readonly onToggleEnabled: ((routine: AdminRoutineRow) => void) | undefined;
  /** `undefined` => the schedule cell is not editable. An empty value resets to the default. */
  readonly onScheduleChange: ((routine: AdminRoutineRow, schedule: string) => void) | undefined;
}

function compareRows(left: AdminRoutineRow, right: AdminRoutineRow, field: ScheduleSortField): number {
  if (field === 'id') return left.id.localeCompare(right.id);
  const a = left[field];
  const b = right[field];
  // "Never" and "not planned" sort LAST in both directions rather than as the
  // epoch: absence is not "a very long time ago".
  if (a === null && b === null) return 0;
  if (a === null) return 1;
  if (b === null) return -1;
  return a.localeCompare(b);
}

export function useAdminSchedulesPage(): AdminSchedulesPageState {
  const [search, setSearch] = useState('');
  const [sort, setSort] = useState<ScheduleSort>({ field: 'id', direction: 'asc' });
  const [errorMessage, setErrorMessage] = useState('');
  const [savedMessage, setSavedMessage] = useState('');

  const routinesQuery = useAdminRoutines();
  const updateRoutine = useUpdateAdminRoutine();

  const canEdit = adminUiShowsControlFor(PERMISSION_SCHEDULES_EDIT);
  const serverRows = routinesQuery.data;

  const rows = useMemo((): readonly AdminRoutineRow[] | undefined => {
    if (!serverRows) return undefined;
    const needle = search.trim().toLowerCase();
    const filtered = needle
      ? serverRows.filter(
          (row) =>
            row.id.toLowerCase().includes(needle) || row.description.toLowerCase().includes(needle),
        )
      : serverRows;
    const sorted = [...filtered].sort((left, right) => compareRows(left, right, sort.field));
    if (sort.direction === 'asc') return sorted;
    // Descending reverses the comparator but keeps absent timestamps last.
    if (sort.field === 'id') return sorted.reverse();
    const present = sorted.filter((row) => row[sort.field] !== null).reverse();
    return [...present, ...sorted.filter((row) => row[sort.field] === null)];
  }, [serverRows, search, sort]);

  const reportFailure = useCallback((error: unknown) => {
    setErrorMessage(scheduleFailureReason(error) ?? 'save');
  }, []);

  const applyUpdate = useCallback(
    (update: { id: string; enabled?: boolean; schedule?: string }) => {
      setErrorMessage('');
      setSavedMessage('');
      updateRoutine.mutate(update, {
        onSuccess: () => setSavedMessage('saved'),
        onError: reportFailure,
      });
    },
    [updateRoutine, reportFailure],
  );

  const onToggleEnabled = useCallback(
    (routine: AdminRoutineRow) => applyUpdate({ id: routine.id, enabled: !routine.enabled }),
    [applyUpdate],
  );

  const onScheduleChange = useCallback(
    (routine: AdminRoutineRow, schedule: string) => {
      const next = schedule.trim();
      // An unchanged schedule is not a save: it would report success for an edit
      // the operator abandoned and stamp the audit trail with a change that did
      // not happen. An empty value is a deliberate reset, but only when there is
      // an override to remove.
      if (next === routine.schedule) return;
      if (next === '' && routine.schedule_override === null) return;
      applyUpdate({ id: routine.id, schedule: next });
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
    isFetching: routinesQuery.isFetching,
    isError: routinesQuery.isError,
    unavailableReason: scheduleFailureReason(routinesQuery.error),
    isSaving: updateRoutine.isPending,
    errorMessage,
    savedMessage,
    canEdit,
    onSearchChange: setSearch,
    onSort,
    onDismissError: useCallback(() => setErrorMessage(''), []),
    onDismissSaved: useCallback(() => setSavedMessage(''), []),
    onToggleEnabled: canEdit ? onToggleEnabled : undefined,
    onScheduleChange: canEdit ? onScheduleChange : undefined,
  };
}
