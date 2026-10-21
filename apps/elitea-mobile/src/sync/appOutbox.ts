import { useEffect, useState } from 'react';
import type { AppStateStatus } from 'react-native';

import { queryKeys } from '@/api/queries/keys';
import type { PreparedTurn } from '@/api/queries/sendMessage';
import { queryClient } from '@/api/queryClient';
import { offlineStore, type OfflineStore } from '@/db/offlineStore';
import { appActivity } from '@/lib/appActivity';
import { log } from '@/lib/log';
import { network } from '@/lib/network';
import { getSession, subscribeSession } from '@/state/session';
import { syncManager } from '@/sync/appSync';
import { OutboxDrainer, enqueue, hasUnsent, listOutbox, type OutboxItem } from '@/sync/outbox';

/** The app's outbox drainer, over the app's offline store. */
export const outboxDrainer = new OutboxDrainer({
  getDb: () => offlineStore.get(),
  onSent: (item) => {
    // The admitted turn is in the transcript now; its answer follows there.
    void queryClient.invalidateQueries({ queryKey: [...queryKeys.projectRoot(item.projectId), 'conversation'] });
    void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(item.projectId) });
    syncManager.requestSync(item.projectId);
  },
  schedule: (delayMs, wake) => {
    const timer = setTimeout(wake, delayMs);
    return () => clearTimeout(timer);
  },
});

/** True when a send should go to the outbox: the device is offline and the store is open. */
export function shouldQueue(): boolean {
  return !network.isOnline() && offlineStore.get() !== null;
}

/**
 * True when a send to this conversation must queue behind messages the outbox
 * has not sent yet (strict order): a live send would otherwise overtake them.
 */
export async function mustQueueBehindOutbox(projectId: number, conversationUuid: string): Promise<boolean> {
  const db = offlineStore.get();
  if (!db) return false;
  return hasUnsent(db, projectId, conversationUuid);
}

/**
 * Queues a send made offline, or online behind a message the outbox has not
 * sent yet: its question_id and exact body are stored, and every later attempt
 * sends those bytes.
 */
export async function queueSend(prepared: PreparedTurn, conversationRef: string): Promise<void> {
  const db = offlineStore.get();
  if (!db) throw new Error('offline store is closed');
  await enqueue(db, prepared, conversationRef);
  outboxDrainer.notify();
  // Queued while online (behind an unsent message): the drain sends it in order.
  if (network.isOnline()) outboxDrainer.drain().catch((error: unknown) => log.warn('outbox drain failed', { error }));
}

/**
 * A live send the workspace could not take while the device is online (no
 * HTTP answer, a proxy's 502/503/504, or in-place retries used up): the outbox
 * takes it over, counted as one failed attempt, so it shows as queued and is
 * retried with backoff (or after the server's `Retry-After`) instead of
 * hanging as "Sending…" or failing for good. The same question_id and bytes
 * make the retry safe even if a lost attempt was admitted. False when offline
 * storage is off.
 */
export async function handOffToOutbox(
  prepared: PreparedTurn,
  conversationRef: string,
  hint: { retryAfterMs?: number } = {},
): Promise<boolean> {
  const db = offlineStore.get();
  if (!db) return false;
  await enqueue(db, prepared, conversationRef, Date.now(), { failedAttempts: 1, retryAfterMs: hint.retryAfterMs });
  outboxDrainer.notify();
  // Nothing is due yet; the drain schedules its wake for the end of the backoff.
  outboxDrainer.drain().catch((error: unknown) => log.warn('outbox drain failed', { error }));
  return true;
}

export interface OutboxBindingDeps {
  store: Pick<OfflineStore, 'subscribe' | 'status'>;
  network: { subscribe(listener: (online: boolean) => void): () => void };
  activity: { addEventListener(type: 'change', listener: (state: AppStateStatus) => void): { remove(): void } };
  drain: () => void;
  /**
   * The update screen (426). A held drain schedules no wake, so it drains again
   * when the screen clears.
   */
  upgrade?: { blocked(): boolean; subscribe(listener: () => void): () => void };
  /** Connectivity regained: the selected project syncs too. */
  onReconnect?: () => void;
}

/** Drains on reconnect, when the store opens (after unlock), on foreground and when the update screen clears. */
export function bindOutbox(deps: OutboxBindingDeps): () => void {
  let wasOpen = deps.store.status === 'open';
  const subs = [
    deps.network.subscribe((online) => {
      if (!online) return;
      deps.drain();
      deps.onReconnect?.();
    }),
    deps.store.subscribe(() => {
      const open = deps.store.status === 'open';
      if (open && !wasOpen) deps.drain();
      wasOpen = open;
    }),
  ];
  if (deps.upgrade) {
    const upgrade = deps.upgrade;
    let blocked = upgrade.blocked();
    subs.push(
      upgrade.subscribe(() => {
        const now = upgrade.blocked();
        if (blocked && !now) deps.drain();
        blocked = now;
      }),
    );
  }
  const activity = deps.activity.addEventListener('change', (state) => {
    if (state === 'active') deps.drain();
  });
  if (wasOpen) deps.drain();
  return () => {
    for (const unsubscribe of subs) unsubscribe();
    activity.remove();
  };
}

let started = false;

export function startOutbox(): void {
  if (started) return;
  started = true;
  bindOutbox({
    store: offlineStore,
    network,
    activity: appActivity,
    upgrade: { blocked: () => getSession().upgradeRequired !== null, subscribe: subscribeSession },
    drain: () => {
      outboxDrainer.drain().catch((error: unknown) => log.warn('outbox drain failed', { error }));
    },
    onReconnect: () => syncManager.requestSync(getSession().projectId),
  });
}

/** The thread's unsent messages, re-read whenever the outbox or the store changes. */
export function useOutboxItems(projectId: number | null, conversationUuid: string | null): OutboxItem[] {
  const [items, setItems] = useState<OutboxItem[]>([]);
  useEffect(() => {
    let cancelled = false;
    const load = () => {
      const db = offlineStore.get();
      if (!db || projectId === null || !conversationUuid) {
        setItems([]);
        return;
      }
      listOutbox(db, projectId, conversationUuid)
        .then((rows) => {
          if (!cancelled) setItems(rows);
        })
        .catch(() => {});
    };
    load();
    const unsubscribeOutbox = outboxDrainer.subscribe(load);
    const unsubscribeStore = offlineStore.subscribe(load);
    return () => {
      cancelled = true;
      unsubscribeOutbox();
      unsubscribeStore();
    };
  }, [projectId, conversationUuid]);
  return items;
}
