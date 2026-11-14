import { useApplicationPermissionState } from '@/shared/lib/applicationEditAccess';

/**
 * The baseline's `useCheckPermission().checkPermission`
 * (`apps/elitea-ui/src/hooks/useCheckPermission.js`). Shares its permission
 * source with the pipeline editor through `shared/lib/applicationEditAccess.ts`,
 * so the two editors cannot disagree about who may edit.
 */
export function useHasPermission(projectId: string | undefined, permission: string): boolean {
  return useApplicationPermissionState(projectId, permission).allowed;
}
