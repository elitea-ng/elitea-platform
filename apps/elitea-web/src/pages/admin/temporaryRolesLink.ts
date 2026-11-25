/**
 * TEMPORARY: REMOVE IN R3.6 (issue 2537), with every use of it.
 *
 * Under the six areas (R3.2, issue 2532) Roles has no link: its item,
 * `access`, is in the console table with `available: false` until R3.6
 * ships the Access page, and the capability document leaves an unavailable
 * item out. Until then this puts a "Roles" link under "People and access"
 * for a caller whose roles open the item.
 *
 * The document cannot say so itself, so the answer is the server's two
 * answers put together, still with no permission name: the caller's roles
 * (`roles` of the document) against the roles that open `access`
 * (`GET /admin/console/items/access`, the same read the access-denied page
 * makes). Until both answer, or when either fails, there is no link: the
 * page still opens by its address, as before.
 */
import { useGetAdminConsoleItem } from '@/shared/api/generated/admin/admin';
import type { AdminConsoleRole } from '@/shared/api/generated/model';

/** The console item of the Roles page. */
const ROLES_ITEM_ID = 'access';

/**
 * A role by name AND kind: a custom role may share a built-in role's name,
 * and it does not open what the built-in one opens.
 */
function roleKey(role: { readonly name: string; readonly built_in: boolean }): string {
  return `${role.built_in ? 'built-in' : 'custom'}:${role.name}`;
}

/** Whether to show the temporary Roles link; `false` until the answer is known. */
export function useTemporaryRolesLink(callerRoles: readonly AdminConsoleRole[] | undefined, enabled: boolean): boolean {
  const ask = enabled && callerRoles !== undefined && callerRoles.length > 0;
  const query = useGetAdminConsoleItem(ROLES_ITEM_ID, {
    query: {
      enabled: ask,
      retry: false,
      staleTime: 60_000,
      select: (response) =>
        response.status === 200 ? response.data.granting_roles.map((role) => roleKey(role)) : undefined,
    },
  });
  if (!ask || query.data === undefined) return false;
  const granting = new Set(query.data);
  return callerRoles.some((role) => granting.has(roleKey(role)));
}
