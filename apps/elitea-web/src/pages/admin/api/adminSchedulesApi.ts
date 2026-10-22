/**
 * REST client for the admin SCHEDULES surface — unit A14, issue #200.
 *
 * One read and one write, both real. The reference page (`admin_ui`'s
 * `SchedulesTasksPage`) has five reads and five mutations; the other four of
 * each belong to its Tasks and Active Tasks tabs, which have no endpoint here
 * and are not ported — see `../SchedulesTasks.tsx`.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as `./adminProjectsApi.ts`.
 *
 * The wire contract mirrors the Go handler in
 * `services/elitea-main/internal/api/v2/scheduling/schedules.go`, which in turn
 * mirrors the legacy original (`legacy/plugins/scheduling/api/v2/schedules.py`)
 * the existing admin_ui client already speaks to — same path, same body keys,
 * same row fields. The trailing `0` in the URL is the legacy `projectID`
 * segment; administration mode ignores it and every client sends `0`.
 *
 * ## What is reused
 *
 * Nothing textual from the four pages before this one — a different endpoint,
 * a different row shape, its own query-key namespace. The schedule HISTORY
 * drawer is a different story: it runs entirely on `./adminAuditApi`'s
 * `useAuditSpans`, unchanged, because a schedule's execution history IS audit
 * events of type `schedule` (`SYSTEM_EVENT_TYPES` there already names it).
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

/** Only `administration` lists every schedule, server-side as in the legacy API. */
const ADMIN_MODE = 'administration';

/**
 * The legacy URL carries a project segment even in administration mode, where
 * the handler ignores it. `0` is what every existing client sends.
 */
const SCHEDULES_URL = `/scheduling/schedules/${ADMIN_MODE}/0`;

/**
 * One row of `GET /scheduling/schedules/administration/0` — one `centry.schedule`
 * row.
 *
 * Every field is a REAL column of that table, checked against the SQLAlchemy
 * model that owns the write path in the hybrid deployment
 * (`legacy/plugins/scheduling/models/schedule.py`) and against the DDL this unit
 * added to `001_initial.sql`. Nothing below is a constant dressed as data — the
 * handler this replaced hardcoded `enabled: true`, so its "active" column could
 * only ever render one value.
 */
export interface AdminScheduleRow {
  readonly id: number;
  readonly name: string;
  /** `null` for a PLATFORM schedule — most of them. */
  readonly project_id: number | null;
  readonly cron: string;
  readonly active: boolean;
  /**
   * The internal platform RPC this schedule invokes. Read-only by design: see
   * `useUpdateAdminSchedule` below and the Go handler's header.
   */
  readonly rpc_func: string;
  readonly rpc_kwargs: Readonly<Record<string, unknown>>;
  /** ISO timestamp, or `null` when the schedule has never run. */
  readonly last_run: string | null;
}

/**
 * One query-key namespace for this page, declared once.
 *
 * The mutation invalidates `adminSchedulesKeys.all`, so a key built ad hoc at a
 * call site would be a cache the write never refreshes — the read/write
 * key-namespace split that made saved data look absent in #132.
 */
const adminSchedulesKeys = {
  all: ['admin', 'schedules'] as const,
  list: () => ['admin', 'schedules', 'list'] as const,
};

/**
 * The server's own explanation of a refusal, when it gave one.
 *
 * It matters more here than on a listing page: this write is refused for
 * several DIFFERENT reasons the operator can act on — an unparseable cron, a
 * name longer than the column, an attempt to change what the schedule runs —
 * and rendering "Failed to save" over all of them would hide the only sentence
 * that says which.
 *
 * A 401 does NOT arrive here, and that is the shared client's decision, not an
 * oversight: `shared/api/http.ts` routes it into the single-flight re-auth path
 * and reports `kind: 'auth'`, which carries no body. Those render the generic
 * notice. Only `kind: 'http'` — 4xx and 5xx that are not an auth challenge —
 * can explain itself, and since issue 93 that INCLUDES a 403: an authorization
 * refusal is not a session failure, so it keeps its body.
 *
 * The `kind !== 'http'` line below is TYPE narrowing and documentation, not a
 * behavioural guard: no other failure variant has a `body` field, so deleting it
 * changes nothing observable. Mutation testing confirms that (mutant M15
 * survives, and is equivalent). It stays because removing it would make the
 * `failure.body` access a type error, and because the next reader needs to know
 * that an auth refusal is deliberately unexplained here.
 */
export function scheduleFailureReason(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  // `auth` failures carry no body — a 401 is handled by the re-auth path — so
  // only `http` can explain itself. A 403 is `http` (issue 93).
  if (failure.kind !== 'http') return undefined;
  const body = failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const reason = (body as { error?: unknown }).error;
  return typeof reason === 'string' && reason !== '' ? reason : undefined;
}

/** `GET /scheduling/schedules/administration/0` — the whole table, unpaginated. */
export function useAdminSchedules(): UseQueryResult<AdminScheduleRow[], Error> {
  return useQuery({
    queryKey: adminSchedulesKeys.list(),
    queryFn: async (): Promise<AdminScheduleRow[]> =>
      unwrapListPage<AdminScheduleRow>(await eliteaFetch<unknown>(SCHEDULES_URL), 'adminSchedules').rows,
  });
}

/**
 * The fields a client may change.
 *
 * `rpc_func` and `rpc_kwargs` are absent DELIBERATELY, and their absence is
 * enforced by the server, not by this type. A scheduled run has no interactive
 * principal: the scheduler publishes `rpc_func` onto the Arbiter bus
 * fire-and-forget, and the handler on the other end is an internal platform
 * function with full privilege. A client able to set them could name any
 * internal RPC, with any arguments, and have the platform invoke it unattended
 * a minute later. The Go handler answers 400 to a body carrying either.
 */
export interface AdminScheduleUpdate {
  readonly id: number;
  readonly name?: string;
  readonly cron?: string;
  readonly active?: boolean;
}

/**
 * `PUT /scheduling/schedules/administration/0` — the id travels in the BODY,
 * which is the legacy shape.
 *
 * Only the changed field is sent. The server applies exactly what it is given
 * and leaves the rest alone, so toggling the switch cannot silently rewrite a
 * cron the operator is mid-edit on.
 */
export function useUpdateAdminSchedule(): UseMutationResult<void, Error, AdminScheduleUpdate> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (update: AdminScheduleUpdate) => {
      await eliteaFetch<unknown>(SCHEDULES_URL, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(update),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminSchedulesKeys.all }),
  });
}
