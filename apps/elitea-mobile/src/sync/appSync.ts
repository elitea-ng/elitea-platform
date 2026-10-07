import type { AppStateStatus } from 'react-native';

import { saveConversationDetail, type ConversationDetail } from '@/db/repo';
import { offlineStore, type OfflineStore } from '@/db/offlineStore';
import { appActivity } from '@/lib/appActivity';
import { log } from '@/lib/log';
import { getSession, subscribeSession, type SessionState } from '@/state/session';
import { SyncManager } from '@/sync/syncManager';

const changeListeners = new Set<(projectId: number) => void>();

/**
 * Told after a sync run changed stored data of a project (the widget
 * snapshot is rewritten after sync, src/features/surfaces/).
 */
export function subscribeSyncChanges(listener: (projectId: number) => void): () => void {
  changeListeners.add(listener);
  return () => {
    changeListeners.delete(listener);
  };
}

/** The app's sync manager, over the app's offline store. */
export const syncManager = new SyncManager({
  getDb: () => offlineStore.get(),
  onChanged: (projectId) => {
    for (const listener of [...changeListeners]) {
      try {
        listener(projectId);
      } catch (error) {
        log.warn('sync change listener failed', { error });
      }
    }
  },
});

export interface SyncBindingDeps {
  store: Pick<OfflineStore, 'subscribe' | 'status'>;
  session: { get(): SessionState; subscribe(listener: () => void): () => void };
  activity: { addEventListener(type: 'change', listener: (state: AppStateStatus) => void): { remove(): void } };
  manager: Pick<SyncManager, 'requestSync'>;
}

/**
 * When to sync the selected project: as soon as the store opens (after
 * unlock), when the selected project changes, and when the app comes back to
 * the foreground. (Connectivity regained is added by the outbox wiring; a
 * successful list read also asks for one.)
 */
export function bindSync(deps: SyncBindingDeps): () => void {
  let wasOpen = deps.store.status === 'open';
  let project = deps.session.get().projectId;
  const request = () => deps.manager.requestSync(deps.session.get().projectId);
  const subs = [
    deps.store.subscribe(() => {
      const open = deps.store.status === 'open';
      if (open && !wasOpen) request();
      wasOpen = open;
    }),
    deps.session.subscribe(() => {
      const next = deps.session.get().projectId;
      if (next !== project) {
        project = next;
        request();
      }
    }),
  ];
  const activity = deps.activity.addEventListener('change', (state) => {
    if (state === 'active') request();
  });
  if (wasOpen) request();
  return () => {
    for (const unsubscribe of subs) unsubscribe();
    activity.remove();
  };
}

let started = false;

export function startSync(): void {
  if (started) return;
  started = true;
  bindSync({
    store: offlineStore,
    session: { get: getSession, subscribe: subscribeSession },
    activity: appActivity,
    manager: syncManager,
  });
}

/**
 * A conversation was read from the network: keep that read (participants,
 * uuid) for offline use and bring its transcript up to date. No-op while the
 * store is closed.
 */
export function rememberConversation(projectId: number, detail: ConversationDetail): void {
  const db = offlineStore.get();
  if (db === null) return;
  saveConversationDetail(db, projectId, detail)
    .then(() => {
      const id = Number(detail.id);
      if (Number.isSafeInteger(id) && id > 0) return syncManager.syncTranscript(projectId, id);
    })
    .catch((error: unknown) => log.warn('conversation not kept offline', { error }));
}
