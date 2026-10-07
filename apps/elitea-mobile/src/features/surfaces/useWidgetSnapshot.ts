import { useEffect, useState, useSyncExternalStore } from 'react';

import { useUnseenCount } from '@/api/queries/notifications';
import { offlineStore } from '@/db/offlineStore';
import { listLiveTurns, subscribeLiveTurns, type LiveTurn } from '@/features/chat/turnController';
import { readPendingApprovals } from '@/features/surfaces/pendingApprovals';
import { buildSnapshot, type PendingConversation } from '@/features/surfaces/surfaceSnapshot';
import { clearSurfaceSnapshot, publishSurfaceSnapshot } from '@/features/surfaces/surfaceWriter';
import { log } from '@/lib/log';
import { usePolicy } from '@/security/policy';
import { subscribeSyncChanges } from '@/sync/appSync';
import { useSession } from '@/state/session';

/**
 * Keeps the widget snapshot (src/features/surfaces/surfaceSnapshot.ts) in
 * step with the active workspace's selected project: after every sync that
 * changed stored data, when the offline store opens, when the unread count
 * changes and when a live turn starts, pauses or ends. Mounted in the
 * signed-in layout; a wipe or a switch clears the snapshot (surfaceWriter).
 */

export interface LiveCounts {
  running: number;
  /** Conversation uuids whose live turn paused for the user (not the output-limit offer). */
  paused: string[];
}

/** What the live turns of `projectId` add to the counts. Keys are `<projectId>:<conversation uuid>`. */
export function liveCounts(turns: readonly [string, Pick<LiveTurn, 'turn' | 'queued' | 'settled'>][], projectId: number): LiveCounts {
  let running = 0;
  const paused: string[] = [];
  const prefix = `${projectId}:`;
  for (const [key, live] of turns) {
    if (!key.startsWith(prefix)) continue;
    const phase = live.turn.phase;
    if (!live.queued && (phase === 'sending' || phase === 'streaming')) running++;
    else if (phase === 'paused' && live.turn.pause !== null && live.turn.pause.kind !== 'output_limit') paused.push(key.slice(prefix.length));
  }
  return { running, paused };
}

/** The stored pending conversations plus those paused live (deduplicated by uuid). */
export function mergePending(stored: readonly PendingConversation[], pausedLive: readonly string[]): PendingConversation[] {
  const out = [...stored];
  const known = new Set(stored.map((p) => p.ref.toLowerCase()));
  for (const uuid of pausedLive) {
    if (!known.has(uuid.toLowerCase())) out.unshift({ ref: uuid, name: '' });
  }
  return out;
}

/** A key that changes only when what the counts depend on changes (not on every streamed frame). */
function liveKey(): string {
  return listLiveTurns()
    .map(([key, controller]) => {
      const s = controller.getSnapshot();
      return `${key}|${s.turn.phase}|${s.queued ? 1 : 0}|${s.turn.pause?.kind ?? ''}`;
    })
    .join(';');
}

function subscribeLiveKey(listener: () => void): () => void {
  let perTurn: (() => void)[] = [];
  const rewire = () => {
    for (const off of perTurn) off();
    perTurn = listLiveTurns().map(([, controller]) => controller.subscribe(listener));
  };
  rewire();
  const offRegistry = subscribeLiveTurns(() => {
    rewire();
    listener();
  });
  return () => {
    offRegistry();
    for (const off of perTurn) off();
    perTurn = [];
  };
}

export function useWidgetSnapshot(): void {
  const workspaceId = useSession((s) => s.workspaceId);
  const projectId = useSession((s) => s.projectId);
  const origin = useSession((s) => s.serverOrigin);
  const allowTitles = usePolicy().policy.allow_system_surfaces;
  const unread = useUnseenCount(projectId).data ?? 0;
  const live = useSyncExternalStore(subscribeLiveKey, liveKey, liveKey);
  const [stored, setStored] = useState<{ projectId: number; pending: PendingConversation[] } | null>(null);
  const [storeTick, setStoreTick] = useState(0);

  // Re-read the pending conversations after a sync, when the store opens or a live turn changes.
  useEffect(() => {
    const bump = () => setStoreTick((n) => n + 1);
    const offSync = subscribeSyncChanges((changed) => {
      if (changed === projectId) bump();
    });
    const offStore = offlineStore.subscribe(bump);
    return () => {
      offSync();
      offStore();
    };
  }, [projectId]);

  useEffect(() => {
    if (projectId === null) return;
    const db = offlineStore.get();
    // Store closed: the stored part is ignored below (its project is checked).
    if (db === null) return;
    let cancelled = false;
    readPendingApprovals(db, projectId)
      .then((pending) => {
        if (!cancelled) setStored({ projectId, pending });
      })
      .catch((error: unknown) => log.warn('pending approvals not read', { error }));
    return () => {
      cancelled = true;
    };
  }, [projectId, storeTick, live]);

  useEffect(() => {
    if (workspaceId === null || projectId === null) {
      clearSurfaceSnapshot();
      return;
    }
    const counts = liveCounts(
      listLiveTurns().map(([key, controller]) => [key, controller.getSnapshot()]),
      projectId,
    );
    const fromStore = stored?.projectId === projectId && offlineStore.get() !== null ? stored.pending : [];
    const pending = mergePending(fromStore, counts.paused);
    publishSurfaceSnapshot(
      workspaceId,
      buildSnapshot({ workspaceId, projectId, origin, allowTitles, approvals: pending, unread, running: counts.running, now: Date.now() }),
    );
  }, [workspaceId, projectId, origin, allowTitles, unread, live, stored]);
}
