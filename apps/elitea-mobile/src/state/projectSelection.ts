import { queryKeys } from '@/api/queries/keys';
import { queryClient } from '@/api/queryClient';
import { abortScope, projectScope } from '@/lib/abortRegistry';
import { log } from '@/lib/log';
import { SECURE_KEYS, setSecureItem } from '@/security/secureStorage';
import { getSession, setSession } from '@/state/session';

/**
 * Makes `projectId` the selected project. Switching away from a project stops
 * its streams (abort scope) and drops its cached data, so nothing from the old
 * project can render or arrive late into the new one. The choice is persisted
 * in secure storage and restored at launch (`hydrateSession`).
 */
export async function selectProject(projectId: number): Promise<void> {
  if (!Number.isSafeInteger(projectId) || projectId <= 0) throw new Error('invalid project id');
  const previous = getSession().projectId;
  if (previous === projectId) return;
  setSession({ projectId });
  if (previous !== null) {
    abortScope(projectScope(previous), 'project_switched');
    await queryClient.cancelQueries({ queryKey: queryKeys.projectRoot(previous) });
    queryClient.removeQueries({ queryKey: queryKeys.projectRoot(previous) });
  }
  try {
    await setSecureItem(SECURE_KEYS.projectSelected, String(projectId));
  } catch (error) {
    log.warn('could not persist the selected project', { error });
  }
}
