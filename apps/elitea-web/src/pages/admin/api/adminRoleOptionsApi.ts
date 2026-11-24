/**
 * The administration roles the Users page can assign (ADR-0059 R0.8c, issue
 * 2586), read from the central permission matrix
 * (`GET /admin/permissions/administration/administration`).
 *
 * The matrix is the roles API of this console: one boolean column per role,
 * and, since R0.8c, a `roles` list that says which column is built-in and with
 * which seed marker (`elitea_auth.admin_role_seeds`). Built-in is a recorded
 * fact, not a name, so nothing here lists role names.
 *
 * The page decides nothing from this. It hides a role that is never assigned
 * (the marker `unassignable`) and a role that holds no permission, and it
 * does NOT filter by what the caller may assign: the server refuses, and the
 * page shows the refusal (`./adminRolesApi`'s `roleRefusalMessage`).
 */
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { PermissionMatrixRoleColumn } from '@/shared/api/generated/model/permissionMatrixRoleColumn.zod';
import { eliteaFetch } from '@/shared/api/generated/mutator';
import { unwrapBody } from '@/shared/api/unwrap';

/** One administration role as the picker needs it. */
export interface AdministrationRole {
  readonly name: string;
  /** The role has a seed row: its label and description come from `./adminRoleCopy`. */
  readonly builtIn: boolean;
  /** `declared`, `as_found`, `unassignable`, or `null` for a custom role. */
  readonly marker: string | null;
  /** The server's English display name of a built-in role, if it sent one. */
  readonly displayName: string | undefined;
  /** At least one cell of the role's column is granted. */
  readonly holdsPermissions: boolean;
}

const MATRIX_URL = '/admin/permissions/administration/administration';

/**
 * Under the Roles page's key namespace (`['admin', 'roles']`), so a matrix
 * save or a role created there refreshes this list too.
 */
const ROLE_OPTIONS_KEY = ['admin', 'roles', 'options'] as const;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

interface ColumnFacts {
  readonly builtIn: boolean;
  readonly marker: string | null;
  readonly displayName: string | undefined;
}

/** One `roles` entry, by the contract's `PermissionMatrixRoleColumn`; a malformed entry is skipped. */
function readColumn(value: unknown): [string, ColumnFacts] | undefined {
  const parsed = PermissionMatrixRoleColumn.safeParse(value);
  if (!parsed.success) return undefined;
  const { name, built_in: builtIn, marker, display_name: displayName } = parsed.data;
  return [name, { builtIn, marker, displayName: displayName === '' ? undefined : displayName }];
}

/** The role columns of the rows, in first-seen order (a server without `roles`). */
function columnsOfRows(rows: readonly Record<string, unknown>[]): string[] {
  const names: string[] = [];
  for (const row of rows) {
    for (const key of Object.keys(row)) {
      if (key !== 'name' && !names.includes(key)) names.push(key);
    }
  }
  return names;
}

/** The matrix body → the roles, in the server's column order. */
function readAdministrationRoles(response: unknown): AdministrationRole[] {
  const body = unwrapBody(response);
  if (!isRecord(body)) return [];
  const rows = Array.isArray(body['rows']) ? body['rows'].filter(isRecord) : [];
  const described = new Map(
    (Array.isArray(body['roles']) ? body['roles'] : [])
      .map(readColumn)
      .filter((entry): entry is [string, ColumnFacts] => entry !== undefined),
  );
  const names = described.size > 0 ? [...described.keys()] : columnsOfRows(rows);
  return names.map((name) => ({
    name,
    builtIn: described.get(name)?.builtIn ?? false,
    marker: described.get(name)?.marker ?? null,
    displayName: described.get(name)?.displayName,
    holdsPermissions: rows.some((row) => row[name] === true),
  }));
}

/** A role the picker offers to everyone: assignable and holding something. */
export function isOfferedRole(role: AdministrationRole): boolean {
  return role.marker !== 'unassignable' && role.holdsPermissions;
}

/** Every administration role with its seed facts. */
export function useAdministrationRoles(
  options: { enabled?: boolean } = {},
): UseQueryResult<AdministrationRole[], Error> {
  return useQuery({
    queryKey: ROLE_OPTIONS_KEY,
    enabled: options.enabled ?? true,
    queryFn: async () => readAdministrationRoles(await eliteaFetch<unknown>(MATRIX_URL)),
  });
}
