import { speaksContract } from '@/api/discovery';
import { WORKFORCE_CONTRACT } from '@/api/queries/workforce';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/** The workforce proposals and the probation badge need client contract 1.56. */
export function useWorkforceAvailable(): boolean {
  const workspace = useWorkspaceInfo();
  return speaksContract(workspace?.discovery, WORKFORCE_CONTRACT.major, WORKFORCE_CONTRACT.minor);
}
