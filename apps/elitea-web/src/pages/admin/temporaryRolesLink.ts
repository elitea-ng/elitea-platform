/**
 * TEMPORARY: REMOVE IN R3.6 (issue 2537), with every use of it.
 *
 * Under the six areas (R3.2, issue 2532) Roles has no link: its item,
 * `access`, is in the console table with `available: false` until R3.6
 * ships the Access page, and the capability document leaves an unavailable
 * item out. Until then this puts a "Roles" link under "People and access"
 * for a caller whose roles open the item, and gives the Roles page its level
 * for the read-only wrapper (R3.4b, issue 2613).
 *
 * The answer is the server's two answers put together: the caller's roles
 * (`roles` of the document) against the roles that open `access` and at
 * which level (`GET /admin/console/items/access`, the same read the
 * access-denied page makes). Until both answer, or when either fails, there
 * is no link and no level: the page still opens by its address and its
 * controls stay on for the server to answer, as before.
 *
 * Today's Roles page IS the permission matrix, and `access` also opens to
 * `access.roles.view`, which reads who holds what (R3.9) and never the
 * matrix: `GET /admin/permissions` needs `configuration.roles.permissions.view`.
 * So a caller whose roles open `access` but who holds neither matrix
 * permission (the security auditor) gets no link and the level `none`
 * (issue 3042). The document names no permission, so the matrix half is
 * read from `window.admin_ui_config.permissions`, the probe the navigation
 * before the six areas used for this link.
 */
import { useGetAdminConsoleItem } from '@/shared/api/generated/admin/admin';
import type { AdminConsoleGrantingRole, AdminConsoleRole } from '@/shared/api/generated/model';

import { adminUiPermissionsKnown, adminUiShowsControlFor } from './adminUiConfig';

/**
 * The level the derivation can answer: a subset of `AdminItemAccess`, kept
 * local so this module does not import `./adminWriteAccess`, which imports it.
 */
export type TemporaryRolesLevel = 'read' | 'write' | 'none';

/** The console item of the Roles page. */
export const ROLES_ITEM_ID = 'access';

/** The permissions that read the matrix; either one opens today's Roles page. */
const MATRIX_PERMISSIONS = ['configuration.roles.permissions.view', 'configuration.roles.permissions.edit'];

/**
 * A role by name AND kind: a custom role may share a built-in role's name,
 * and it does not open what the built-in one opens.
 */
function roleKey(role: { readonly name: string; readonly built_in: boolean }): string {
  return `${role.built_in ? 'built-in' : 'custom'}:${role.name}`;
}

/**
 * The caller's level on the Roles page: `undefined` when none of its roles is
 * a granting one (the caller reached the page some other way, so the server
 * decides); `none` when they are, but the caller holds no matrix permission;
 * otherwise `write` when any of its roles writes the item, else `read`.
 */
export function temporaryRolesLevel(
  callerRoles: readonly AdminConsoleRole[],
  granting: readonly AdminConsoleGrantingRole[],
  holdsMatrix: boolean,
): TemporaryRolesLevel | undefined {
  const mine = new Set(callerRoles.map((role) => roleKey(role)));
  const levels = granting.filter((role) => mine.has(roleKey(role))).map((role) => role.level);
  if (levels.length === 0) return undefined;
  if (!holdsMatrix) return 'none';
  return levels.includes('write') ? 'write' : 'read';
}

/**
 * Whether the caller holds a matrix permission, as the admin handler states
 * it. When the list is not the caller's (no config in dev, or a resolver
 * failure the handler reports as `unavailable`), the answer is yes: failure
 * must not read as no access, so the level falls back to the roles alone and
 * the server decides.
 */
function holdsMatrixPermission(): boolean {
  if (!adminUiPermissionsKnown()) return true;
  return MATRIX_PERMISSIONS.some((permission) => adminUiShowsControlFor(permission));
}

/** The roles that open `access`, once both answers are known. */
function useGrantingRoles(
  callerRoles: readonly AdminConsoleRole[] | undefined,
  enabled: boolean,
): readonly AdminConsoleGrantingRole[] | undefined {
  const ask = enabled && callerRoles !== undefined && callerRoles.length > 0;
  const query = useGetAdminConsoleItem(ROLES_ITEM_ID, {
    query: {
      enabled: ask,
      retry: false,
      staleTime: 60_000,
      select: (response) => (response.status === 200 ? response.data.granting_roles : undefined),
    },
  });
  return ask ? query.data : undefined;
}

/** The Roles page's level for the read-only wrapper; `undefined` until known. */
export function useTemporaryRolesAccess(
  callerRoles: readonly AdminConsoleRole[] | undefined,
  enabled: boolean,
): TemporaryRolesLevel | undefined {
  const granting = useGrantingRoles(callerRoles, enabled);
  if (callerRoles === undefined || granting === undefined) return undefined;
  return temporaryRolesLevel(callerRoles, granting, holdsMatrixPermission());
}

/** Whether to show the temporary Roles link; `false` until the answer is known, and at `none`. */
export function useTemporaryRolesLink(callerRoles: readonly AdminConsoleRole[] | undefined, enabled: boolean): boolean {
  const level = useTemporaryRolesAccess(callerRoles, enabled);
  return level === 'read' || level === 'write';
}
