/**
 * REST client for the admin Roles surface — the permission matrix (unit A14).
 *
 * Not generated: `orval` builds from `v2.yaml`, which does not describe the
 * admin-panel routes. Handwritten in the same shape as `./adminUsersApi` and
 * `./adminAuditApi`.
 *
 * The wire contract mirrors pylon (`legacy/plugins/admin/api/v2/permissions.py`)
 * and its existing client (`frontends/admin_ui/frontend/src/api/usersApi.js`)
 * exactly — same paths, same body — because #137 showed what inventing a shape
 * costs. Its first path segment is the pylon MODE and selects which matrix is
 * being edited; this module calls it `scope` to keep it distinct from the target
 * mode in the second segment:
 *
 *   GET|PUT  /admin/permissions/administration/{administration|default|developer}
 *   GET|PUT  /admin/permissions/public/{targetMode}     ← the public project
 *   GET|PUT  /admin/permissions/support/{targetMode}    ← the support project
 *   POST     /admin/permissions/administration/default  ← "Apply to Projects"
 *
 * All four were served by ONE Go handler that ignored `scope` entirely, and the
 * PUT and POST had no route at all, until `internal/api/v2/admin/roles.go`.
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

/** Which matrix is being read or written. See this module's header. */
type PermissionScope = 'administration' | 'public' | 'support';

/**
 * One row of the matrix: a permission name plus one boolean column per role.
 *
 * The role columns are DYNAMIC — the server reports whichever roles the selected
 * scope defines, and a deployment may define others. Hard-coding them here would
 * silently hide a role from the operator, so they are read off the row.
 */
export interface PermissionMatrixRow {
  readonly name: string;
  readonly [role: string]: string | boolean | undefined;
}

export interface PermissionMatrixTarget {
  readonly scope: PermissionScope;
  /** The pylon `target_mode`. Ignored by the server for the project scopes. */
  readonly targetMode: string;
}

/**
 * One query-key namespace for this page, declared once.
 *
 * Both mutations invalidate `adminRolesKeys.all`, so a key built ad hoc at a
 * call site would be a cache the writes never refresh — the read/write
 * key-namespace split that made saved data look absent in #132.
 */
const adminRolesKeys = {
  all: ['admin', 'roles'] as const,
  matrix: (target: PermissionMatrixTarget) =>
    ['admin', 'roles', 'matrix', target.scope, target.targetMode] as const,
};

function matrixUrl(target: PermissionMatrixTarget): string {
  return `/admin/permissions/${target.scope}/${target.targetMode}`;
}

/**
 * The server's own explanation of a refusal, when it gave one.
 *
 * It matters here more than elsewhere: a Roles tab can be legitimately
 * unavailable (the support project is not configured) or legitimately refused
 * (the caller lacks `configuration.roles.permissions.edit`), and both are 4xx.
 * Rendering "Failed to load" over either would hide a fact the operator needs —
 * so the reason is surfaced verbatim rather than replaced with a generic string.
 */
export function permissionMatrixFailureReason(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  // `auth` failures carry no body — a 401 is handled by the re-auth path, not
  // rendered as a reason — so only `http` can explain itself.
  if (failure.kind !== 'http') return undefined;
  const body = failure.body;
  if (typeof body !== 'object' || body === null) return undefined;
  const reason = (body as { error?: unknown }).error;
  return typeof reason === 'string' && reason !== '' ? reason : undefined;
}

/** `GET /admin/permissions/{scope}/{targetMode}` — the whole matrix, unpaginated. */
export function usePermissionMatrix(
  target: PermissionMatrixTarget,
  options: { enabled?: boolean } = {},
): UseQueryResult<PermissionMatrixRow[], Error> {
  return useQuery({
    queryKey: adminRolesKeys.matrix(target),
    enabled: options.enabled ?? true,
    queryFn: async (): Promise<PermissionMatrixRow[]> =>
      unwrapListPage<PermissionMatrixRow>(
        await eliteaFetch<unknown>(matrixUrl(target)),
        'adminPermissionMatrix',
      ).rows,
  });
}

interface PermissionMatrixSaveVariables {
  readonly target: PermissionMatrixTarget;
  readonly rows: readonly PermissionMatrixRow[];
}

/**
 * `PUT /admin/permissions/{scope}/{targetMode}` — the matrix as the page shows
 * it. The server diffs it against the stored one and applies the difference.
 */
export function useSavePermissionMatrix(): UseMutationResult<
  void,
  Error,
  PermissionMatrixSaveVariables
> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ target, rows }: PermissionMatrixSaveVariables) => {
      await eliteaFetch<unknown>(matrixUrl(target), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(rows),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminRolesKeys.all }),
  });
}

/**
 * `POST /admin/permissions/administration/default` — pushes the standard matrix
 * onto every shared project. Defined for that one target only, server-side and
 * in pylon before it.
 */
export function useSyncPermissionMatrix(): UseMutationResult<void, Error, void> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      await eliteaFetch<unknown>('/admin/permissions/administration/default', { method: 'POST' });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminRolesKeys.all }),
  });
}

/* ── role DEFINITIONS: create, rename, delete (gap G9) ─────────────────── */

/**
 * `POST | PUT | DELETE /admin/roles/{scope}/{targetMode}`.
 *
 * The matrix above edits the CELLS of a table whose columns already exist.
 * These three change the columns themselves. pylon has had them all along
 * (`legacy/plugins/admin/api/v2/roles.py`); Go had only the two GET listings, so
 * a deployment that wanted a `security_reviewer` role had to INSERT it by hand,
 * and a hand-inserted role reached projects created afterwards and no existing
 * one.
 *
 * They take the SAME `{scope}/{targetMode}` pair the matrix does, because a role
 * belongs to exactly one of this page's four tabs. The bodies are pylon's:
 * `{name}` for create and delete, `{name, new_name}` for the rename.
 *
 * Handwritten, like the matrix calls above and for the same reason: `v2.yaml`
 * describes `GET /admin/roles/default/{project_id}` — the project role LISTING,
 * which the app calls through a generated hook — and none of the
 * `{scope}/{mode}` admin-panel routes. Describing these three would put them in
 * a spec whose siblings on the same page are absent from it, so they follow the
 * convention this module already states in its header.
 *
 * Each returns the invalidation promise from `onSuccess`, so the mutation does
 * not settle until the matrix has been re-read. `useAdminRolesPage` re-seeds its
 * draft on that settlement; without the await it would re-seed from the matrix
 * that still lacks the new column.
 */
function roleDefinitionUrl(target: PermissionMatrixTarget): string {
  return `/admin/roles/${target.scope}/${target.targetMode}`;
}

export interface RoleCreateVariables {
  readonly target: PermissionMatrixTarget;
  readonly name: string;
}

export interface RoleRenameVariables {
  readonly target: PermissionMatrixTarget;
  readonly name: string;
  readonly newName: string;
}

export interface RoleDeleteVariables {
  readonly target: PermissionMatrixTarget;
  readonly name: string;
}

export function useCreateRole(): UseMutationResult<void, Error, RoleCreateVariables> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ target, name }: RoleCreateVariables) => {
      await eliteaFetch<unknown>(roleDefinitionUrl(target), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminRolesKeys.all }),
  });
}

export function useRenameRole(): UseMutationResult<void, Error, RoleRenameVariables> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ target, name, newName }: RoleRenameVariables) => {
      await eliteaFetch<unknown>(roleDefinitionUrl(target), {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, new_name: newName }),
      });
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminRolesKeys.all }),
  });
}

/**
 * The name travels in the BODY, which is what pylon's client sends and what the
 * Go handler reads first, AND in the query string. A DELETE body is legal but is
 * dropped by enough intermediaries that a delete which silently became "no name
 * given" is a real failure mode; the server prefers the body and falls back to
 * `?name=`.
 */
export function useDeleteRole(): UseMutationResult<void, Error, RoleDeleteVariables> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async ({ target, name }: RoleDeleteVariables) => {
      await eliteaFetch<unknown>(
        `${roleDefinitionUrl(target)}?name=${encodeURIComponent(name)}`,
        {
          method: 'DELETE',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name }),
        },
      );
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: adminRolesKeys.all }),
  });
}
