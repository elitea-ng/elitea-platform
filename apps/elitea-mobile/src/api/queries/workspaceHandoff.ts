import { useQuery } from '@tanstack/react-query';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import type { components } from '@/api/generated/elitea.gen';
import { queryKeys } from '@/api/queries/keys';
import { useHasProjectPermission } from '@/api/queries/permissions';
import { PROJECT_SETTINGS_EDIT } from '@/features/localWork/model';

/**
 * The cloud handoff of a Local work thread (ADR-0034, client contract 1.8), as the web app's
 * Cloud executor notice reads and writes it:
 * - `getWorkspaceHandoff`: the banner state (`LOCAL` when the thread was never sent);
 * - `requestWorkspaceHandoffReclaim` ("Ask laptop to take back"): the owner only; it notifies the
 *   owner's desktop and changes no state;
 * - `eraseWorkspaceHandoff` ("Erase cloud copy"): the owner or a project settings editor; it
 *   stops a running cloud turn and the thread goes back to the laptop.
 * - `setWorkspaceHandoffPullRequestCredential` (client contract 1.29): the owner chooses the
 *   GitHub or GitLab configuration the cloud agent pushes and opens pull requests through; only
 *   the reference is sent, never a secret. `listConfigurations` lists the choices.
 */

export type HandoffInfo = components['schemas']['WorkspaceHandoffState'];
export type ForgeCredential = components['schemas']['WorkspaceForgeCredential'];
export type ForgeType = ForgeCredential['type'];

/** The forges a cloud thread pushes to, in the order the picker lists them. */
export const FORGES: readonly ForgeType[] = ['github', 'gitlab'];

/** How often an open Local work thread asks for its state (the web page's interval). */
export const HANDOFF_POLL_MS = 10_000;

/** The route's path parameters (the thread's uuid, lowercase canonical). */
function handoffPath(projectId: number, conversationUuid: string) {
  return { params: { path: { project_id: String(projectId), conversation_uuid: conversationUuid.toLowerCase() } } };
}

/**
 * Which of two handoff reads to keep. A read that left before a newer one landed never moves the
 * thread backwards (higher epoch wins) - but `LOCAL` is authoritative: a thread with no handoff
 * (after an erase or a finished take back) answers `{LOCAL, epoch 0}`, a lower epoch than the
 * cloud state it replaces.
 */
export function mergeHandoff(previous: HandoffInfo | undefined, next: HandoffInfo): HandoffInfo {
  if (previous === undefined || next.state === 'LOCAL') return next;
  return previous.epoch > next.epoch ? previous : next;
}

export function fetchHandoff(projectId: number, conversationUuid: string, client: ApiClient = sessionApiClient(), signal?: AbortSignal): Promise<HandoffInfo> {
  return unwrap(
    client.GET('/elitea_core/workspace_handoff/prompt_lib/{project_id}/{conversation_uuid}', {
      ...handoffPath(projectId, conversationUuid),
      signal,
    }),
  );
}

export async function requestReclaim(projectId: number, conversationUuid: string, client: ApiClient = sessionApiClient()): Promise<void> {
  await unwrap(
    client.POST('/elitea_core/workspace_handoff_reclaim_request/prompt_lib/{project_id}/{conversation_uuid}', {
      ...handoffPath(projectId, conversationUuid),
    }),
  );
}

export async function eraseCloudCopy(projectId: number, conversationUuid: string, client: ApiClient = sessionApiClient()): Promise<void> {
  await unwrap(
    client.DELETE('/elitea_core/workspace_handoff/prompt_lib/{project_id}/{conversation_uuid}', {
      ...handoffPath(projectId, conversationUuid),
    }),
  );
}

export async function setPullRequestCredential(
  projectId: number,
  conversationUuid: string,
  credential: ForgeCredential | null,
  client: ApiClient = sessionApiClient(),
): Promise<void> {
  await unwrap(
    client.PUT('/elitea_core/workspace_handoff_pull_request_credential/prompt_lib/{project_id}/{conversation_uuid}', {
      ...handoffPath(projectId, conversationUuid),
      body: { credential },
    }),
  );
}

/** The titles of one project's configurations of one forge (with the shared ones when asked). */
export async function listForgeTitles(
  projectId: number,
  type: ForgeType,
  includeShared: boolean,
  client: ApiClient = sessionApiClient(),
  signal?: AbortSignal,
): Promise<{ own: string[]; shared: string[] }> {
  const page = await unwrap(
    client.GET('/configurations/configurations/{project_id}', {
      params: { path: { project_id: String(projectId) }, query: { type: [type], include_shared: includeShared, limit: 200 } },
      signal,
    }),
  );
  const titles = (rows: readonly { elitea_title?: string }[] | undefined) => (rows ?? []).map((row) => row.elitea_title ?? '').filter((title) => title !== '');
  return { own: titles(page.items), shared: titles(page.shared?.items) };
}

/**
 * The thread's handoff state, re-read every `HANDOFF_POLL_MS` while the screen is open.
 * A read that left before a newer one landed never moves the thread backwards (the web's epoch
 * rule). `enabled` is false for an ordinary chat, an offline copy and a server older than 1.8.
 */
export function useHandoffInfo(projectId: number | null, conversationUuid: string | null, enabled: boolean) {
  const on = enabled && projectId !== null && conversationUuid !== null;
  return useQuery({
    queryKey: queryKeys.handoff(projectId ?? 0, conversationUuid ?? ''),
    enabled: on,
    retry: false,
    refetchInterval: on ? HANDOFF_POLL_MS : false,
    queryFn: ({ signal }) => fetchHandoff(projectId as number, conversationUuid as string, sessionApiClient(), signal),
    // The previous answer is kept while a read fails; the newer epoch wins, LOCAL always.
    structuralSharing: (previous, next) => mergeHandoff(previous as HandoffInfo | undefined, next as HandoffInfo),
  });
}

/** True when the caller may edit the project's settings (`models.project_settings.edit`); false while unknown or unreadable. */
export function usePermissionToEditProjectSettings(projectId: number | null, enabled: boolean): boolean {
  return useHasProjectPermission(projectId, PROJECT_SETTINGS_EDIT, enabled);
}
