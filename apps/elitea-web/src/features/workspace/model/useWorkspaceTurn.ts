/**
 * One live agent turn: subscribes to `agent://event`, folds it with
 * `turnReducer`, and exposes start / cancel / answer-approval.
 *
 * The subscription is made BEFORE `agent_turn_start` is invoked (the host
 * emits as soon as it has a turn), and `start` waits for it to be live.
 */
import { useCallback, useEffect, useReducer, useRef, useState } from 'react';

import type { ApprovalDecision, TurnStartRequest, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';

import { describeWorkspaceError } from './describeWorkspaceError';
import { activeView, initialTurnsState, turnReducer, type TurnView } from './turnReducer';

export interface WorkspaceTurn {
  view: TurnView;
  turnId: string | null;
  /**
   * True from `start` until the host's `done` event. Not on `error` or a
   * terminal `status`: a failed run still commits after its `error`, and the
   * host keeps the workspace until then (IPC.md: `done` is always last).
   */
  busy: boolean;
  startError: string | null;
  /** Resolves `true` once the host started the turn; `false` when it refused (see `startError`). */
  start(request: TurnStartRequest): Promise<boolean>;
  cancel(): Promise<void>;
  answer(requestId: string, decision: ApprovalDecision): Promise<void>;
}

export function useWorkspaceTurn(ipc: WorkspaceIpc): WorkspaceTurn {
  const [state, dispatch] = useReducer(turnReducer, initialTurnsState);
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState<string | null>(null);
  const ready = useRef<Promise<unknown>>(Promise.resolve());

  useEffect(() => {
    let unlisten: (() => void) | undefined;
    let disposed = false;
    const subscribed = ipc.onEvent((event) => dispatch({ type: 'event', event })).then((off) => {
      if (disposed) off();
      else unlisten = off;
    });
    ready.current = subscribed.catch(() => undefined);
    return () => {
      disposed = true;
      unlisten?.();
    };
  }, [ipc]);

  const view = activeView(state);
  const turnId = state.activeTurnId;

  const start = useCallback(
    async (request: TurnStartRequest): Promise<boolean> => {
      setStartError(null);
      setStarting(true);
      try {
        await ready.current;
        const started = await ipc.startTurn(request);
        dispatch({ type: 'begin', turnId: started.turn_id });
        return true;
      } catch (error) {
        setStartError(describeWorkspaceError(error));
        return false;
      } finally {
        setStarting(false);
      }
    },
    [ipc],
  );

  const cancel = useCallback(async () => {
    if (turnId === null) return;
    try {
      await ipc.cancelTurn(turnId);
    } catch (error) {
      setStartError(describeWorkspaceError(error));
    }
  }, [ipc, turnId]);

  const answer = useCallback(
    async (requestId: string, decision: ApprovalDecision) => {
      if (turnId === null) return;
      // Drop it from the queue first: the dialog must move on even if the host is slow to ack.
      dispatch({ type: 'approval-resolved', turnId, requestId });
      try {
        await ipc.respondApproval(requestId, decision);
      } catch (error) {
        setStartError(describeWorkspaceError(error));
      }
    },
    [ipc, turnId],
  );

  const busy = starting || (turnId !== null && view.done === undefined);

  return { view, turnId, busy, startError, start, cancel, answer };
}
