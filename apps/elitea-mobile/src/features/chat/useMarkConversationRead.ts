import { useEffect, useRef } from 'react';
import type { AppStateStatus } from 'react-native';

import { runMarkConversationRead, type MarkReadDeps } from '@/api/queries/markRead';
import { queryClient as appQueryClient } from '@/api/queryClient';
import { offlineStore } from '@/db/offlineStore';
import { appActivity as realAppActivity } from '@/lib/appActivity';
import { network as realNetwork } from '@/lib/network';

/** How long the thread settles (opening, a burst of answers) before its read is sent. */
export const MARK_READ_SETTLE_MS = 600;

export interface MarkReadEnvironment {
  appActivity: { readonly currentState: AppStateStatus; addEventListener(type: 'change', listener: (state: AppStateStatus) => void): { remove(): void } };
  network: { isOnline(): boolean };
  deps: MarkReadDeps;
}

const APP_ENVIRONMENT: MarkReadEnvironment = {
  appActivity: realAppActivity,
  network: realNetwork,
  deps: { queryClient: appQueryClient, getDb: () => offlineStore.get() },
};

/**
 * Marks the open conversation read (client contract 1.12) while the user can see it, as the web
 * and the desktop do: when it opens, when the app comes back to the foreground with it open, and
 * when `revision` changes (a new answer, a turn that finished) while it is on screen. Each read
 * waits `MARK_READ_SETTLE_MS` so a burst sends one request; one request runs at a time, and a
 * read asked for meanwhile runs after it, so a new answer is never left unread.
 *
 * Off (`enabled` false) on a server before 1.12, for the offline copy, and while another screen
 * covers the thread. Offline or in the background nothing is sent.
 */
export function useMarkConversationRead({
  projectId,
  conversationId,
  revision,
  enabled,
  environment = APP_ENVIRONMENT,
}: {
  projectId: number | null;
  /** The conversation's numeric id; null while unknown. */
  conversationId: number | null;
  /** Changes when the open conversation gets a new answer. */
  revision?: unknown;
  enabled: boolean;
  environment?: MarkReadEnvironment;
}): void {
  const inFlight = useRef(false);
  const again = useRef(false);
  const latest = useRef<(() => void) | null>(null);

  useEffect(() => {
    if (!enabled || projectId === null || conversationId === null || !Number.isSafeInteger(conversationId) || conversationId <= 0) return undefined;
    let timer: ReturnType<typeof setTimeout> | null = null;
    const send = () => {
      if (environment.appActivity.currentState !== 'active' || !environment.network.isOnline()) return;
      if (inFlight.current) {
        again.current = true;
        return;
      }
      inFlight.current = true;
      void runMarkConversationRead(projectId, conversationId, environment.deps).finally(() => {
        inFlight.current = false;
        if (again.current) {
          again.current = false;
          // The latest effect's read: the open conversation may have changed meanwhile.
          latest.current?.();
        }
      });
    };
    const schedule = () => {
      if (timer !== null) clearTimeout(timer);
      timer = setTimeout(() => {
        timer = null;
        send();
      }, MARK_READ_SETTLE_MS);
    };
    latest.current = send;
    schedule();
    const sub = environment.appActivity.addEventListener('change', (state) => {
      if (state === 'active') schedule();
    });
    return () => {
      if (timer !== null) clearTimeout(timer);
      if (latest.current === send) latest.current = null;
      sub.remove();
    };
  }, [enabled, projectId, conversationId, revision, environment]);
}
