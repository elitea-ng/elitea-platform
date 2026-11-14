import { useApplicationPermissionState } from '@/shared/lib/applicationEditAccess';

/**
 * The baseline's `useCheckPermission().checkPermission`
 * (`apps/elitea-ui/src/hooks/useCheckPermission.js`). Reads the permission list through `shared/lib/applicationEditAccess.ts`, shared with the pipeline editor.
 */
export function useHasPermission(projectId: string | undefined, permission: string): boolean {
  return useApplicationPermissionState(projectId, permission).allowed;
}
