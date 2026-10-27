/**
 * Hand-written client for the cloud handoff routes the web uses (ADR-0034,
 * decisions 7, 12 and 14; contract 2.7 and 2.9):
 *
 *   GET    elitea_core/workspace_handoff/prompt_lib/{project}/{conversation}
 *   DELETE elitea_core/workspace_handoff/prompt_lib/{project}/{conversation}
 *   POST   elitea_core/workspace_handoff_reclaim_request/prompt_lib/{project}/{conversation}
 *
 * TEMPORARY: these routes are added to v2.yaml by the server unit, which
 * lands in a parallel branch. When the generated client has them, delete
 * this file and point `pages/chat/CloudExecutorNotice.tsx` at it; the three
 * functions below are the whole surface it uses.
 *
 * `eliteaFetch` resolves to the ENVELOPE `{data, status, headers}` and casts
 * it to its type argument, so every call here is typed `{ data: T }` and
 * returns `.data` through one helper (the #132 defect shape).
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';

async function fetchData<T>(url: string, options?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(url, options);
  return envelope.data;
}

/** The thread's states, as the banner and the live frames carry them. */
/**
 * The Files changed read of one thread (pages/chat; the handoff live events invalidate it): a frame (a cloud turn
 * starting or ending, a take back) re-reads it, so the count follows the
 * turns without the dialog open.
 */
export const handoffChangesKey = (projectId: string, conversationUuid: string) =>
  ['workspace-handoff-changes', projectId, conversationUuid.toLowerCase()] as const;

export const WORKSPACE_HANDOFF_STATES = ['LOCAL', 'HANDING_OFF', 'CLOUD_IDLE', 'CLOUD_RUNNING', 'RECLAIMING'] as const;
export type WorkspaceHandoffState = (typeof WORKSPACE_HANDOFF_STATES)[number];

/** The banner state (contract 2.9). No row means `{state: 'LOCAL', epoch: 0}`. */
export interface WorkspaceHandoffInfo {
  readonly state: WorkspaceHandoffState;
  readonly epoch: number;
  readonly reclaim_pending?: boolean;
  readonly device_name?: string;
  readonly history_mode?: string;
  /** When the cloud copy is erased on its own (after a take back, or after idling too long). */
  readonly delete_after?: string | null;
  readonly updated_at?: string;
  /** Contract 1.29: the GitHub or GitLab configuration the cloud pushes and opens pull requests through (a reference, never a secret). */
  readonly pull_request_credential?: { readonly type: 'github' | 'gitlab'; readonly elitea_title: string; readonly private: boolean } | null;
}

const handoffUrl = (projectId: string | number, conversationUuid: string): string =>
  `/elitea_core/workspace_handoff/prompt_lib/${String(projectId)}/${encodeURIComponent(conversationUuid)}`;

export function getWorkspaceHandoff(projectId: string | number, conversationUuid: string): Promise<WorkspaceHandoffInfo> {
  return fetchData<WorkspaceHandoffInfo>(handoffUrl(projectId, conversationUuid));
}

/** Owner, or `models.project_settings.edit`. Cancels a running cloud turn; the thread goes back to `LOCAL`. */
export function eraseWorkspaceHandoff(projectId: string | number, conversationUuid: string): Promise<{ state: WorkspaceHandoffState; epoch: number }> {
  return fetchData<{ state: WorkspaceHandoffState; epoch: number }>(handoffUrl(projectId, conversationUuid), { method: 'DELETE' });
}

/** Owner only. Notifies the owner's desktop (`workspace_reclaim_requested`); the thread's state does not change. */
export function requestWorkspaceReclaim(projectId: string | number, conversationUuid: string): Promise<unknown> {
  return fetchData<unknown>(
    `/elitea_core/workspace_handoff_reclaim_request/prompt_lib/${String(projectId)}/${encodeURIComponent(conversationUuid)}`,
    { method: 'POST', body: JSON.stringify({}), headers: { 'Content-Type': 'application/json' } },
  );
}
