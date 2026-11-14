import { useMemo } from 'react';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';

/**
 * Whether the caller may edit an agent or a pipeline in the editors that read
 * this module (the agent editor and the pipeline editor): not when it is
 * published, otherwise when they hold `application.update`. It replaces two
 * private copies of the same rule. A pure refactor: no behaviour changed.
 * Other surfaces (`features/chat-input`) still carry their own permission check.
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
