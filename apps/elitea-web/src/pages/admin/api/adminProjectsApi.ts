/**
 * REST client for the admin PROJECTS surface — unit A14, issue #200.
 *
 * Two reads and three writes, all of which reach a server. Project CREATE and
 * DELETE are two more writes, and they live next door in
 * `./adminProjectProvisioningApi.ts` — a different endpoint family with a
 * different response shape, split out when this module outgrew the file-length
 * budget. Both invalidate `adminProjectsKeys.all`, which is exported for them
 * and for that reason only: a second key namespace would be a cache the
 * provisioning writes never refresh (#132).
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as `./adminUsersApi.ts` and
 * `./adminAuditApi.ts`.
 *
 * The wire contract mirrors the Go handlers in
 * `services/elitea-main/internal/api/v2/admin/projects.go` and
 * `internal/api/v2/eliteacore/users_write.go`, which in turn mirror the pylon
 * originals (`legacy/plugins/admin/api/v2/projects.py`, `project_suspend.py`,
 * `users.py`) the existing admin_ui client already speaks to — same paths, same
 * query parameters, same body keys.
 *
 * ## What is reused from the two pages before this one
 *
 * Nothing in this module. A different endpoint family, different row shapes,
 * different query-key namespace. The only shared code is what all three import:
 * `eliteaFetch` and `@/shared/api/unwrap` (R-A6, issue #132 — never a
 * hand-rolled `.data.data`). The page ABOVE it is a different story: the
 * activity drawer runs entirely on `./adminAuditApi`, which the Audit Trail
 * port left behind.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody, unwrapListPage } from '@/shared/api/unwrap';

/** Only `administration` has a handler, server-side and in pylon before it. */
export const ADMIN_MODE = 'administration';

/** The page's two tabs. The server filters on the same two words. */
export type ProjectType = 'team' | 'personal';

/**
 * The four statuses `projects.py` derives from `suspended` / `create_success`.
 *
 * pylon has a fifth, `pending`, for `create_success IS NULL`. That column is
 * NOT NULL in this schema, so the server never emits it; the type keeps it out
 * rather than carrying a value nothing can produce.
 */
export type ProjectStatus = 'active' | 'suspended' | 'failed';

/**
 * One row of `GET /admin/projects/administration`.
 *
 * Every field is one the server actually sends — checked against
 * `internal/api/v2/admin/projects.go` and against `centry.project`'s columns in
 * a live database. That check is not ceremony: the admin Users reference page
 * read a `status` column that has never existed, so its chip could only ever
 * render one value and its suspend toggle only ever computed one direction.
 *
 * `project_name` and `admin_name` are pylon-era aliases the server still emits
 * for the old SPA; this client reads `name` and `owner_name` and does not
 * declare them.
 */
export interface AdminProjectRow {
  readonly id: number;
  readonly name: string;
  readonly owner_id: number;
  /** The owner's display name, falling back to their email. `''` if unknown. */
  readonly owner_name: string;
  /** Project admins OTHER than the owner. Empty, never null. */
  readonly admin_names: readonly string[];
  readonly status: ProjectStatus;
  readonly suspended: boolean;
  readonly create_success: boolean;
  /** True when the name matches pylon's `project_user_%` personal-project rule. */
  readonly is_personal: boolean;
  /**
   * Bytes the project's cloud copies of desktop Local work threads use (the
   * live workspace segments, ADR-0034 decision 3). Apart from artifact usage.
   * Optional only for a server older than this field; the column shows `—`.
   */
  readonly workspace_bytes?: number;
}

export interface AdminProjectsPage {
  readonly rows: AdminProjectRow[];
  /** The count of the FILTERED set — what the pagination controls page over. */
  readonly total: number;
  /** Tab counts over ALL projects — deliberately NOT narrowed by the filters. */
  readonly counts: { readonly team: number; readonly personal: number };
}

export interface AdminProjectsQueryParams {
  readonly limit: number;
  readonly offset: number;
  readonly search?: string | undefined;
  readonly projectType?: ProjectType | undefined;
  readonly sortBy?: string | undefined;
  readonly sortOrder?: 'asc' | 'desc' | undefined;
}

/**
 * One query-key namespace for this page's data, declared once.
 *
 * Every mutation below invalidates `adminProjectsKeys.all`, so a key built ad
 * hoc at a call site would be a cache the writes never refresh — the read/write
 * key-namespace split that made saved data look absent in #132. The member
 * queries live under the same root for the same reason: adding an admin changes
 * BOTH the member list and the `admin_names` column of the listing.
 */
export const adminProjectsKeys = {
  all: ['admin', 'projects'] as const,
  list: (params: AdminProjectsQueryParams) => ['admin', 'projects', 'list', params] as const,
  members: (projectId: number) => ['admin', 'projects', 'members', projectId] as const,
  roles: (projectId: number) => ['admin', 'projects', 'roles', projectId] as const,
  activity: (projectId: number, dateFrom: string, dateTo: string) =>
    ['admin', 'projects', 'activity', projectId, dateFrom, dateTo] as const,
};

function buildListUrl(params: AdminProjectsQueryParams): string {
  const query = new URLSearchParams({
    limit: String(params.limit),
    offset: String(params.offset),
  });
  if (params.search) query.set('search', params.search);
  if (params.projectType) query.set('project_type', params.projectType);
  if (params.sortBy) query.set('sort_by', params.sortBy);
  if (params.sortOrder) query.set('sort_order', params.sortOrder);
  return `/admin/projects/${ADMIN_MODE}?${query.toString()}`;
}

/**
 * `counts` has no home in `unwrapListPage`'s `{rows,total}` result. The
 * transport peel comes from `unwrapBody` (R-A6's sanctioned module) rather than
 * a hand-rolled `resp.data` descent; only the per-field validation is local.
 *
 * A missing or malformed `counts` degrades to zeroes, which renders as tab
 * labels without a count — never as a wrong count.
 */
function readCounts(response: unknown): AdminProjectsPage['counts'] {
  const fallback = { team: 0, personal: 0 };
  const body = unwrapBody(response);
  if (typeof body !== 'object' || body === null) return fallback;
  const counts = (body as { counts?: unknown }).counts;
  if (typeof counts !== 'object' || counts === null) return fallback;
  const { team, personal } = counts as { team?: unknown; personal?: unknown };
  return {
    team: typeof team === 'number' ? team : fallback.team,
    personal: typeof personal === 'number' ? personal : fallback.personal,
  };
}

/**
 * One page of `GET /admin/projects/administration`, without a hook.
 *
 * Exported because the export control (see `useAdminProjectsPage`) has to walk
 * EVERY page imperatively on click; going through `useQuery` there would mean
 * either a second always-mounted query for data nobody renders, or a
 * `limit: total` request whose size no server bound checks.
 */
export async function fetchAdminProjectsPage(
  params: AdminProjectsQueryParams,
): Promise<AdminProjectsPage> {
  const response = await eliteaFetch<unknown>(buildListUrl(params));
  const { rows, total } = unwrapListPage<AdminProjectRow>(response, 'adminProjects');
  return { rows, total, counts: readCounts(response) };
}

/** `GET /admin/projects/administration` — one page of the project list. */
export function useAdminProjects(
  params: AdminProjectsQueryParams,
): UseQueryResult<AdminProjectsPage, Error> {
  return useQuery({
    queryKey: adminProjectsKeys.list(params),
    queryFn: () => fetchAdminProjectsPage(params),
  });
}

/**
 * `PUT /admin/project_suspend/administration/{id}` — toggles the flag.
 *
 * The handler behind this existed in elitea-main before unit A14 but was
 * mounted on no route at all, so this control had nothing to reach.
 */
export function useSuspendAdminProject(): UseMutationResult<
  void,
  Error,
  { projectId: number; suspended: boolean }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ projectId, suspended }) => {
      await eliteaFetch<unknown>(`/admin/project_suspend/${ADMIN_MODE}/${projectId}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ suspended }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminProjectsKeys.all }),
  });
}

/* ── members ───────────────────────────────────────────────────────────── */

/** The project roles the member dialog offers. Resolved from the server, not hardcoded. */
export interface ProjectRole {
  readonly id: string;
  readonly name: string;
}

/**
 * One row of `GET /admin/users/administration/{projectID}`.
 *
 * `id` is a STRING here — `internal/api/v2/eliteacore/handler.go`'s member
 * listing formats it with `%d` into a string, unlike the projects listing's
 * numeric `id`. Declared as it arrives rather than coerced at the boundary, so
 * a comparison against a number cannot silently fail.
 */
export interface ProjectMemberRow {
  readonly id: string;
  readonly email: string;
  readonly name: string;
  readonly roles: readonly string[];
}

/**
 * `GET /admin/users/administration/{projectID}` — this project's members.
 *
 * `projectId` is a plain `number`, not `number | null`. Every caller mounts
 * only once a project is chosen (the dialog and the drawer both render their
 * content behind a `project !== null` check), so a nullable parameter here
 * would be a branch nothing can reach — and an unreachable guard is
 * indistinguishable from a working one until it is needed. Confirmed by
 * mutation: flipping the `enabled` gate to `true` changed no test's outcome.
 */
export function useProjectMembers(projectId: number): UseQueryResult<ProjectMemberRow[], Error> {
  return useQuery({
    queryKey: adminProjectsKeys.members(projectId),
    queryFn: async (): Promise<ProjectMemberRow[]> =>
      unwrapListPage<ProjectMemberRow>(
        await eliteaFetch<unknown>(`/admin/users/${ADMIN_MODE}/${projectId}?limit=200&offset=0`),
        'adminProjectMembers',
      ).rows,
  });
}

/**
 * `GET /admin/roles/administration/{projectID}` — the roles THIS project defines.
 *
 * The reference dialog hardcodes `['admin','editor','viewer']`. The server
 * rejects a role the project does not define (`resolveProjectRoleIDs` returns
 * it as unknown and the write is a 400), so a hardcoded list is a dialog that
 * can offer an option guaranteed to fail. This asks.
 *
 * The endpoint answers a PLAIN ARRAY, not a `{rows,total}` envelope — see the
 * `// UI roleList query expects a plain array` note on the Go handler. That is
 * one of the three shapes `unwrapListPage` was written for (R-A6, #132), so it
 * is still the peel; sniffing for the array here is what the rule forbids.
 */
export function useProjectRoles(projectId: number): UseQueryResult<ProjectRole[], Error> {
  return useQuery({
    queryKey: adminProjectsKeys.roles(projectId),
    queryFn: async (): Promise<ProjectRole[]> =>
      unwrapListPage<ProjectRole>(
        await eliteaFetch<unknown>(`/admin/roles/${ADMIN_MODE}/${projectId}`),
        'adminProjectRoles',
      ).rows,
  });
}

/**
 * `POST /admin/users/administration/{projectID}` — invite an address into the
 * project with the given role.
 *
 * The server answers 400 with a per-address result array when any address
 * failed, which `eliteaFetch` surfaces as a rejection; the caller reports it.
 * The reference client instead inspected `result[0].status === 'error'` on a
 * RESOLVED promise and left the success path's cache invalidation running, so a
 * rejected invite still refreshed the table as though something had changed.
 */
export function useAddProjectMember(): UseMutationResult<
  void,
  Error,
  { projectId: number; email: string; role: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ projectId, email, role }) => {
      await eliteaFetch<unknown>(`/admin/users/${ADMIN_MODE}/${projectId}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ emails: [email], roles: [role] }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminProjectsKeys.all }),
  });
}

/**
 * `PUT /admin/users/administration/{projectID}` — REPLACE an existing member's
 * roles. Replacement, not merge: the server deletes the member's rows for the
 * project before inserting the new set.
 */
export function useUpdateProjectMemberRole(): UseMutationResult<
  void,
  Error,
  { projectId: number; userId: string; role: string }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ projectId, userId, role }) => {
      await eliteaFetch<unknown>(`/admin/users/${ADMIN_MODE}/${projectId}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: userId, roles: [role] }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminProjectsKeys.all }),
  });
}

/* ── activity ──────────────────────────────────────────────────────────── */

/** One member's event count over the selected window. */
export interface ProjectUserActivityRow {
  readonly user_id: number;
  readonly user_email: string | null;
  readonly event_count: number;
}

/**
 * `GET /elitea_core/project_user_activity/administration` — per-user event
 * counts for one project.
 *
 * Before unit A14 this was one of elitea-main's `_ *http.Request` stubs: 200
 * with an empty array, the request discarded. The activity squares it feeds
 * therefore rendered uniformly inactive — a chart that was a constant.
 */
export function useProjectUserActivity(
  projectId: number,
  dateFrom: string,
  dateTo: string,
): UseQueryResult<ProjectUserActivityRow[], Error> {
  return useQuery({
    queryKey: adminProjectsKeys.activity(projectId, dateFrom, dateTo),
    queryFn: async (): Promise<ProjectUserActivityRow[]> => {
      const query = new URLSearchParams({ project_id: String(projectId) });
      if (dateFrom) query.set('date_from', dateFrom);
      if (dateTo) query.set('date_to', dateTo);
      return unwrapListPage<ProjectUserActivityRow>(
        await eliteaFetch<unknown>(`/elitea_core/project_user_activity/${ADMIN_MODE}?${query}`),
        'adminProjectUserActivity',
      ).rows;
    },
  });
}
