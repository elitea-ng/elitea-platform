import { Platform } from 'react-native';

import { queryClient } from '@/api/queryClient';
import { listLiveTurns, subscribeLiveTurns } from '@/features/chat/turnController';
import { TurnActivityController, type ActivityFactory } from '@/features/surfaces/turnActivityController';
import { activityTitlePreview } from '@/features/surfaces/turnActivityModel';
import { appActivity } from '@/lib/appActivity';
import { log } from '@/lib/log';
import { registerWipeHook } from '@/security/endSession';
import { getPolicy, subscribePolicy } from '@/security/policy';
import { getSession } from '@/state/session';

/**
 * The app's Live Activity controller (src/features/surfaces/turnActivityController.ts),
 * over the live turns, the app's activity (a locked app counts as
 * background), the workspace's notification preview (titles also need allow_system_surfaces) and the active
 * workspace. Started once at launch; a wipe or a switch ends every activity.
 */

function nativeFactory(): ActivityFactory | null {
  if (Platform.OS !== 'ios') return null;
  try {
    // Lazy, iOS only: there is no Live Activity module on Android or web.
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    return (require('@/features/surfaces/TurnActivity') as typeof import('@/features/surfaces/TurnActivity')).turnActivity;
  } catch (error) {
    log.warn('live activities unavailable', { error });
    return null;
  }
}

/** The conversation's title from the query cache (`<projectId>:<uuid>`), or null. */
export function cachedConversationTitle(key: string): string | null {
  const split = key.indexOf(':');
  const projectId = Number(key.slice(0, split));
  const uuid = key.slice(split + 1).toLowerCase();
  if (!Number.isSafeInteger(projectId)) return null;
  for (const [, data] of queryClient.getQueriesData<{ uuid?: unknown; name?: unknown }>({ queryKey: ['p', projectId, 'conversation'] })) {
    if (data && typeof data.uuid === 'string' && data.uuid.toLowerCase() === uuid && typeof data.name === 'string') return data.name;
  }
  return null;
}

let controller: TurnActivityController | null = null;

export function startTurnActivities(): void {
  if (controller) return;
  controller = new TurnActivityController({
    factory: nativeFactory(),
    turns: { list: listLiveTurns, subscribe: subscribeLiveTurns },
    appState: {
      current: () => appActivity.currentState,
      subscribe: (listener) => {
        const sub = appActivity.addEventListener('change', listener);
        return () => sub.remove();
      },
    },
    preview: { get: () => activityTitlePreview(getPolicy()), subscribe: subscribePolicy },
    workspaceId: () => getSession().workspaceId,
    titleFor: cachedConversationTitle,
  });
  controller.start();
}

// Nothing of a workspace outlives its session or a switch on the lock screen.
registerWipeHook('live_activities', () => controller?.endAll());
