import { useEffect, useState } from 'react';

import { speaksContract } from '@/api/discovery';
import type { ConversationSummary } from '@/api/queries/conversations';
import { offlineStore } from '@/db/offlineStore';
import { readPinnedConversationRows, searchConversations, type SearchHit } from '@/db/search';
import { log } from '@/lib/log';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { conversationOpsDrainer } from '@/sync/appConversationOps';
import { subscribeSyncChanges } from '@/sync/appSync';

/** Rename and pin need client contract 1.4 (the rename route and `is_pinned` in rows and deltas). */
export function useListActionsAvailable(): boolean {
  const workspace = useWorkspaceInfo();
  return speaksContract(workspace?.discovery, 1, 4);
}

/** Re-runs `read` when the store opens or closes, a sync changed the project, or the action queue moved. */
function useStoreRead<T>(projectId: number | null, initial: T, read: (projectId: number) => Promise<T> | null, deps: readonly unknown[]): T {
  const [value, setValue] = useState<T>(initial);
  useEffect(() => {
    let cancelled = false;
    let seq = 0;
    const load = () => {
      const mine = ++seq;
      const pending = projectId === null ? null : read(projectId);
      if (pending === null) {
        setValue(initial);
        return;
      }
      pending
        .then((next) => {
          if (!cancelled && mine === seq) setValue(next);
        })
        .catch((error: unknown) => log.warn('offline list read failed', { error }));
    };
    load();
    const subs = [
      offlineStore.subscribe(load),
      subscribeSyncChanges((changed) => {
        if (changed === projectId) load();
      }),
      conversationOpsDrainer.subscribe(load),
    ];
    return () => {
      cancelled = true;
      for (const unsubscribe of subs) unsubscribe();
    };
    // `read` and `initial` are recreated each render; the deps say when they change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectId, ...deps]);
  return value;
}

const NO_ROWS: ConversationSummary[] = [];

/** The saved copy's pinned conversations (the Pinned section is complete even when the pages are not). */
export function useSavedPinnedRows(projectId: number | null, enabled: boolean): ConversationSummary[] {
  return useStoreRead(
    enabled ? projectId : null,
    NO_ROWS,
    (id) => {
      const db = offlineStore.get();
      return db === null ? null : readPinnedConversationRows(db, id);
    },
    [enabled],
  );
}

/** How long typing settles before the index is asked. */
export const SEARCH_SETTLE_MS = 150;

/**
 * Offline full-text search (src/db/search.ts) for the query; null when there is no index to
 * ask (the store is closed: offline storage off, or locked), so the screen falls back to the
 * title filter. Also null for a blank query.
 */
export function useConversationSearch(projectId: number | null, query: string): SearchHit[] | null {
  const [settled, setSettled] = useState(query);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(query), SEARCH_SETTLE_MS);
    return () => clearTimeout(timer);
  }, [query]);
  return useStoreRead<SearchHit[] | null>(
    settled.trim() === '' ? null : projectId,
    null,
    (id) => {
      const db = offlineStore.get();
      return db === null ? null : searchConversations(db, id, settled);
    },
    [settled],
  );
}
