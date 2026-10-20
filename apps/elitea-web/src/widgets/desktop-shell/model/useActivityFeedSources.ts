/**
 * Feeds the activity store from the host (`agent://event`, `index://event`,
 * `handoff://event`) for as long as the window lives, and reconciles what it
 * may have missed: events are not replayed, so unfinished turns are asked
 * about (`agent_turn_status`) when the window regains focus and, while a
 * commit is pending, every few seconds.
 */
import { useEffect } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { useWorkspaceIpc } from '@/features/workspace';
import { useHandoffIpc } from '@/features/workspace-handoff';
import { useIndexIpc } from '@/features/workspace-index';
import type { HandoffEvent } from '@/shared/desktop/handoffIpc';
import type { IndexEvent } from '@/shared/desktop/indexIpc';
import type { AgentEvent, Workspace } from '@/shared/desktop/workspaceIpc';

import { useActivityFeed } from './activityFeed.store';
import { useAppIpc } from './appIpcContext';
import { WORKSPACE_LIST_KEY } from './useShellActions';

const COMMIT_POLL_MS = 3_000;

/**
 * Subscribes to one channel; the returned cleanup also covers a subscription
 * that resolves late. The handler is made here, fresh per call: a host client
 * that keys its listeners by function identity (the in-memory fakes do) would
 * otherwise see one listener where React's strict mode registers two.
 */
function subscribe<E>(register: (handler: (event: E) => void) => Promise<() => void>, apply: (event: E) => void): () => void {
  let off: (() => void) | undefined;
  let disposed = false;
  register((event) => apply(event))
    .then((unsubscribe) => {
      if (disposed) unsubscribe();
      else off = unsubscribe;
    })
    .catch(() => undefined);
  return () => {
    disposed = true;
    off?.();
  };
}

export function useActivityFeedSources(): void {
  // The app (its signed-in session) going away takes what it was doing with it:
  // a sign-out reloads, but a stale turn must never outlive the identity that ran it.
  useEffect(() => () => useActivityFeed.getState().reset(), []);
  const workspaceIpc = useWorkspaceIpc();
  const indexIpc = useIndexIpc();
  const handoffIpc = useHandoffIpc();
  const queryClient = useQueryClient();
  const appIpc = useAppIpc();

  useEffect(() => {
    if (workspaceIpc === undefined) return undefined;
    return subscribe<AgentEvent>((h) => workspaceIpc.onEvent(h), (event) => useActivityFeed.getState().applyAgentEvent(event));
  }, [workspaceIpc]);

  useEffect(() => {
    if (indexIpc === undefined) return undefined;
    return subscribe<IndexEvent>((h) => indexIpc.onEvent(h), (event) => {
      const list = queryClient.getQueryData<Workspace[]>(WORKSPACE_LIST_KEY);
      useActivityFeed.getState().applyIndexEvent(event, list?.find((w) => w.id === event.workspace_id)?.name ?? null);
    });
  }, [indexIpc, queryClient]);

  useEffect(() => {
    if (handoffIpc === undefined) return undefined;
    return subscribe<HandoffEvent>((h) => handoffIpc.onEvent(h), (event) => useActivityFeed.getState().applyHandoffEvent(event));
  }, [handoffIpc]);

  // A cloud turn streams into this window (the page's chat transport) or waits on the person: the host's
  // menu-bar item and Dock badge show it too.
  useEffect(() => {
    if (appIpc === undefined) return undefined;
    let known = new Map<string, string>();
    const signature = (run: { running: boolean; waiting: string | null }): string => `${String(run.running)}|${run.waiting ?? ''}`;
    return useActivityFeed.subscribe((state) => {
      const now = new Map(Object.entries(state.cloudRuns).map(([uuid, run]) => [uuid, signature(run)]));
      for (const [uuid, sig] of now) {
        const run = state.cloudRuns[uuid];
        if (run === undefined || known.get(uuid) === sig) continue;
        void appIpc
          .cloudRun({ conversationUuid: uuid, running: run.running, waiting: run.waiting !== null, ...(run.workspaceId === null ? {} : { workspaceId: run.workspaceId }), workspaceName: run.title })
          .catch(() => undefined);
      }
      for (const uuid of known.keys()) {
        if (!now.has(uuid)) void appIpc.cloudRun({ conversationUuid: uuid, running: false, waiting: false }).catch(() => undefined);
      }
      known = now;
    });
  }, [appIpc]);

  // What a missed event left behind.
  useEffect(() => {
    if (workspaceIpc === undefined) return undefined;
    const reconcile = (only?: 'pending'): void => {
      for (const turn of Object.values(useActivityFeed.getState().turns)) {
        const open = !turn.finished && (only === undefined || turn.commit === 'pending');
        if (!open) continue;
        workspaceIpc.turnStatus(turn.turnId).then(
          (status) => useActivityFeed.getState().reconcileTurn(turn.turnId, status),
          // The host forgot it (an app restart forgets turns).
          () => useActivityFeed.getState().reconcileTurn(turn.turnId, null),
        );
      }
    };
    const onFocus = (): void => reconcile();
    const onVisible = (): void => {
      if (document.visibilityState === 'visible') reconcile();
    };
    window.addEventListener('focus', onFocus);
    document.addEventListener('visibilitychange', onVisible);
    const timer = window.setInterval(() => reconcile('pending'), COMMIT_POLL_MS);
    return () => {
      window.removeEventListener('focus', onFocus);
      document.removeEventListener('visibilitychange', onVisible);
      window.clearInterval(timer);
    };
  }, [workspaceIpc]);
}
