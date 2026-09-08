/**
 * The admin CROSS-PROJECT bulk membership invite (issue 247).
 *
 * `POST /admin/invites_bulk/administration` — every listed account into every
 * listed project with one role. It replaces two pylon console pages,
 * `invites_bulkusers.py` ("every user into one project") and
 * `invites_bulkprojects.py` ("one user into many projects"), which are the same
 * cross product with one side pinned.
 *
 * GENERATED, unlike the rest of `pages/admin/api/**`. Those modules are
 * handwritten because `v2.yaml` never described the admin-panel routes; this
 * operation is described (`bulkInviteMembers`), so orval builds the caller and
 * the request/response types, and a shape drift between the Go handler and this
 * client becomes a codegen diff rather than a runtime surprise.
 *
 * The transport envelope is peeled through `unwrapBody`, the one sanctioned
 * path (R-A6, issue 132): `eliteaFetch` resolves `{data, status, headers}`, and
 * a call site that read the summary straight off the result would find every
 * field `undefined` on a perfectly good 200.
 *
 * There is NO cache invalidation here, and that is deliberate. This write
 * changes project MEMBERSHIP, which no query in the admin Users page's
 * namespace holds — the page lists `auth_core__user` rows, and a membership
 * write does not change any of them. The projects page's `admin_names` column
 * can change, so `adminProjectsKeys.all` is invalidated and nothing else.
 */
import { useMutation, useQueryClient, type UseMutationResult } from '@tanstack/react-query';

import { bulkInviteMembers } from '@/shared/api/generated/admin/admin';
import { unwrapBody } from '@/shared/api/unwrap';

import { adminProjectsKeys } from './adminProjectsApi';

/**
 * The eight outcomes the server reports per pair
 * (`internal/api/v2/admin/invites_bulk.go`). Each is a different thing for an
 * operator to do next, which is why the server does not collapse them into one
 * "error" the way pylon's log blob did.
 */
export type BulkInviteOutcome =
  | 'added'
  | 'already_member'
  | 'unknown_user'
  | 'unknown_project'
  | 'unknown_role'
  | 'system_user'
  | 'personal_project'
  | 'failed';

/** One (user, project) pair's result row. */
export interface BulkInviteResultRow {
  readonly user_id: number;
  readonly user_email: string;
  readonly project_id: number;
  readonly project_name: string;
  readonly status: 'ok' | 'error';
  readonly outcome: BulkInviteOutcome;
  readonly msg: string;
}

/** The whole batch's report. */
export interface BulkInviteReport {
  readonly ok: boolean;
  readonly role: string;
  readonly requested: number;
  readonly added: number;
  readonly skipped: number;
  readonly failed: number;
  readonly results: readonly BulkInviteResultRow[];
}

export interface BulkInviteInput {
  readonly users: readonly number[];
  readonly projects: readonly number[];
  readonly role: string;
}

/**
 * A response that is not a report is not silently rendered as an empty batch.
 * `results: []` with `requested: 0` reads as "nothing was asked for", which is
 * the one thing this call can never mean.
 */
function readReport(response: unknown): BulkInviteReport {
  const body = unwrapBody(response);
  if (typeof body !== 'object' || body === null || !Array.isArray((body as BulkInviteReport).results)) {
    throw new TypeError('the bulk invite answered a body with no per-pair results');
  }
  return body as BulkInviteReport;
}

/**
 * `POST /admin/invites_bulk/administration`.
 *
 * The server answers 200 even when some pairs failed — the batch is a report,
 * and a 400 would tell this client that nothing landed when something did. So
 * a resolved mutation is NOT "everything worked": the caller must read
 * `report.ok` and `report.failed`. That is exactly the trap
 * `ProjectMemberDialog`'s doc comment records the reference client falling into
 * from the other direction.
 */
export function useBulkInviteMembers(): UseMutationResult<BulkInviteReport, Error, BulkInviteInput> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: BulkInviteInput): Promise<BulkInviteReport> =>
      readReport(
        await bulkInviteMembers({
          users: [...input.users],
          projects: [...input.projects],
          role: input.role,
        }),
      ),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminProjectsKeys.all }),
  });
}
