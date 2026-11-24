/**
 * REST client for the GLOBAL (admin-panel) users surface — unit A14.
 *
 * This is the admin twin of `pages/settings/Users.tsx`'s project-scoped
 * membership API, and it is a DIFFERENT backend surface: `/admin/users/…`
 * lists the members of one project, `/admin/auth_users/administration` lists
 * every `auth_core__user` row in the deployment. They share no endpoint, no
 * row shape and no query key.
 *
 * GENERATED underneath since ADR-0059 R0.8b (issue 2574): `v2.yaml` now
 * describes `GET` and `POST /admin/auth_users/{mode}` (`listAdminAuthUsers`,
 * `adminAuthUsersAction`), so the listing and both writes call the orval
 * functions, and the web mocks are the contract's examples. This module keeps
 * its exported names, so the page compiles unchanged, and holds what
 * generation cannot supply: the envelope peel, the `counts` read and the
 * query-key namespace. The suspend route is not in the contract yet and stays
 * hand-written.
 *
 * The wire contract mirrors the legacy runtime
 * (`legacy/plugins/admin/api/v2/auth_users.py` and `user_suspend.py`) and its
 * existing client (`frontends/admin_ui/frontend/src/api/usersApi.js`) exactly —
 * same paths, same `action` discriminator, same body keys — because #137 showed
 * what inventing a shape costs.
 */
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from '@tanstack/react-query';

import {
  adminAuthUsersAction,
  listAdminAuthUsers,
} from '@/shared/api/generated/admin/admin';
import type { AdminAuthUsersActionRequest, ListAdminAuthUsersParams } from '@/shared/api/generated/model';
import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody, unwrapListPage } from '@/shared/api/unwrap';

/** Only `administration` has a handler, server-side as in the legacy API. */
const ADMIN_MODE = 'administration';

/** The two legacy user tabs. `system` rows are the platform's own service accounts. */
export type AdminUserType = 'platform' | 'system';

/**
 * One row of `GET /admin/auth_users/administration`.
 *
 * `suspended` is a BOOLEAN, and there is no `status` column: `auth_core__user`
 * has (id, email, name, last_login, suspended) and nothing else. The reference
 * page reads a `status` string here and compares it to `'suspended'`, which no
 * response has ever carried — so its status chip always rendered "Active" and
 * its suspend toggle always computed `true`. This port reads `suspended`.
 */
export interface AdminUserRow {
  readonly id: number;
  readonly name: string;
  readonly email: string;
  /** ISO-8601 to the second, or `null` for a user who has never logged in. */
  readonly last_login: string | null;
  readonly suspended: boolean;
  readonly is_admin: boolean;
  /** Every administration role held, sorted by name (ADR-0059 R0.8b, R0.13); the page reads only this (R0.8c). */
  readonly admin_roles: readonly string[];
}

export interface AdminUsersPage {
  readonly rows: AdminUserRow[];
  readonly total: number;
  /** Tab counts over ALL users — deliberately NOT narrowed by `search`. */
  readonly counts: { readonly platform: number; readonly system: number };
}

export interface AdminUsersQueryParams {
  readonly limit: number;
  readonly offset: number;
  readonly search?: string | undefined;
  readonly userType?: AdminUserType | undefined;
  readonly sortBy?: string | undefined;
  readonly sortOrder?: 'asc' | 'desc' | undefined;
}

/**
 * One query-key namespace for this page's data, declared once.
 *
 * Every mutation below invalidates `adminUsersKeys.all`, so a key built ad hoc
 * at a call site would be a cache the writes never refresh — the read/write
 * key-namespace split that made saved data look absent in #132.
 */
const adminUsersKeys = {
  all: ['admin', 'auth_users'] as const,
  list: (params: AdminUsersQueryParams) => ['admin', 'auth_users', 'list', params] as const,
};

/** The page's camel-cased query as the operation's parameters; empty ones are left out. */
function listParams(params: AdminUsersQueryParams): ListAdminAuthUsersParams {
  return {
    limit: params.limit,
    offset: params.offset,
    ...(params.search ? { search: params.search } : {}),
    ...(params.userType ? { user_type: params.userType } : {}),
    ...(params.sortBy ? { sort_by: params.sortBy } : {}),
    ...(params.sortOrder ? { sort_order: params.sortOrder } : {}),
  };
}

/**
 * `counts` has no home in `unwrapListPage`'s `{rows,total}` result. The
 * transport peel comes from `unwrapBody` (R-A6's sanctioned module) rather than
 * a hand-rolled `resp.data` descent; only the per-field validation is local.
 *
 * A missing or malformed `counts` degrades to zeroes, which renders as tab
 * labels without a count — never as a wrong count.
 */
function readCounts(response: unknown): AdminUsersPage['counts'] {
  const fallback = { platform: 0, system: 0 };
  const body = unwrapBody(response);
  if (typeof body !== 'object' || body === null) return fallback;
  const counts = (body as { counts?: unknown }).counts;
  if (typeof counts !== 'object' || counts === null) return fallback;
  const { platform, system } = counts as { platform?: unknown; system?: unknown };
  return {
    platform: typeof platform === 'number' ? platform : fallback.platform,
    system: typeof system === 'number' ? system : fallback.system,
  };
}

/**
 * One page of `GET /admin/auth_users/administration`, without a hook.
 *
 * Exported because the export control (see `useAdminUsersPage`) has to walk
 * EVERY page imperatively on click; going through `useQuery` there would mean
 * either a second always-mounted query for data nobody renders, or a
 * `limit: total` request whose size no server bound checks.
 */
export async function fetchAdminUsersPage(params: AdminUsersQueryParams): Promise<AdminUsersPage> {
  const response: unknown = await listAdminAuthUsers(ADMIN_MODE, listParams(params));
  const { rows, total } = unwrapListPage<AdminUserRow>(response, 'adminAuthUsers');
  return { rows, total, counts: readCounts(response) };
}

/** `GET /admin/auth_users/administration` — one page of the global user list. */
export function useAdminUsers(
  params: AdminUsersQueryParams,
  options: { enabled?: boolean } = {},
): UseQueryResult<AdminUsersPage, Error> {
  return useQuery({
    queryKey: adminUsersKeys.list(params),
    enabled: options.enabled ?? true,
    queryFn: () => fetchAdminUsersPage(params),
  });
}

async function postAdminUsersAction(body: AdminAuthUsersActionRequest): Promise<void> {
  await adminAuthUsersAction(ADMIN_MODE, body);
}

/** `POST … {action:'delete'}` — removes the users outright (the row cascades). */
export function useDeleteAdminUsers(): UseMutationResult<void, Error, readonly number[]> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (userIds: readonly number[]) =>
      postAdminUsersAction({ action: 'delete', users: userIds.map((id) => ({ id })) }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminUsersKeys.all }),
  });
}

/**
 * `POST … {action:'set_admin_role', role_names}` — the person's whole set of
 * administration roles afterwards (`[]` for none). The server applies the
 * difference and checks each role given or taken (ADR-0059 R0.8b).
 */
export function useSetAdminRoles(): UseMutationResult<void, Error, { userId: number; roleNames: readonly string[] }> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ userId, roleNames }) =>
      postAdminUsersAction({ action: 'set_admin_role', user_id: userId, role_names: [...roleNames] }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminUsersKeys.all }),
  });
}

/** `PUT /admin/user_suspend/administration/{id}` — toggles the suspended flag. */
export function useSuspendAdminUser(): UseMutationResult<
  void,
  Error,
  { userId: number; suspended: boolean }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ userId, suspended }) => {
      await eliteaFetch<unknown>(`/admin/user_suspend/${ADMIN_MODE}/${userId}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ suspended }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminUsersKeys.all }),
  });
}
