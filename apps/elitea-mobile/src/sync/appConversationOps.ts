import type { InfiniteData, QueryClient } from '@tanstack/react-query';
import { useEffect, useMemo, useState } from 'react';

import { sendConversationAction, type ConversationAction } from '@/api/queries/conversationActions';
import type { ConversationPage } from '@/api/queries/conversations';
import { queryKeys } from '@/api/queries/keys';
import { queryClient } from '@/api/queryClient';
import { offlineStore } from '@/db/offlineStore';
import { appActivity } from '@/lib/appActivity';
import { log } from '@/lib/log';
import { network } from '@/lib/network';
import { getSession, subscribeSession } from '@/state/session';
import { bindOutbox } from '@/sync/appOutbox';
import { syncManager } from '@/sync/appSync';
import { ConversationOpsDrainer, enqueueOp, listOps, overlayOf, type ConversationOp, type ConversationOverlay } from '@/sync/conversationOps';

/**
 * The app's Chats-list actions (rename, pin, delete): queued in the encrypted store and
 * drained when the workspace can take them (src/sync/conversationOps.ts). With no store
 * (offline storage off by policy, or not unlocked yet) an action is sent at once and
 * reports failure to the screen instead.
 */

/** Patches the loaded list pages so a taken action shows before the refetch lands. */
export function patchConversationPages(client: QueryClient, projectId: number, conversationId: number, action: ConversationAction): void {
  client.setQueryData<InfiniteData<ConversationPage, number>>(queryKeys.conversations(projectId), (data) => {
    if (!data) return data;
    return {
      ...data,
      pages: data.pages.map((page) => ({
        ...page,
        rows:
          action.kind === 'delete'
            ? page.rows.filter((row) => row.id !== conversationId)
            : page.rows.map((row) =>
                row.id !== conversationId ? row : action.kind === 'rename' ? { ...row, name: action.name } : { ...row, is_pinned: action.pinned },
              ),
      })),
    };
  });
}

const failureListeners = new Set<(op: Pick<ConversationOp, 'projectId' | 'conversationId' | 'action'>) => void>();

/** Told when the workspace refused an action (the list goes back to what it says). */
export function subscribeActionFailures(listener: (op: Pick<ConversationOp, 'projectId' | 'conversationId' | 'action'>) => void): () => void {
  failureListeners.add(listener);
  return () => {
    failureListeners.delete(listener);
  };
}

function reportFailure(op: Pick<ConversationOp, 'projectId' | 'conversationId' | 'action'>): void {
  for (const listener of [...failureListeners]) {
    try {
      listener(op);
    } catch (error) {
      log.warn('action failure listener failed', { error });
    }
  }
}

export const conversationOpsDrainer = new ConversationOpsDrainer({
  getDb: () => offlineStore.get(),
  send: (op) => sendConversationAction(op.projectId, op.conversationId, op.action),
  isOnline: () => network.isOnline(),
  isBlocked: () => getSession().upgradeRequired !== null,
  schedule: (delayMs, wake) => {
    const timer = setTimeout(wake, delayMs);
    return () => clearTimeout(timer);
  },
  onSettled: (op, outcome) => {
    if (outcome === 'rejected') reportFailure(op);
    // A rename or pin of a conversation that is gone: it leaves the list (it was not renamed).
    else if (outcome === 'gone') patchConversationPages(queryClient, op.projectId, op.conversationId, { kind: 'delete' });
    else patchConversationPages(queryClient, op.projectId, op.conversationId, op.action);
    void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(op.projectId) });
    if (op.action.kind !== 'pin') void queryClient.invalidateQueries({ queryKey: [...queryKeys.projectRoot(op.projectId), 'conversation'] });
    syncManager.requestSync(op.projectId);
  },
});

/** Actions sent directly (no store), shown as pending until they settle. */
const inFlight = new Map<number, ConversationOp[]>();
const inFlightListeners = new Set<() => void>();
const notifyInFlight = () => {
  for (const listener of [...inFlightListeners]) listener();
};

export type ActionResult = 'queued' | 'done' | 'failed';

/**
 * Runs a list action: queued (and drained right away) when the store is open, else sent now.
 * `queued` means the device has it and the list already shows it.
 */
export async function runConversationAction(projectId: number, conversationId: number, action: ConversationAction): Promise<ActionResult> {
  const db = offlineStore.get();
  if (db !== null) {
    await enqueueOp(db, projectId, conversationId, action);
    const shown = conversationOpsDrainer.notify();
    conversationOpsDrainer.drain().catch((error: unknown) => log.warn('conversation ops drain failed', { error }));
    // Resolve once the list overlay shows it (the screen keeps the row hidden until then).
    await shown;
    return 'queued';
  }
  const op: ConversationOp = { projectId, conversationId, action, attempts: 0, nextAttemptMs: 0, createdMs: Date.now() };
  inFlight.set(projectId, [...(inFlight.get(projectId) ?? []), op]);
  notifyInFlight();
  try {
    const result = await sendConversationAction(projectId, conversationId, action);
    const ok = result.outcome === 'done' || (result.outcome === 'gone' && action.kind !== 'rename' && action.kind !== 'pin');
    if (ok) patchConversationPages(queryClient, projectId, conversationId, action);
    // Same as the queue: a rename or pin of a conversation that is gone takes it off the list.
    else if (result.outcome === 'gone') patchConversationPages(queryClient, projectId, conversationId, { kind: 'delete' });
    void queryClient.invalidateQueries({ queryKey: queryKeys.conversations(projectId) });
    return ok ? 'done' : 'failed';
  } finally {
    inFlight.set(
      projectId,
      (inFlight.get(projectId) ?? []).filter((o) => o !== op),
    );
    notifyInFlight();
  }
}

/** The queued and in-flight state of a project's conversations, live. */
export function useConversationOverlay(projectId: number | null): Map<number, ConversationOverlay> {
  const [queued, setQueued] = useState<ConversationOp[]>([]);
  const [direct, setDirect] = useState<ConversationOp[]>([]);
  useEffect(() => {
    let cancelled = false;
    const load = (): Promise<void> => {
      const db = offlineStore.get();
      if (db === null || projectId === null) {
        setQueued([]);
        return Promise.resolve();
      }
      return listOps(db, projectId)
        .then((ops) => {
          if (!cancelled) setQueued(ops);
        })
        .catch(() => {});
    };
    const loadDirect = () => setDirect(projectId === null ? [] : [...(inFlight.get(projectId) ?? [])]);
    void load();
    loadDirect();
    const unsubscribeOps = conversationOpsDrainer.subscribe(load);
    const unsubscribeStore = offlineStore.subscribe(load);
    inFlightListeners.add(loadDirect);
    return () => {
      cancelled = true;
      unsubscribeOps();
      unsubscribeStore();
      inFlightListeners.delete(loadDirect);
    };
  }, [projectId]);
  return useMemo(() => overlayOf([...queued, ...direct]), [queued, direct]);
}

let started = false;

/** Drains the action queue on reconnect, when the store opens, on foreground and when the update screen clears. */
export function startConversationOps(): void {
  if (started) return;
  started = true;
  bindOutbox({
    store: offlineStore,
    network,
    activity: appActivity,
    upgrade: { blocked: () => getSession().upgradeRequired !== null, subscribe: subscribeSession },
    drain: () => {
      conversationOpsDrainer.drain().catch((error: unknown) => log.warn('conversation ops drain failed', { error }));
    },
  });
}
