/**
 * The deprecated permissions of the Roles matrix (ADR-0059 R0.14a).
 *
 * The server refuses a save that adds a deprecated name to a role that does not
 * hold it (`permission_deprecated`), and keeps the grants that exist. So each
 * deprecated name carries the roles that hold it ON THE SERVER: those may keep
 * or drop it, every other role's cell is locked.
 */
import type { PermissionMatrixRow } from './api/adminRolesApi';

/** Deprecated name → the roles that hold it in the server's matrix. */
export type DeprecatedGrants = ReadonlyMap<string, ReadonlySet<string>>;

export function deprecatedGrants(
  names: readonly string[],
  serverRows: readonly PermissionMatrixRow[],
): DeprecatedGrants {
  const byName = new Map(serverRows.map((row) => [row.name, row]));
  const result = new Map<string, ReadonlySet<string>>();
  for (const name of names) {
    const row = byName.get(name);
    const holders = row
      ? Object.entries(row).flatMap(([role, held]) => (role !== 'name' && held === true ? [role] : []))
      : [];
    result.set(name, new Set(holders));
  }
  return result;
}

/** The cell is a deprecated name the role does not hold: it cannot be granted. */
export function isLockedGrant(deprecated: DeprecatedGrants | undefined, name: string, role: string): boolean {
  const holders = deprecated?.get(name);
  return holders !== undefined && !holders.has(role);
}
