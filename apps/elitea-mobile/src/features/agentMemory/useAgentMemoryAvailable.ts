import { speaksContract } from '@/api/discovery';
import { AGENT_MEMORY_CONTRACT } from '@/api/queries/agentMemory';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/** Agent memory needs client contract 1.44 (ADR-0050 1b): the one gate the entry points and the screen share. */
export function useAgentMemoryAvailable(): boolean {
  const workspace = useWorkspaceInfo();
  return speaksContract(workspace?.discovery, AGENT_MEMORY_CONTRACT.major, AGENT_MEMORY_CONTRACT.minor);
}
