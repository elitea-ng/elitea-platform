/**
 * What the caller may do on the Evaluation tab.
 *
 * The ten strings are gated by `internal/api/router.go` and granted by
 * `migrations/shared/0104_evaluation_dimension_permissions.sql` and
 * `migrations/shared/0116_evaluation_dataset_run_permissions.sql`; a viewer
 * holds the three reads and none of the writes.
 *
 * EVERY `canRead*` GATES A QUERY, not just a control. With no read permission
 * the sub-view must not ask the server at all — the request would answer 403,
 * and a 403 rendered as an error banner tells a viewer their product is broken
 * when in fact they simply may not author rubrics.
 *
 * The dataset and run permissions are read from the SAME permission list as the
 * dimension four, in one request. Three hooks over three sub-views would make
 * three identical calls and give the tab three independent loading states.
 */
import { useMemo } from 'react';

import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';
import { PERMISSIONS } from '@/shared/lib/permissions';

export interface EvaluationPermissions {
  readonly canRead: boolean;
  readonly canCreate: boolean;
  readonly canUpdate: boolean;
  readonly canDelete: boolean;
  readonly canReadDatasets: boolean;
  readonly canCreateDatasets: boolean;
  readonly canUpdateDatasets: boolean;
  readonly canDeleteDatasets: boolean;
  readonly canReadRuns: boolean;
  readonly canStartRuns: boolean;
}

export function useEvaluationPermissions(projectId: string | undefined): EvaluationPermissions {
  const permissionQuery = usePermissionList(projectId ?? '', {
    query: { enabled: projectId !== undefined && projectId !== '' },
  });

  return useMemo(() => {
    const list = permissionQuery.data?.data as Permission[] | undefined;
    const granted = new Set((list ?? []).filter((entry) => entry.enabled).map((entry) => entry.name));
    return {
      canRead: granted.has(PERMISSIONS.evaluation.dimensionRead),
      canCreate: granted.has(PERMISSIONS.evaluation.dimensionCreate),
      canUpdate: granted.has(PERMISSIONS.evaluation.dimensionUpdate),
      canDelete: granted.has(PERMISSIONS.evaluation.dimensionDelete),
      canReadDatasets: granted.has(PERMISSIONS.evaluation.datasetRead),
      canCreateDatasets: granted.has(PERMISSIONS.evaluation.datasetCreate),
      canUpdateDatasets: granted.has(PERMISSIONS.evaluation.datasetUpdate),
      canDeleteDatasets: granted.has(PERMISSIONS.evaluation.datasetDelete),
      canReadRuns: granted.has(PERMISSIONS.evaluation.runRead),
      canStartRuns: granted.has(PERMISSIONS.evaluation.runCreate),
    };
  }, [permissionQuery.data]);
}
