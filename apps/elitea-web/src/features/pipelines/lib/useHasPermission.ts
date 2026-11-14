import { useApplicationPermissionState } from '@/shared/lib/applicationEditAccess';

/** Reads the permission list through `shared/lib/applicationEditAccess.ts`, shared with the agent editor. */
export function useHasPermission(projectId: string | undefined, permission: string): boolean {
  return useApplicationPermissionState(projectId, permission).allowed;
}

export const usePermissionState = useApplicationPermissionState;
