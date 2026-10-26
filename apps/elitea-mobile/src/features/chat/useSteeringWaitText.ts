import { useEffect, useState } from 'react';

import { steeringWaitSeconds } from '@elitea/client-core/stream';

/** The time between two updates of the countdown. */
const TICK_MS = 250;

/**
 * The composer hint while a rate-limited steering message waits for its
 * next try (client contract 1.26); null when the wait is over or none waits.
 */
export function steeringWaitText(retryAt: number | null | undefined, now: number): string | null {
  if (retryAt == null || retryAt <= now) return null;
  return `Too many messages at once. You can send again in ${steeringWaitSeconds(retryAt - now)} s. Your message goes again then.`;
}

/** `steeringWaitText` that counts down: it updates each tick until the wait is over. */
export function useSteeringWaitText(retryAt: number | null | undefined): string | null {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (retryAt == null) return undefined;
    const tick = () => {
      const at = Date.now();
      setNow(at);
      if (at >= retryAt) clearInterval(timer);
    };
    // The first tick comes at once, so that the hint does not use an old time.
    const first = setTimeout(tick, 0);
    const timer = setInterval(tick, TICK_MS);
    return () => {
      clearTimeout(first);
      clearInterval(timer);
    };
  }, [retryAt]);
  return steeringWaitText(retryAt, now);
}
