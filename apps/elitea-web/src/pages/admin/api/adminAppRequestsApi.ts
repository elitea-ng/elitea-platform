/**
 * REST client for the admin APP REQUESTS surface — unit A14, issue #200.
 *
 * One paginated read and one write, both real as of this unit. Before it the
 * read was a `_ *http.Request` stub returning a fixed empty page and the write
 * had no route at all, so the queue was permanently empty and its approve and
 * reject buttons had nothing behind them.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as `./adminProjectsApi.ts`.
 *
 * The wire contract mirrors the Go handler in
 * `services/elitea-main/internal/api/v2/moderation/requests.go`, which in turn
 * mirrors the pylon originals (`legacy/plugins/admin/api/v2/moderation_statuses.py`
 * and `moderation_status.py`) the existing admin_ui client already speaks to —
 * same paths, same query parameters, same body keys, same row fields.
 *
 * ## What is reused from the six pages before this one
 *
 * Nothing textual. A different endpoint family, a different row shape, its own
 * query-key namespace. The only shared code is what every admin page imports:
 * `eliteaFetch` and `@/shared/api/unwrap` (R-A6, issue #132 — never a
 * hand-rolled `.data.data`). `failureReason` below is the same IDEA as
 * `./adminSchedulesApi`'s `scheduleFailureReason` and is deliberately a second
 * copy rather than a shared helper: see its own comment.
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

/** Only `administration` has a handler, server-side and in pylon before it. */
const ADMIN_MODE = 'administration';

const QUEUE_URL = `/admin/moderation_statuses/${ADMIN_MODE}`;
/** The decision endpoint takes the row id in the BODY. pylon's shape. */
const DECISION_URL = `/admin/moderation_status/${ADMIN_MODE}`;

/** The three values `status` can hold. The server refuses anything else. */
export type AppRequestStatus = 'pending' | 'approved' | 'rejected';

/**
 * One row of `GET /admin/moderation_statuses/administration` — one
 * `centry.moderation_state` row.
 *
 * Every field is a REAL column of that table except `user_email`, which the
 * server resolves by joining `auth_core__user`: pylon attaches it the same way,
 * and the queue's "Requesting User" column is the only place it is read.
 */
export interface AppRequestRow {
  readonly id: number;
  readonly user_id: number;
  /** `''` when the requester's account has since been deleted. */
  readonly user_email: string;
  readonly project_id: number;
  /** The label the requesting client displayed for the catalogue entry. */
  readonly issue_type: string;
  /** The catalogue entry itself — an opaque key, not a foreign key. */
  readonly entity_id: string;
  /** The requester's own justification. */
  readonly description: string;
  readonly status: AppRequestStatus;
  /** Set only on a rejection, and required there. */
  readonly rejection_comment: string | null;
  readonly created_at: string;
  readonly updated_at: string;
  /**
   * Set ONLY on an approved `issue_type: "Project Request"` row (#871) — the
   * id the project-creation pipeline assigned when this request was
   * approved. Absent on every other row, and absent on a Project Request
   * that is still pending or was rejected
   * (internal/api/v2/moderation/project_requests.go's `decideProjectRequest`).
   */
  readonly created_project_id?: number;
}

export interface AppRequestsPage {
  readonly rows: readonly AppRequestRow[];
  /** The count of the FILTERED set — what the pagination controls page over. */
  readonly total: number;
}

export interface AppRequestsQueryParams {
  readonly limit: number;
  readonly offset: number;
  readonly search?: string | undefined;
  readonly status?: AppRequestStatus | undefined;
  /**
   * `issue_type` is the label the requesting client showed for the catalogue
   * entry — free text, not an enum the server declares anywhere (unlike
   * `status`). The Go handler has always accepted this filter
   * (`internal/api/v2/moderation/requests.go`'s `queueFilters`); nothing on
   * the client sent it until the issue-type filter control did.
   */
  readonly issueType?: string | undefined;
  readonly sortBy?: string | undefined;
  readonly sortOrder?: 'asc' | 'desc' | undefined;
}

/**
 * One query-key namespace for this page, declared once.
 *
 * The decision invalidates `adminAppRequestsKeys.all`, so a key built ad hoc at
 * a call site would be a cache the write never refreshes — the read/write
 * key-namespace split that made saved data look absent in #132.
 */
const adminAppRequestsKeys = {
  all: ['admin', 'appRequests'] as const,
  list: (params: AppRequestsQueryParams) => ['admin', 'appRequests', 'list', params] as const,
};

/**
 * The server's own explanation of a refusal, when it gave one.
 *
 * It earns its place on this page: the decision is refused for several DIFFERENT
 * reasons an operator can act on — a rejection with no reason, a body carrying a
 * field the moderator may not write, a request that has since been deleted — and
 * rendering "Failed to save" over all of them would hide the only sentence that
 * says which.
 *
 * A 401 does NOT arrive here (a 403 does, since issue 93), and that is the
 * shared client's decision:
 * `shared/api/http.ts` routes both into the single-flight re-auth path and
 * reports `kind: 'auth'`, which carries no body.
 *
 * Duplicated from `./adminSchedulesApi`'s `scheduleFailureReason` rather than
 * hoisted into a shared module. The two are the same six lines today, but they
 * are assertions about two different servers' error envelopes; a shared helper
 * would make a change to one page's contract silently a change to the other's.
 */
export function failureReason(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  if (failure.kind !== 'http') return undefined;
  const body = failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const reason = (body as { error?: unknown }).error;
  return typeof reason === 'string' && reason !== '' ? reason : undefined;
}

function buildQueueUrl(params: AppRequestsQueryParams): string {
  const query = new URLSearchParams({
    limit: String(params.limit),
    offset: String(params.offset),
  });
  if (params.search) query.set('search', params.search);
  if (params.status) query.set('status', params.status);
  if (params.issueType) query.set('issue_type', params.issueType);
  if (params.sortBy) query.set('sort_by', params.sortBy);
  if (params.sortOrder) query.set('sort_order', params.sortOrder);
  return `${QUEUE_URL}?${query.toString()}`;
}

/** `GET /admin/moderation_statuses/administration` — one page of the queue. */
export function useAdminAppRequests(
  params: AppRequestsQueryParams,
): UseQueryResult<AppRequestsPage, Error> {
  return useQuery({
    queryKey: adminAppRequestsKeys.list(params),
    queryFn: async (): Promise<AppRequestsPage> =>
      unwrapListPage<AppRequestRow>(
        await eliteaFetch<unknown>(buildQueueUrl(params)),
        'adminAppRequests',
      ),
  });
}

/**
 * The fields a moderator may change.
 *
 * `entity_id`, `issue_type`, `description`, `user_id`, `project_id` and `meta`
 * are absent DELIBERATELY, and their absence is enforced by the server, not by
 * this type. The record of what was ASKED must not be editable by the person
 * answering it, or an approved row stops being evidence of what was approved;
 * `meta` is refused because the endpoint replaces rather than merges it, so a
 * decision would destroy whatever the requester stored. The Go handler answers
 * 400 to a body carrying any of them.
 *
 * `status` is narrowed to the two DECISIONS. pylon accepts `pending` here too,
 * which silently reopens a decided request with no record that it was ever
 * answered; the server refuses it.
 */
export interface AppRequestDecision {
  readonly id: number;
  readonly status: 'approved' | 'rejected';
  /** Required when rejecting, and refused on an approval. */
  readonly rejection_comment?: string;
}

/** `PUT /admin/moderation_status/administration` — approve or reject one row. */
export function useDecideAppRequest(): UseMutationResult<void, Error, AppRequestDecision> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (decision: AppRequestDecision) => {
      await eliteaFetch<unknown>(DECISION_URL, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(decision),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminAppRequestsKeys.all }),
  });
}
