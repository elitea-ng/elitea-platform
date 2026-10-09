/**
 * One live agent turn: subscribes to `agent://event`, folds it with
 * `turnReducer`, and exposes start / cancel / answer-approval.
 *
 * The subscription is made BEFORE `agent_turn_start` is invoked (the host
 * emits as soon as it has a turn), and `start` waits for it to be live.
 */
import { useCallback, useEffect, useReducer, useRef, useState } from 'react';

import type { ApprovalDecision, TurnStartRequest, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';

import { activeView, initialTurnsState, isTerminalPhase, turnReducer, type TurnView } from './turnReducer';

export interface WorkspaceTurn {
  view: TurnView;
  turnId: string | null;
  /** True from `start` until the turn reaches a terminal phase. */
  busy: boolean;
  startError: string | null;
  start(request: TurnStartRequest): Promise<void>;
  cancel(): Promise<void>;
  answer(requestId: string, decision: ApprovalDecision): Promise<void>;
}

function describe(error: unknown): string {
  if (typeof error === 'string') return error;
  return error instanceof Error ? error.message : 'Something went wrong.';
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
    async (request: TurnStartRequest) => {
      setStartError(null);
      setStarting(true);
      try {
        await ready.current;
        const started = await ipc.startTurn(request);
        dispatch({ type: 'begin', turnId: started.turn_id });
      } catch (error) {
        setStartError(describe(error));
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
      setStartError(describe(error));
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
        setStartError(describe(error));
      }
    },
    [ipc, turnId],
  );

  const busy = starting || (turnId !== null && !isTerminalPhase(view.phase) && view.done === undefined && view.error === undefined);

  return { view, turnId, busy, startError, start, cancel, answer };
}
