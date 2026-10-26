/**
 * Taking a thread back, as the dialog walks it:
 *
 *   reclaim → (waiting for the running cloud turn) → preflight → review
 *   → apply → done (→ undo)
 *
 * `handoff_reclaim` answers `RECLAIMING` at once for an idle thread, or
 * `reclaim_pending` while a cloud turn runs: the state then becomes
 * `RECLAIMING` when that turn ends (or after 60 s, when it is cancelled), and
 * this polls `handoff_status` until it does. The preflight writes nothing to
 * the folder; only `apply` does, and it keeps a checkpoint `undo` restores.
 * The first preflight takes back every cloud turn; `pickTurn` previews the
 * take back up to an earlier one instead, and `apply` sends that turn along.
 * With `rest`, it takes back the remaining turns after an earlier take back
 * up to turn k (`handoff_reclaim` with `rest`; the preflight answers
 * `from_turn: k`), all of them together.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import type { ApplyResult, HandoffIpc, Preflight, TakeBackMode } from '@/shared/desktop/handoffIpc';
import { toHandoffIpcError } from '@/shared/desktop/handoffIpc';

export const RECLAIM_POLL_MS = 2000;

type TakeBackStep =
  | { kind: 'idle' }
  | { kind: 'reclaiming' }
  /** A cloud turn is still running; the take back starts when it ends. */
  | { kind: 'waiting' }
  | { kind: 'preflighting' }
  | { kind: 'review'; preflight: Preflight }
  | { kind: 'applying'; preflight: Preflight; mode: TakeBackMode }
  | { kind: 'done'; preflight: Preflight; result: ApplyResult; undone: boolean }
  /** The thread was released elsewhere (erased, or it expired): nothing to take back. */
  | { kind: 'gone' };

export interface TakeBack {
  step: TakeBackStep;
  error: unknown;
  undoing: boolean;
  start: () => void;
  /** Preview the take back up to cloud turn `turn` (1-based) instead. */
  pickTurn: (turn: number) => void;
  apply: (mode: TakeBackMode) => void;
  undo: () => void;
}

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

export function useTakeBack(ipc: HandoffIpc, conversationUuid: string, onSettled: () => void, rest = false): TakeBack {
  const [step, setStep] = useState<TakeBackStep>({ kind: 'idle' });
  const [error, setError] = useState<unknown>(null);
  const [undoing, setUndoing] = useState(false);
  // One run at a time; a closed dialog (unmount) stops a poll.
  const run = useRef(0);
  const settled = useRef(onSettled);
  useEffect(() => {
    settled.current = onSettled;
  });
  useEffect(() => {
    const current = run;
    return () => {
      current.current += 1;
    };
  }, []);

  const start = useCallback(() => {
    const mine = (run.current += 1);
    const live = (): boolean => run.current === mine;
    const fail = (cause: unknown): void => {
      if (!live()) return;
      setError(toHandoffIpcError(cause));
      setStep({ kind: 'idle' });
    };
    setError(null);
    setStep({ kind: 'reclaiming' });
    void (async () => {
      try {
        const answer = await ipc.reclaim(conversationUuid, rest ? { rest: true } : undefined);
        if (!live()) return;
        if (answer.state !== 'RECLAIMING') {
          setStep({ kind: 'waiting' });
          for (;;) {
            await sleep(RECLAIM_POLL_MS);
            if (!live()) return;
            const status = await ipc.status(conversationUuid);
            if (!live()) return;
            if (status.state === 'LOCAL') {
              setStep({ kind: 'gone' });
              settled.current();
              return;
            }
            if (status.state === 'RECLAIMING') break;
          }
        }
        setStep({ kind: 'preflighting' });
        const preflight = await ipc.preflight(conversationUuid);
        if (live()) setStep({ kind: 'review', preflight });
      } catch (cause) {
        fail(cause);
      }
    })();
  }, [ipc, conversationUuid, rest]);

  const pickTurn = useCallback(
    (turn: number) => {
      if (step.kind !== 'review' || turn === step.preflight.up_to_turn || (step.preflight.from_turn ?? 0) > 0) return;
      const previous = step.preflight;
      const mine = (run.current += 1);
      setError(null);
      setStep({ kind: 'preflighting' });
      ipc.preflight(conversationUuid, turn).then(
        (preflight) => {
          if (run.current === mine) setStep({ kind: 'review', preflight });
        },
        (cause: unknown) => {
          if (run.current !== mine) return;
          setError(toHandoffIpcError(cause));
          setStep({ kind: 'review', preflight: previous });
        },
      );
    },
    [ipc, conversationUuid, step],
  );

  const apply = useCallback(
    (mode: TakeBackMode) => {
      if (step.kind !== 'review') return;
      const { preflight } = step;
      const mine = (run.current += 1);
      setError(null);
      setStep({ kind: 'applying', preflight, mode });
      // An earlier turn than the latest is sent along, so the host checks it is the preview's.
      const turn = preflight.up_to_turn < preflight.cloud_turns ? preflight.up_to_turn : undefined;
      ipc.apply(preflight.preflight_id, mode, turn).then(
        (result) => {
          if (run.current !== mine) return;
          setStep({ kind: 'done', preflight, result, undone: false });
          settled.current();
        },
        (cause: unknown) => {
          if (run.current !== mine) return;
          setError(toHandoffIpcError(cause));
          setStep({ kind: 'review', preflight });
        },
      );
    },
    [ipc, step],
  );

  const undo = useCallback(() => {
    if (step.kind !== 'done' || step.result.checkpoint_id === null || step.undone) return;
    const done = step;
    setError(null);
    setUndoing(true);
    ipc.undo(done.result.checkpoint_id as string).then(
      () => {
        setUndoing(false);
        setStep({ ...done, undone: true });
      },
      (cause: unknown) => {
        setUndoing(false);
        setError(toHandoffIpcError(cause));
      },
    );
  }, [ipc, step]);

  return { step, error, undoing, start, pickTurn, apply, undo };
}
