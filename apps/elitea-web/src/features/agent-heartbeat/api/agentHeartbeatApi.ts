/**
 * The heartbeat of a proactive agent (ADR-0050 decision 2, issue 1618), over
 * the generated fetchers. Each call unwraps the transport envelope once, so
 * no caller reads a field of `{data, status, headers}` as the answer.
 */
import { getAgentHeartbeat, listAgentHeartbeats, saveAgentHeartbeat } from '@/shared/api/generated/applications/applications';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type { AgentHeartbeatList, AgentHeartbeatSave, AgentHeartbeatSettings } from '@/shared/api/generated/model';

function bodyOf<T>(envelope: unknown): T {
  return (envelope as { readonly data: T }).data;
}

/* manifest: agentHeartbeat.get */
export async function fetchAgentHeartbeat(projectId: string, applicationId: number): Promise<AgentHeartbeatSettings> {
  return bodyOf<AgentHeartbeatSettings>(await getAgentHeartbeat(projectId, applicationId));
}

/* manifest: agentHeartbeat.save */
export async function storeAgentHeartbeat(
  projectId: string,
  applicationId: number,
  save: AgentHeartbeatSave,
): Promise<AgentHeartbeatSettings> {
  return bodyOf<AgentHeartbeatSettings>(await saveAgentHeartbeat(projectId, applicationId, save));
}

/* manifest: agentHeartbeat.history */
export async function fetchAgentHeartbeats(projectId: string, applicationId: number, limit: number): Promise<AgentHeartbeatList> {
  return bodyOf<AgentHeartbeatList>(await listAgentHeartbeats(projectId, applicationId, { limit }));
}

/** The server's sentence of a refused save (422), or undefined. */
export function refusalMessage(error: unknown): string | undefined {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http' || error.failure.status !== 422) {
    return undefined;
  }
  const body = error.failure.body as { readonly error?: unknown } | null;
  return typeof body?.error === 'string' ? body.error : undefined;
}
