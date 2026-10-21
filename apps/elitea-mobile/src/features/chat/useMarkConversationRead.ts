import { useEffect, useRef } from 'react';
import type { AppStateStatus } from 'react-native';

import { runMarkConversationRead, type MarkReadDeps } from '@/api/queries/markRead';
import { queryClient as appQueryClient } from '@/api/queryClient';
import { offlineStore } from '@/db/offlineStore';
import type { ChatMessage } from '@/features/chat/messageModel';
import { appActivity as realAppActivity } from '@/lib/appActivity';
import { network as realNetwork } from '@/lib/network';

/** How long the thread settles (opening, a burst of answers) before its read is sent. */
export const MARK_READ_SETTLE_MS = 600;

/**
 * A read the workspace answered `unread: true` (the read landed before the answer was stored)
 * is sent again after this long while the thread is still on screen, at most
 * `MARK_READ_RETRY_LIMIT` times per open, new answer or return to the foreground.
 */
export const MARK_READ_RETRY_MS = 1500;
export const MARK_READ_RETRY_LIMIT = 2;

/**
 * What `revision` is made of: the newest message (its key, time, whether it still streams or
 * failed) and whether the live turn settled. A count alone misses an answer that replaces
 * another (a capped window, a regeneration).
 */
export function readRevision(
  messages: readonly Pick<ChatMessage, 'key' | 'createdAt' | 'isStreaming' | 'error'>[],
  settled: boolean,
): string {
  const newest = messages[messages.length - 1];
  if (newest === undefined) return `none:${settled}`;
  return `${newest.key}:${newest.createdAt ?? ''}:${newest.isStreaming}:${newest.error !== null}:${settled}`;
}

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
 * read asked for meanwhile runs after it, so a new answer is never left unread. A read the
 * workspace answers `unread: true` is retried (`MARK_READ_RETRY_MS`); one it refuses for good
 * (403, 404, 426, ...) is never sent again for that conversation while the screen lives.
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
  /** Conversations whose read the workspace refused for good (`project:conversation`). */
  const refused = useRef(new Set<string>());

  useEffect(() => {
    if (!enabled || projectId === null || conversationId === null || !Number.isSafeInteger(conversationId) || conversationId <= 0) return undefined;
    const key = `${projectId}:${conversationId}`;
    let live = true;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    let retries = 0;
    const send = () => {
      if (refused.current.has(key)) return;
      if (environment.appActivity.currentState !== 'active' || !environment.network.isOnline()) return;
      if (inFlight.current) {
        again.current = true;
        return;
      }
      inFlight.current = true;
      void runMarkConversationRead(projectId, conversationId, environment.deps)
        .then((outcome) => {
          if (outcome.kind === 'refused') refused.current.add(key);
          else if (outcome.kind === 'read' && outcome.unread && live && retries < MARK_READ_RETRY_LIMIT) {
            // Still on screen, and the answer was stored after the read: read it again shortly.
            retries += 1;
            if (retryTimer !== null) clearTimeout(retryTimer);
            retryTimer = setTimeout(() => {
              retryTimer = null;
              send();
            }, MARK_READ_RETRY_MS);
          }
        })
        .finally(() => {
          inFlight.current = false;
          if (again.current) {
            again.current = false;
            // The latest effect's read: the open conversation may have changed meanwhile.
            latest.current?.();
          }
        });
    };
    const schedule = () => {
      retries = 0;
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
      live = false;
      if (timer !== null) clearTimeout(timer);
      if (retryTimer !== null) clearTimeout(retryTimer);
      if (latest.current === send) latest.current = null;
      sub.remove();
    };
  }, [enabled, projectId, conversationId, revision, environment]);
}
