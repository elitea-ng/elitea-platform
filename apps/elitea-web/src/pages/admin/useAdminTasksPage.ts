/**
 * State, data and the cancel for `pages/admin/Tasks.tsx`.
 *
 * Split out of the page for the reason every sibling hook is: the page stays a
 * render, and the branching that decides which controls exist lives in one
 * place.
 *
 * ## Filtering and paging are SERVER-side
 *
 * The opposite call from `useAdminSchedulesPage`, and for the opposite reason.
 * A schedule table holds tens of rows and the endpoint is unpaginated, so the
 * browser can sort the whole of it. The job union is bounded only by the
 * server's own scan window, and the server already filters and pages it — so
 * filtering here would page over a subset of a subset and report a `total` that
 * belongs to neither.
 *
 * ## Changing a filter returns to page one
 *
 * A filter change that keeps the offset lands the operator on an empty page of
 * a shorter list, which reads as "no jobs match" for a filter that matches
 * plenty.
 */
import { useCallback, useState } from 'react';

import { t } from '@/shared/i18n';
import { EliteaApiError } from '@/shared/api/generated/mutator';

import {
  useAdminTasks,
  useCancelAdminTask,
  type AdminTaskRow,
} from './api/adminTasksApi';

/** The reference polls its Tasks tab; ten seconds — see `Tasks.tsx`. */
const REFRESH_INTERVAL_MS = 10_000;
const PAGE_SIZE = 50;

export interface AdminTasksPageState {
  readonly rows: readonly AdminTaskRow[];
  readonly total: number;
  readonly truncated: boolean;
  readonly isFetching: boolean;
  readonly isCancelling: boolean;
  /** The SERVER's reason when the listing is refused, not a copy of it. */
  readonly unavailableReason: string | undefined;
  readonly errorMessage: string | undefined;
  readonly kind: string;
  readonly status: string;
  readonly hasPrevious: boolean;
  readonly hasNext: boolean;
  readonly setKind: (kind: string) => void;
  readonly setStatus: (status: string) => void;
  readonly previousPage: () => void;
  readonly nextPage: () => void;
  readonly cancel: (row: AdminTaskRow) => void;
}

/**
 * The server's own sentence for a refused listing.
 *
 * A 503 from this route carries a reason — "this deployment has no
 * background-job store configured". Rendering THAT rather than a local
 * "unavailable" string is what makes the page move when the server's answer
 * changes, and is the same rule `SchedulesTasks.tsx` follows.
 */
function refusalReason(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  if (failure.kind !== 'http') return undefined;
  if (failure.status !== 501 && failure.status !== 503) return undefined;
  const body = failure.body;
  if (typeof body === 'object' && body !== null && typeof (body as { error?: unknown }).error === 'string') {
    return (body as { error: string }).error;
  }
  return t('pages.admin.tasks.unavailable', 'This deployment cannot report its background jobs.');
}

/**
 * The ONE message the page shows, or none.
 *
 * A cancel failure wins over a read failure: it is the thing the operator just
 * did. A refusal (the 503 branch) suppresses the read failure entirely — the
 * page already renders the server's own sentence for it, and a second, vaguer
 * error beside it would read as two problems.
 */
function pageErrorMessage(
  cancelFailed: boolean,
  readFailed: boolean,
  unavailableReason: string | undefined,
): string | undefined {
  if (cancelFailed) {
    return t('pages.admin.tasks.error.cancel', 'Failed to stop the job. It may have already finished.');
  }
  if (readFailed && unavailableReason === undefined) {
    return t('pages.admin.tasks.error.load', 'Failed to read the background jobs.');
  }
  return undefined;
}

export function useAdminTasksPage(
  refetchInterval: number | false | undefined = REFRESH_INTERVAL_MS,
): AdminTasksPageState {
  const interval = refetchInterval ?? REFRESH_INTERVAL_MS;
  const [kind, setKindState] = useState('');
  const [status, setStatusState] = useState('');
  const [offset, setOffset] = useState(0);
  const [cancelFailed, setCancelFailed] = useState(false);

  const query = useAdminTasks({ kind, status, limit: PAGE_SIZE, offset }, interval);
  const cancelTask = useCancelAdminTask();

  const page = query.data;
  const rows = page?.rows ?? [];
  const total = page?.total ?? 0;
  const unavailableReason = refusalReason(query.error);

  const setKind = useCallback((next: string) => {
    setKindState(next);
    setOffset(0);
  }, []);
  const setStatus = useCallback((next: string) => {
    setStatusState(next);
    setOffset(0);
  }, []);

  const cancel = useCallback((row: AdminTaskRow) => {
    setCancelFailed(false);
    cancelTask.mutate(
      { kind: row.kind, taskId: row.task_id, projectId: row.project_id },
      { onError: () => setCancelFailed(true) },
    );
  }, [cancelTask]);

  return {
    rows,
    total,
    truncated: page?.truncated ?? false,
    isFetching: query.isFetching,
    isCancelling: cancelTask.isPending,
    unavailableReason,
    errorMessage: pageErrorMessage(cancelFailed, query.isError, unavailableReason),
    kind,
    status,
    hasPrevious: offset > 0,
    hasNext: offset + PAGE_SIZE < total,
    setKind,
    setStatus,
    previousPage: () => setOffset((current) => Math.max(0, current - PAGE_SIZE)),
    nextPage: () => setOffset((current) => current + PAGE_SIZE),
    cancel,
  };
}
