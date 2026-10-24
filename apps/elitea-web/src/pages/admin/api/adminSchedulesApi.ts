/**
 * REST client for the admin SCHEDULES surface (issue #1369).
 *
 * The page lists the recurring routines the platform really runs - elitea-main's
 * scheduled jobs and elitea-scheduler's workers - and lets an operator switch
 * one off or change its schedule. Both take effect without a restart.
 *
 * Handwritten in the same shape as `./adminProjectsApi.ts`; the routes are
 * described in `services/elitea-main/api/openapi/v2.yaml` (`listPlatformRoutines`,
 * `updatePlatformRoutine`) and served by
 * `services/elitea-main/internal/api/v2/admin/platform_routines.go`.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { EliteaApiError, eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapListPage } from '@/shared/api/unwrap';

const ROUTINES_URL = '/admin/platform_routines/administration';

/** One row of `GET /admin/platform_routines/administration`. */
export interface AdminRoutineRow {
  readonly id: string;
  /** `main` runs on elitea-main's scheduler, `scheduler` in elitea-scheduler. */
  readonly owner: string;
  readonly description: string;
  /** What the code or chart supplies; in force until an operator overrides it. */
  readonly default_schedule: string;
  readonly schedule_override: string | null;
  /** The schedule in force now: the override when there is one. */
  readonly schedule: string;
  /** The shortest gap an override may set, in seconds; 0 means no bound. */
  readonly min_interval_seconds: number;
  readonly enabled: boolean;
  readonly last_run_at: string | null;
  /** `ok`, `error` or `maintenance`. */
  readonly last_outcome: string | null;
  readonly next_run_at: string | null;
  readonly updated_by: string | null;
  readonly updated_at: string;
}

/**
 * One query-key namespace for this page. The mutation invalidates
 * `adminSchedulesKeys.all`, so a key built ad hoc at a call site would be a cache
 * the write never refreshes.
 */
const adminSchedulesKeys = {
  all: ['admin', 'schedules'] as const,
  list: () => ['admin', 'schedules', 'list'] as const,
};

/**
 * The server's own explanation of a refusal, when it gave one. A schedule write
 * is refused for reasons the operator can act on - an unparseable expression, a
 * schedule faster than the routine allows, a concurrent edit - and "Failed to
 * save" over all of them would hide the only sentence that says which.
 *
 * A 401 never arrives here (the shared client routes it into re-auth and it
 * carries no body); a 403 does, and keeps its body.
 */
export function scheduleFailureReason(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  if (failure.kind !== 'http') return undefined;
  const body = failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const reason = (body as { error?: unknown }).error;
  return typeof reason === 'string' && reason !== '' ? reason : undefined;
}

/** `GET /admin/platform_routines/administration` - every routine, unpaginated. */
export function useAdminRoutines(): UseQueryResult<AdminRoutineRow[], Error> {
  return useQuery({
    queryKey: adminSchedulesKeys.list(),
    queryFn: async (): Promise<AdminRoutineRow[]> =>
      unwrapListPage<AdminRoutineRow>(await eliteaFetch<unknown>(ROUTINES_URL), 'adminRoutines').rows,
  });
}

/**
 * The fields a client may change. Only the changed field is sent: the server
 * leaves the rest alone, so toggling the switch cannot rewrite a schedule the
 * operator is mid-edit on. An empty `schedule` removes the override and puts
 * the default back.
 */
export interface AdminRoutineUpdate {
  readonly id: string;
  readonly enabled?: boolean;
  readonly schedule?: string;
}

/** `PUT /admin/platform_routines/administration/{id}`. */
export function useUpdateAdminRoutine(): UseMutationResult<void, Error, AdminRoutineUpdate> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ id, ...body }: AdminRoutineUpdate) => {
      await eliteaFetch<unknown>(`${ROUTINES_URL}/${encodeURIComponent(id)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminSchedulesKeys.all }),
  });
}
