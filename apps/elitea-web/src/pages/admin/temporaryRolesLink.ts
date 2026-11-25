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
 * The document cannot say so itself, so the answer is the server's two
 * answers put together, still with no permission name: the caller's roles
 * (`roles` of the document) against the roles that open `access` and at
 * which level (`GET /admin/console/items/access`, the same read the
 * access-denied page makes). Until both answer, or when either fails, there
 * is no link and no level: the page still opens by its address and its
 * controls stay on for the server to answer, as before.
 */
import { useGetAdminConsoleItem } from '@/shared/api/generated/admin/admin';
import type { AdminConsoleGrantingRole, AdminConsoleRole } from '@/shared/api/generated/model';

import type { AdminItemAccess } from './adminWriteAccess';

/** The console item of the Roles page. */
export const ROLES_ITEM_ID = 'access';

/**
 * A role by name AND kind: a custom role may share a built-in role's name,
 * and it does not open what the built-in one opens.
 */
function roleKey(role: { readonly name: string; readonly built_in: boolean }): string {
  return `${role.built_in ? 'built-in' : 'custom'}:${role.name}`;
}

/**
 * The caller's level on `access`: `write` when any of its roles writes,
 * `read` when one reads, `undefined` when none of its roles is a granting
 * one (the caller reached the page some other way, so the server decides).
 */
export function temporaryRolesLevel(
  callerRoles: readonly AdminConsoleRole[],
  granting: readonly AdminConsoleGrantingRole[],
): AdminItemAccess | undefined {
  const mine = new Set(callerRoles.map((role) => roleKey(role)));
  const levels = granting.filter((role) => mine.has(roleKey(role))).map((role) => role.level);
  if (levels.includes('write')) return 'write';
  return levels.includes('read') ? 'read' : undefined;
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

/** Whether to show the temporary Roles link; `false` until the answer is known. */
export function useTemporaryRolesLink(callerRoles: readonly AdminConsoleRole[] | undefined, enabled: boolean): boolean {
  const granting = useGrantingRoles(callerRoles, enabled);
  if (callerRoles === undefined || granting === undefined) return false;
  return temporaryRolesLevel(callerRoles, granting) !== undefined;
}

/** The Roles page's level for the read-only wrapper; `undefined` until known. */
export function useTemporaryRolesAccess(
  callerRoles: readonly AdminConsoleRole[] | undefined,
  enabled: boolean,
): AdminItemAccess | undefined {
  const granting = useGrantingRoles(callerRoles, enabled);
  if (callerRoles === undefined || granting === undefined) return undefined;
  return temporaryRolesLevel(callerRoles, granting);
}
