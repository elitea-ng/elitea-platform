/**
 * An agent's own memory (ADR-0050 decision 1b, issues 1607 and 1608), over the
 * generated fetchers of the six Memory routes.
 *
 * The generated fetchers resolve the TRANSPORT envelope `{data, status,
 * headers}`, never the body: each call here unwraps it once, so no caller can
 * read a field of the envelope as if it were the answer (#132).
 *
 * What the caller may do comes back with the list (`can_manage`,
 * `can_delete`, `can_approve`); the server decides it per agent (an owner may
 * do everything without the permissions), so the client never derives it from
 * the permission list.
 */
import {
  clearAgentMemory,
  deleteAgentMemoryEntry,
  getAgentMemoryEntry,
  listAgentMemory,
  setAgentMemoryOptOut,
  updateAgentMemoryEntry,
} from '@/shared/api/generated/applications/applications';
import type { AgentMemoryEntry, AgentMemoryList, AgentMemoryUpdate, ListAgentMemoryParams } from '@/shared/api/generated/model';

function bodyOf<T>(envelope: unknown): T {
  return (envelope as { readonly data: T }).data;
}

/* manifest: agentMemory.list */
export async function fetchAgentMemory(projectId: string, applicationId: number, params: ListAgentMemoryParams): Promise<AgentMemoryList> {
  return bodyOf<AgentMemoryList>(await listAgentMemory(projectId, applicationId, params));
}

/* manifest: agentMemory.get */
export async function fetchAgentMemoryEntry(projectId: string, applicationId: number, entryId: number): Promise<AgentMemoryEntry> {
  return bodyOf<AgentMemoryEntry>(await getAgentMemoryEntry(projectId, applicationId, entryId));
}

/* manifest: agentMemory.update */
export async function changeAgentMemoryEntry(
  projectId: string,
  applicationId: number,
  entryId: number,
  change: AgentMemoryUpdate,
): Promise<AgentMemoryEntry> {
  return bodyOf<AgentMemoryEntry>(await updateAgentMemoryEntry(projectId, applicationId, entryId, change));
}

/* manifest: agentMemory.delete */
export async function forgetAgentMemoryEntry(projectId: string, applicationId: number, entryId: number): Promise<void> {
  await deleteAgentMemoryEntry(projectId, applicationId, entryId);
}

/* manifest: agentMemory.clear */
export async function clearAllAgentMemory(projectId: string, applicationId: number): Promise<number> {
  return bodyOf<{ readonly cleared: number }>(await clearAgentMemory(projectId, applicationId)).cleared;
}

/* manifest: agentMemory.optOut */
export async function setAgentMemoryOptedOut(projectId: string, applicationId: number, optedOut: boolean): Promise<void> {
  await setAgentMemoryOptOut(projectId, applicationId, { opted_out: optedOut });
}
