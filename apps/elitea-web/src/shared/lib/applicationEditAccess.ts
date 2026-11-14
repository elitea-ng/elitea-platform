import { useMemo } from 'react';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';

/**
 * The ONE rule for "may this caller edit an agent or a pipeline", read by the
 * agent editor and the pipeline editor alike. Both are applications; a pipeline
 * is editable by exactly the callers who can edit an agent's configuration
 * (project admin/editor roles, which carry `application.update`), and a
 * published one is read-only for everyone. Two private copies of this rule used
 * to exist, one per feature, and could drift apart.
 */
export function canEditApplication(isPublished: boolean, hasEditPermission: boolean): boolean {
  return !isPublished && hasEditPermission;
}

/** `settled` is false until the permission list answered (or failed), so a caller can tell "not yet known" from "no". */
export function useApplicationPermissionState(
  projectId: string | undefined,
  permission: string,
): { readonly allowed: boolean; readonly settled: boolean } {
  const query = usePermissionList(projectId ?? '', { query: { enabled: projectId !== undefined } });

  const permissions = useMemo(() => {
    // `eliteaFetch` throws instead of resolving with the error envelope, so the declared union never reaches here.
    const list = query.data?.data as Permission[] | undefined;
    if (!list) return new Set<string>();
    return new Set(list.filter((entry) => entry.enabled).map((entry) => entry.name));
  }, [query.data]);

  return { allowed: permissions.has(permission), settled: projectId !== undefined && !query.isPending };
}
