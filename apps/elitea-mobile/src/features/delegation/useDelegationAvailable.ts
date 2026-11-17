import { speaksContract } from '@/api/discovery';
import { DELEGATION_CONTRACT } from '@/api/queries/delegation';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/** Delegation grants need client contract 1.59 (ADR-0044 amendment 1): the one gate the entry points and the screen share. */
export function useDelegationAvailable(): boolean {
  const workspace = useWorkspaceInfo();
  return speaksContract(workspace?.discovery, DELEGATION_CONTRACT.major, DELEGATION_CONTRACT.minor);
}
