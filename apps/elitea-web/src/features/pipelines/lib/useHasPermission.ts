import { useApplicationPermissionState } from '@/shared/lib/applicationEditAccess';

/**
 * Same permission source as the agent editor (`features/agents/lib/useHasPermission.ts`):
 * both read `shared/lib/applicationEditAccess.ts`, because a pipeline is editable
 * by exactly the callers who can edit an agent.
 */
export function useHasPermission(projectId: string | undefined, permission: string): boolean {
  return useApplicationPermissionState(projectId, permission).allowed;
}

export const usePermissionState = useApplicationPermissionState;
