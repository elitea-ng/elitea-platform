/**
 * One live agent turn: subscribes to `agent://event`, folds it with
 * `turnReducer`, and exposes start / cancel / answer-approval.
 *
 * The subscription is made BEFORE `agent_turn_start` is invoked (the host
 * emits as soon as it has a turn), and `start` waits for it to be live.
 *
 * Events are not replayed, so a lost `done` would keep the composer busy for
 * good. While a turn is unfinished the hook asks the host where it is
 * (`agent_turn_status`) when the window comes back into view, every
 * `RESYNC_MS`, and when a cancel is answered with a code that says the turn
 * is past running; a `done` it learns of that way is folded in like the event.
 */
import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from 'react';

import { toWorkspaceIpcError, type ApprovalDecision, type TurnDonePayload, type TurnStartRequest, type WorkspaceIpc } from '@/shared/desktop/workspaceIpc';

import { describeWorkspaceError } from './describeWorkspaceError';
import { activeView, earlierViews, initialTurnsState, turnReducer, type TurnView } from './turnReducer';

export interface WorkspaceTurn {
  view: TurnView;
  turnId: string | null;
  /** The turns started here before the active one, oldest first (cleared with the transcript). */
  earlier: { turnId: string; view: TurnView }[];
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
  /** Forget the shown turn (the transcript only; nothing on the host changes). Ignored while a turn runs. */
  clear(): void;
}

const RESYNC_MS = 15_000;
/** Past any seq the host sends: a `done` learnt from the status folds in last. */
const RESYNC_SEQ = Number.MAX_SAFE_INTEGER;
/** The host no longer keeps the turn (an app restart forgets them): it is over, nothing to show. */
const FORGOTTEN: TurnDonePayload = { committed: false, conversation_id: '', message_ids: [], changed_files: 0 };
/** Cancel answers after which the turn is not running any more. */
const PAST_RUNNING = new Set(['turn_not_cancellable', 'turn_unknown', 'turn_expired']);

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
  const earlier = useMemo(() => earlierViews(state), [state]);

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

  const unfinished = turnId !== null && view.done === undefined;

  const resync = useCallback(async () => {
    if (turnId === null) return;
    let done: TurnDonePayload | null;
    try {
      done = (await ipc.turnStatus(turnId)).done;
    } catch (error) {
      const { code } = toWorkspaceIpcError(error);
      done = code === 'turn_unknown' || code === 'turn_expired' ? FORGOTTEN : null;
    }
    if (done !== null) dispatch({ type: 'event', event: { turn_id: turnId, seq: RESYNC_SEQ, kind: 'done', payload: done } });
  }, [ipc, turnId]);

  useEffect(() => {
    if (!unfinished) return undefined;
    const onVisible = (): void => {
      if (document.visibilityState === 'visible') void resync();
    };
    window.addEventListener('focus', onVisible);
    document.addEventListener('visibilitychange', onVisible);
    const timer = window.setInterval(onVisible, RESYNC_MS);
    return () => {
      window.removeEventListener('focus', onVisible);
      document.removeEventListener('visibilitychange', onVisible);
      window.clearInterval(timer);
    };
  }, [unfinished, resync]);

  const cancel = useCallback(async () => {
    if (turnId === null) return;
    try {
      await ipc.cancelTurn(turnId);
    } catch (error) {
      setStartError(describeWorkspaceError(error));
      if (PAST_RUNNING.has(toWorkspaceIpcError(error).code)) await resync();
    }
  }, [ipc, turnId, resync]);

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

  const busy = starting || unfinished;

  const clear = useCallback(() => {
    if (busy) return;
    setStartError(null);
    dispatch({ type: 'reset' });
  }, [busy]);

  return { view, turnId, earlier, busy, startError, start, cancel, answer, clear };
}
