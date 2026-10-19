/**
 * A terminal PAUSE ends observation after the pause batch, not at its first card.
 *
 * Main reveals a paused run's pause frames only once the run's result is
 * projected (`replay_events.go`, the `visible` column), then streams them
 * back to back: one progress frame per card, then the aggregate. The frames
 * carry no marker that tells the last from the first, so detaching on the
 * first dropped every later card of a parallel pause from the live view (they
 * came back only on reload). Observation therefore stays open for a short,
 * bounded quiet window after a terminal pause and each further pause frame
 * extends it, up to a hard cap. Any other terminal frame detaches at once.
 *
 * Interim workaround for FU-227: once the Worker stamps
 * `response_metadata.pause_progress: true` on per-card progress frames,
 * `isTerminalPauseFrame` already treats those as non-terminal, only the
 * aggregate arms this window, and the window becomes dead code to delete.
 * A pause frame arriving after the window closed is not shown live; the
 * stored pending set (Main persists every card) shows it on reload, and no
 * decision is lost because the set is only submitted once it is complete.
 */
import { useCallback, useEffect, useMemo, useRef } from 'react';

/** Quiet time after the last pause frame before observation ends (FU-227 interim). */
export const PAUSE_SETTLE_QUIET_MS = 750;
/** Upper bound from the first terminal pause frame, however many follow (FU-227 interim). */
export const PAUSE_SETTLE_MAX_MS = 3_000;

const PAUSE_FRAME_TYPES: ReadonlySet<string> = new Set(['agent_hitl_interrupt', 'mcp_authorization_required']);

/** The frame types a paused run ends with. */
export function isPauseFrameType(type: string | undefined): boolean {
  return type !== undefined && PAUSE_FRAME_TYPES.has(type);
}

export interface PauseSettle {
  /** A terminal pause frame arrived: (re)arm the bounded quiet window; `settle` runs when it closes. */
  readonly defer: (settle: () => void) => void;
  /** Observation ended some other way: drop any armed window. */
  readonly cancel: () => void;
}

export function usePauseSettle(now: () => number = Date.now): PauseSettle {
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const firstAt = useRef<number | undefined>(undefined);

  const cancel = useCallback(() => {
    if (timer.current !== undefined) clearTimeout(timer.current);
    timer.current = undefined;
    firstAt.current = undefined;
  }, []);

  const defer = useCallback((settle: () => void) => {
    const started = firstAt.current ?? now();
    const remaining = PAUSE_SETTLE_MAX_MS - (now() - started);
    if (timer.current !== undefined) clearTimeout(timer.current);
    if (remaining <= 0) {
      cancel();
      settle();
      return;
    }
    firstAt.current = started;
    timer.current = setTimeout(() => {
      cancel();
      settle();
    }, Math.min(PAUSE_SETTLE_QUIET_MS, remaining));
  }, [cancel, now]);

  useEffect(() => cancel, [cancel]);
  return useMemo(() => ({ defer, cancel }), [defer, cancel]);
}
