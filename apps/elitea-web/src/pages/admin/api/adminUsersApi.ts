/**
 * REST client for the GLOBAL (admin-panel) users surface — unit A14.
 *
 * This is the admin twin of `pages/settings/Users.tsx`'s project-scoped
 * membership API, and it is a DIFFERENT backend surface: `/admin/users/…`
 * lists the members of one project, `/admin/auth_users/administration` lists
 * every `auth_core__user` row in the deployment. They share no endpoint, no
 * row shape and no query key.
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes (there is no `auth_users` operation in
 * `shared/api/generated/**`). Handwritten in the same shape as
 * `shared/api/configurationsApi.ts` and `entities/secret/api/secretApi.ts`.
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

import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody, unwrapListPage } from '@/shared/api/unwrap';

/** Only `administration` has a handler, server-side as in the legacy API. */
const ADMIN_MODE = 'administration';

/** The two legacy user tabs. `system` rows are the platform's own service accounts. */
export type AdminUserType = 'platform' | 'system';

/** The administration-mode roles `set_admin_role` accepts. `null` clears them all. */
export type AdminRole = 'super_admin' | 'admin' | 'editor' | 'viewer';

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
  readonly admin_role: AdminRole | null;
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

function buildListUrl(params: AdminUsersQueryParams): string {
  const query = new URLSearchParams({
    limit: String(params.limit),
    offset: String(params.offset),
  });
  if (params.search) query.set('search', params.search);
  if (params.userType) query.set('user_type', params.userType);
  if (params.sortBy) query.set('sort_by', params.sortBy);
  if (params.sortOrder) query.set('sort_order', params.sortOrder);
  return `/admin/auth_users/${ADMIN_MODE}?${query.toString()}`;
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
  const response = await eliteaFetch<unknown>(buildListUrl(params));
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

async function postAdminUsersAction(body: Record<string, unknown>): Promise<void> {
  await eliteaFetch<unknown>(`/admin/auth_users/${ADMIN_MODE}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
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

/** `POST … {action:'set_admin_role'}` — `null` revokes every administration role. */
export function useSetAdminRole(): UseMutationResult<
  void,
  Error,
  { userId: number; roleName: AdminRole | null }
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ userId, roleName }) =>
      postAdminUsersAction({ action: 'set_admin_role', user_id: userId, role_name: roleName }),
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
