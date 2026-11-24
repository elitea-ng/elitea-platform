/**
 * What the caller may do on the project DB actions surface (T-5E 5e-2).
 *
 * Mirrors `useHttpActionRulePermissions`: `canManage` gates the full-rule
 * query and every write, `canList` gates the redacted names-only query.
 * `resolved` keeps the no-permission banner from flashing while the
 * permission request is in flight.
 */
import { useMemo } from 'react';

import type { Permission } from '@/shared/api/generated/model';
import { usePermissionList } from '@/shared/api/generated/auth/auth';
import { PERMISSIONS } from '@/shared/lib/permissions';

export interface DbActionRulePermissions {
  readonly resolved: boolean;
  /** `models.db_action_rules.manage` — full rules, create and edit. */
  readonly canManage: boolean;
  /** `models.db_action_rules.list` — redacted names, dialect, mode and enabled flags only. */
  readonly canList: boolean;
}

export function useDbActionRulePermissions(projectId: string): DbActionRulePermissions {
  const permissionQuery = usePermissionList(projectId, { query: { enabled: !!projectId } });

  return useMemo(() => {
    const list = permissionQuery.data?.data as Permission[] | undefined;
    const granted = new Set((list ?? []).filter((entry) => entry.enabled).map((entry) => entry.name));
    return {
      resolved: permissionQuery.isSuccess || permissionQuery.isError,
      canManage: granted.has(PERMISSIONS.dbActionRules.manage),
      canList: granted.has(PERMISSIONS.dbActionRules.list),
    };
  }, [permissionQuery.data, permissionQuery.isSuccess, permissionQuery.isError]);
}
