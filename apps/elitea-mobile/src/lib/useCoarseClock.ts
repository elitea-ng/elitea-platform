import { useEffect, useState } from 'react';

/** How often a coarse clock moves on: list ages ("5m") and the 24 h activity checks need no finer. */
export const COARSE_CLOCK_MS = 60_000;

/**
 * The time, moved on every `intervalMs` while `active` (and at once when it becomes active
 * again), so what a mounted list derives from it (a row's age, a finished turn's check that
 * expires after a day, its date section) is worked out again without a refetch.
 */
export function useCoarseClock(active = true, intervalMs = COARSE_CLOCK_MS): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return undefined;
    const tick = () => setNow(Date.now());
    // Catch up at once (it stood still while inactive), then every interval.
    const first = setTimeout(tick, 0);
    const timer = setInterval(tick, intervalMs);
    return () => {
      clearTimeout(first);
      clearInterval(timer);
    };
  }, [active, intervalMs]);
  return now;
}
