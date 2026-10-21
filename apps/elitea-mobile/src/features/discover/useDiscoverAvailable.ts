import { speaksContract } from '@/api/discovery';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/** Discover needs client contract 1.3 (the catalogue rows carry the public project and the published version). */
export function useDiscoverAvailable(): boolean {
  const workspace = useWorkspaceInfo();
  return speaksContract(workspace?.discovery, 1, 3);
}
