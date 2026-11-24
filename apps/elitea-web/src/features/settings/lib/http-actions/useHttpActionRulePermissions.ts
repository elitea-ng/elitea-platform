/**
 * What the caller may do on the project HTTP actions surface (T-5D).
 *
 * Same shape as `useWebhookPermissions`: `canManage` gates the full-rule
 * query and every write, `canList` gates the names-only query. `resolved`
 * keeps the no-permission banner from flashing while the permission request
 * is in flight (the same reason `pages/settings/Users.tsx` tracks it).
 */
import { useMemo } from 'react';

import type { Permission } from '@/shared/api/generated/model';
import { usePermissionList } from '@/shared/api/generated/auth/auth';
import { PERMISSIONS } from '@/shared/lib/permissions';

export interface HttpActionRulePermissions {
  readonly resolved: boolean;
  /** `models.http_action_rules.manage` — full rules, create and edit. */
  readonly canManage: boolean;
  /** `models.http_action_rules.list` — names and enabled flags only. */
  readonly canList: boolean;
}

export function useHttpActionRulePermissions(projectId: string): HttpActionRulePermissions {
  const permissionQuery = usePermissionList(projectId, { query: { enabled: !!projectId } });

  return useMemo(() => {
    const list = permissionQuery.data?.data as Permission[] | undefined;
    const granted = new Set((list ?? []).filter((entry) => entry.enabled).map((entry) => entry.name));
    return {
      resolved: permissionQuery.isSuccess || permissionQuery.isError,
      canManage: granted.has(PERMISSIONS.httpActionRules.manage),
      canList: granted.has(PERMISSIONS.httpActionRules.list),
    };
  }, [permissionQuery.data, permissionQuery.isSuccess, permissionQuery.isError]);
}
