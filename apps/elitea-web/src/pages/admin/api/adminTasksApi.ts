/**
 * REST client for `Admin › Tasks` — the platform's own background jobs.
 *
 * GENERATED, unlike its neighbours in this folder: the two operations ARE
 * described in `v2.yaml` (`listBackgroundJobs`, `cancelBackgroundJob`), so the
 * hooks come from orval and this module holds only the two things generation
 * cannot supply — the envelope peel and the query-key namespace.
 *
 * ## The envelope
 *
 * `eliteaFetch` returns `{data, status, headers}`, not the body. Typing a call
 * as the body and reading `.rows` off it yields `undefined` on a perfectly good
 * 200 — the #132 shape, which has appeared twice on the same endpoint. Every
 * read here goes through `unwrapListPage`, which accepts the envelope OR the
 * body and reports an unrecognised shape rather than answering "nothing is
 * running".
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import {
  cancelBackgroundJob,
  listBackgroundJobs,
} from '@/shared/api/generated/admin/admin';
import { unwrapListPage } from '@/shared/api/unwrap';

/** The kinds the server serves and filters on. */
export const TASK_KINDS = ['index', 'agent', 'toolkit', 'execution', 'schedule', 'eval'] as const;
type TaskKind = (typeof TASK_KINDS)[number];

/** One row of the Tasks table, in the server's own key names. */
export interface AdminTaskRow {
  readonly task_id: string;
  readonly kind: TaskKind;
  readonly name: string;
  readonly status: string;
  readonly started_at: string | null;
  readonly finished_at: string | null;
  readonly project_id: number | null;
  readonly user: string;
  readonly cancellable: boolean;
}

export interface AdminTasksQuery {
  readonly kind?: string;
  readonly status?: string;
  readonly limit: number;
  readonly offset: number;
}

export interface AdminTasksPage {
  readonly rows: readonly AdminTaskRow[];
  readonly total: number;
  /** The server's scan window was full, so older rows exist off this page. */
  readonly truncated: boolean;
}

const adminTasksQueryKey = (query: AdminTasksQuery): readonly unknown[] =>
  ['admin', 'background-jobs', query.kind ?? '', query.status ?? '', query.limit, query.offset];

/**
 * The listing, refetched on an interval.
 *
 * `refetchInterval` is a caller argument rather than a constant here so a test
 * can disable the poll: a 10-second timer inside a jsdom test either makes the
 * test slow or makes it flaky, and neither proves anything the explicit refetch
 * does not.
 */
export function useAdminTasks(query: AdminTasksQuery, refetchInterval: number | false) {
  return useQuery({
    queryKey: adminTasksQueryKey(query),
    queryFn: async (): Promise<AdminTasksPage> => {
      const response = await listBackgroundJobs({
        ...(query.kind === undefined || query.kind === '' ? {} : { kind: query.kind as TaskKind }),
        ...(query.status === undefined || query.status === '' ? {} : { status: query.status }),
        limit: query.limit,
        offset: query.offset,
      });
      const { rows, total } = unwrapListPage<AdminTaskRow>(response, 'listBackgroundJobs');
      // `truncated` lives beside `rows` in the body, which unwrapListPage does
      // not carry, so it is read from whichever level holds it. Absent reads as
      // false: an older server that does not send it is not a truncated one.
      const body = (response as { data?: unknown }).data ?? response;
      const truncated = typeof body === 'object' && body !== null
        ? (body as { truncated?: unknown }).truncated === true
        : false;
      return { rows, total, truncated };
    },
    refetchInterval,
  });
}

export interface CancelTaskInput {
  readonly kind: TaskKind;
  readonly taskId: string;
  /** Required for `eval`, ignored otherwise — see the Go handler. */
  readonly projectId: number | null;
}

/**
 * The cancel. It invalidates the whole listing namespace rather than one page:
 * a cancelled job changes its own status AND its `cancellable` flag, and the
 * operator may be on any page when they press it.
 */
export function useCancelAdminTask() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: CancelTaskInput) => {
      await cancelBackgroundJob(
        input.kind,
        input.taskId,
        input.kind === 'eval' && input.projectId !== null
          ? { project_id: input.projectId }
          : undefined,
      );
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['admin', 'background-jobs'] }),
  });
}
