/**
 * The notices of the steering hook (`useChatSteering`): the countdown of a
 * rate-limited send, and the text of a refused send.
 */
import { useCallback, useEffect, useState } from 'react';

import { STEERING_ERROR, steeringWaitSeconds } from '@elitea/client-core/stream';

import { t } from '@/shared/i18n';

/** The time between two updates of the countdown. */
const COUNTDOWN_TICK_MS = 250;

export const sleep = (ms: number) => new Promise<void>((resolve) => { setTimeout(resolve, ms); });

/**
 * The countdown of a rate-limited send. `start(ms)` shows "You can send
 * again in N s" until the wait ends; the text updates each tick. A later
 * wait that ends later replaces the current one.
 */
export function useRetryCountdown(): { readonly text: string | null; readonly start: (ms: number) => void; readonly clear: () => void } {
  const [until, setUntil] = useState<number | null>(null);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (until === null) return undefined;
    const timer = setInterval(() => {
      const at = Date.now();
      setNow(at);
      if (at >= until) setUntil(null);
    }, COUNTDOWN_TICK_MS);
    return () => { clearInterval(timer); };
  }, [until]);
  const start = useCallback((ms: number) => {
    const at = Date.now();
    setNow(at);
    setUntil((current) => Math.max(current ?? 0, at + ms));
  }, []);
  const clear = useCallback(() => { setUntil(null); }, []);
  const left = until === null ? 0 : until - now;
  const text = left > 0
    ? t('chatMessages.steering.rateLimited', 'Too many messages at once. You can send again in {{seconds}} s. Your message goes again then.', { seconds: String(steeringWaitSeconds(left)) })
    : null;
  return { text, start, clear };
}

export function failureNotice(code: string | undefined): string {
  if (code === STEERING_ERROR.queueFull) {
    return t('chatMessages.steering.queueFull', 'This turn has too many waiting messages. Your message is sent after the turn.');
  }
  return t('chatMessages.steering.failed', 'Your message could not reach the running turn. It is sent after the turn.');
}
