import { useQuery } from '@tanstack/react-query';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { unwrap } from '@/api/errors';
import { queryKeys } from '@/api/queries/keys';

/**
 * The caller's permissions in a project (`permissionList`), as the web app's `useHasPermission`
 * reads them: the names of the enabled entries. One read per project, shared by every screen
 * that asks (the Local work erase, the Chats list's Archive).
 */

/** Archive and unarchive a conversation (client contract 1.10): the permission the web app checks. */
export const CHAT_ARCHIVE_PERMISSION = 'models.chat.conversation.edit';

export async function fetchProjectPermissions(projectId: number, client: ApiClient = sessionApiClient(), signal?: AbortSignal): Promise<string[]> {
  const list = await unwrap(client.GET('/auth/permissions/prompt_lib/{project_id}', { params: { path: { project_id: String(projectId) } }, signal }));
  if (!Array.isArray(list)) return [];
  return list.filter((entry) => entry?.enabled === true && typeof entry.name === 'string').map((entry) => entry.name as string);
}

/**
 * True when the caller holds `permission` in the project; false while unknown (not read yet,
 * offline before the first read) or unreadable, so an action that needs it stays hidden.
 * The last answer is kept in memory for the session and re-read after five minutes.
 */
export function useHasProjectPermission(projectId: number | null, permission: string, enabled = true): boolean {
  const on = enabled && projectId !== null;
  const query = useQuery({
    queryKey: queryKeys.permissions(projectId ?? 0),
    enabled: on,
    retry: false,
    staleTime: 5 * 60_000,
    queryFn: ({ signal }) => fetchProjectPermissions(projectId as number, sessionApiClient(), signal),
  });
  return on && Array.isArray(query.data) && query.data.includes(permission);
}
